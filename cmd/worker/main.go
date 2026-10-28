package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"golang.org/x/sync/errgroup"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/config"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/health"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/isochrone"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/metrics"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/worker"
)

type options struct {
	check   bool
	envFile string
}

func parseArgs(args []string) (options, error) {
	fs := flag.NewFlagSet("sparks-effect-routing-worker", flag.ContinueOnError)
	opts := options{}
	fs.BoolVar(&opts.check, "check", false, "validate config and exit without connecting to AMQP, the API, or Valhalla")
	fs.StringVar(&opts.envFile, "env-file", "", "dotenv file to load (default: .env if present)")
	if err := fs.Parse(args); err != nil {
		return options{}, err
	}
	return opts, nil
}

func loadDotenv(path string) error {
	if path == "" {
		_ = godotenv.Load()
		return nil
	}
	if err := godotenv.Overload(path); err != nil {
		return fmt.Errorf("could not load %s: %w", path, err)
	}
	return nil
}

func main() {
	os.Exit(start())
}

// start returns the exit code instead of calling os.Exit so the deferred
// OTLP flush runs: os.Exit skips defers, and the lines worth exporting most
// are the fatal ones written just before exiting.
func start() int {
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		return 2
	}
	if err := loadDotenv(opts.envFile); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	cfg := config.Load()
	ctx := context.Background()

	// -check promises no network, and the exporter would open a connection
	// on its first flush, so it stays off there.
	otlp, flushLogs, err := logger.OTLP(ctx, cfg.ExportLogs && !opts.check)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() {
		flushCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = flushLogs(flushCtx)
	}()
	log := logger.Default(cfg.Debug, otlp)
	switch {
	case otlp != nil:
		log.Info(ctx, "logger: exporting logs over OTLP")
	case !cfg.ExportLogs:
		log.Info(ctx, "logger: OTEL_EXPORTER_OTLP_ENDPOINT not set; logs stay on stderr")
	}

	if err := inspectConfig(cfg, log); err != nil {
		log.Error(ctx, "fatal", "err", err.Error())
		return 1
	}
	if opts.check {
		log.Info(ctx, "config: ok",
			"valhalla_synthetic", cfg.ValhallaSynthetic,
			"routing_queue", cfg.RoutingQueue,
			"metrics_addr", cfg.MetricsAddr,
			"stuck_after", cfg.StuckAfter.String())
		return 0
	}

	if err := run(cfg, log); err != nil {
		log.Error(ctx, "fatal", "err", err.Error())
		return 1
	}
	return 0
}

// inspectConfig asserts the process env is one this binary can honour. It
// performs no network I/O, so the same rules apply locally, in tests, and in
// cluster: fill AMQP_URL, API_URL, WORKER_TOKEN, and either VALHALLA_URL or
// VALHALLA_SYNTHETIC=true.
func inspectConfig(cfg config.Config, log *logger.Logger) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	ctx := context.Background()
	for _, name := range config.UnrecognizedEnv() {
		log.Warn(ctx, "config: unrecognised variable", "name", name)
	}
	return nil
}

// The API reads two minutes without contact as a worker gone, so a 30 s beat
// survives three misses before a live worker is reported offline.
const heartbeatEvery = 30 * time.Second

func run(cfg config.Config, log *logger.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	metrics.Process.MarkStarted(time.Now())

	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	repo, err := store.Connect(connectCtx, cfg.APIURL, cfg.WorkerToken, cfg.APITimeout)
	if err != nil {
		return err
	}

	routingClient, err := newRoutingClient(ctx, cfg, log)
	if err != nil {
		return err
	}

	routingClient = metrics.ObserveValhalla(routingClient)
	stamp := isochrone.NewTilesetStamp(time.Time{})
	stamp.Refresh(ctx, routingClient, log)
	cache := isochrone.EgressCache{Store: repo, Stamp: stamp, Metrics: metrics.Process}

	handler := worker.NewHandler(repo, routingClient, cfg.Limits, cache, log, metrics.Process)
	consumer := worker.NewConsumer(cfg.AMQPURL, cfg.RoutingQueue, cfg.DeadLetterQueue, cfg.MaxRetries, handler, log, metrics.Process)
	defer consumer.Close()

	monitor := health.New(consumer, cfg.StuckAfter, time.Now)
	consumer.TrackJobs(monitor)
	metrics.Process.ObserveJobInProgress(monitor.InProgress)

	log.Info(ctx, "routing: serving metrics and health", "addr", cfg.MetricsAddr, "stuck_after", cfg.StuckAfter.String())

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return metrics.Serve(ctx, cfg.MetricsAddr, serverMux(monitor))
	})
	g.Go(func() error {
		return consumer.Run(ctx)
	})
	g.Go(func() error {
		return repo.Heartbeat(ctx, heartbeatEvery, func(err error) {
			log.Warn(ctx, "store: heartbeat failed", "err", err.Error())
		})
	})
	return g.Wait()
}

func serverMux(healthz http.Handler) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.Handle("/healthz", healthz)
	return mux
}

func newRoutingClient(ctx context.Context, cfg config.Config, log *logger.Logger) (valhalla.Client, error) {
	client, err := valhalla.NewClient(cfg.ValhallaClientConfig())
	if err != nil {
		return nil, err
	}
	if cfg.ValhallaSynthetic {
		log.Warn(ctx, "routing: VALHALLA_SYNTHETIC=true — serving SYNTHETIC isochrones, not real ones")
		return client, nil
	}

	log.Info(ctx, "routing: using Valhalla",
		"valhalla_url", cfg.ValhallaURL,
		"max_iso_contour_mins", cfg.Limits.MaxIsoContourMins,
		"max_matrix_pairs", cfg.Limits.MaxMatrixPairs,
		"max_concurrency", cfg.Limits.MaxConcurrency,

		"service_area_zone", valhalla.ServiceAreaZone().String())
	return client, nil
}
