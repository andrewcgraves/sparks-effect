package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"golang.org/x/sync/errgroup"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/config"
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
	opts, err := parseArgs(os.Args[1:])
	if err != nil {
		os.Exit(2)
	}
	if err := loadDotenv(opts.envFile); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	cfg := config.Load()
	log := logger.Default(cfg.Debug)
	ctx := context.Background()

	if err := inspectConfig(cfg, log); err != nil {
		log.Error(ctx, "fatal", "err", err.Error())
		os.Exit(1)
	}
	if opts.check {
		log.Info(ctx, "config: ok",
			"valhalla_synthetic", cfg.ValhallaSynthetic,
			"routing_queue", cfg.RoutingQueue,
			"metrics_addr", cfg.MetricsAddr)
		return
	}

	if err := run(cfg, log); err != nil {
		log.Error(ctx, "fatal", "err", err.Error())
		os.Exit(1)
	}
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

func run(cfg config.Config, log *logger.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	repo, err := store.Connect(connectCtx, cfg.APIURL, cfg.WorkerToken, cfg.APITimeout)
	if err != nil {
		return err
	}

	routingClient, tilesetAt, err := newRoutingClient(ctx, cfg, log)
	if err != nil {
		return err
	}

	routingClient = metrics.ObserveValhalla(routingClient)
	cache := isochrone.EgressCache{Store: repo, TilesetAt: tilesetAt, Metrics: metrics.Process}

	handler := worker.NewHandler(repo, routingClient, cfg.Limits, cache, log, metrics.Process)
	consumer := worker.NewConsumer(cfg.AMQPURL, cfg.RoutingQueue, cfg.DeadLetterQueue, cfg.MaxRetries, handler, log, metrics.Process)
	defer consumer.Close()

	log.Info(ctx, "routing: serving metrics", "addr", cfg.MetricsAddr)

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return metrics.Serve(ctx, cfg.MetricsAddr)
	})
	g.Go(func() error {
		return consumer.Run(ctx)
	})
	return g.Wait()
}

func newRoutingClient(ctx context.Context, cfg config.Config, log *logger.Logger) (valhalla.Client, time.Time, error) {
	client, err := valhalla.NewClient(cfg.ValhallaClientConfig())
	if err != nil {
		return nil, time.Time{}, err
	}
	if cfg.ValhallaSynthetic {
		log.Warn(ctx, "routing: VALHALLA_SYNTHETIC=true — serving SYNTHETIC isochrones, not real ones")
		return client, time.Time{}, nil
	}

	log.Info(ctx, "routing: using Valhalla",
		"valhalla_url", cfg.ValhallaURL,
		"max_iso_contour_mins", cfg.Limits.MaxIsoContourMins,
		"max_matrix_pairs", cfg.Limits.MaxMatrixPairs,
		"max_concurrency", cfg.Limits.MaxConcurrency,

		"service_area_zone", valhalla.ServiceAreaZone().String())
	return client, reportTileset(ctx, client, log), nil
}

func reportTileset(ctx context.Context, client valhalla.Client, log *logger.Logger) time.Time {
	statusCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	status, err := client.Status(statusCtx)
	if err != nil {
		log.Warn(ctx, "routing: could not read Valhalla /status", "err", err.Error())
		return time.Time{}
	}
	if status.TilesetAt.IsZero() {
		log.Warn(ctx, "routing: Valhalla reports no tileset timestamp", "valhalla_version", status.Version)
		return time.Time{}
	}
	log.Info(ctx, "routing: Valhalla tileset",
		"valhalla_version", status.Version,
		"tileset_at", status.TilesetAt.Format(time.RFC3339))
	return status.TilesetAt
}
