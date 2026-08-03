// Command worker consumes routing jobs and computes isochrones against
// Valhalla.
//
// It runs inside the home cluster, beside the Valhalla service. That placement
// is the reason it exists: Valhalla is ClusterIP-only with no ingress, so the
// API — deployed elsewhere — cannot reach it, and the queue is the only
// transport between them.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/config"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/worker"
)

func main() {
	// A missing .env is normal — in the cluster the environment is supplied by
	// the pod spec — so the error is deliberately ignored.
	_ = godotenv.Load()

	cfg := config.Load()
	log := logger.Default(cfg.Debug)

	if err := run(cfg, log); err != nil {
		log.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func run(cfg config.Config, log *logger.Logger) error {
	// Both of these are refused at startup rather than degraded around. A
	// worker with no queue has nothing to consume, and one with no database
	// has nowhere to record what it computed — in either case it would run
	// indefinitely doing nothing, which is worse than not starting.
	if cfg.AMQPURL == "" {
		return errors.New("AMQP_URL is required")
	}
	if cfg.DatabaseURL == "" {
		return errors.New("DATABASE_URL is required")
	}

	// Cancelled on SIGINT/SIGTERM. The consumer finishes settling the delivery
	// it is holding before returning, so a job in flight at shutdown is
	// redelivered rather than lost.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	repo, err := store.Connect(connectCtx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer repo.Close()

	routingClient := newRoutingClient(cfg, log)

	handler := worker.NewHandler(repo, routingClient, cfg.Limits, log)
	consumer := worker.NewConsumer(cfg.AMQPURL, cfg.RoutingQueue, cfg.DeadLetterQueue, cfg.MaxRetries, handler, log)
	defer consumer.Close()

	return consumer.Run(ctx)
}

// newRoutingClient picks the routing backend, and says loudly which one it
// picked.
//
// Falling back to synthetic geometry is the right default for local
// development and a serious problem in production, and the two are told apart
// only by an environment variable. So the fallback is logged at Printf rather
// than Debugf: an operator reading the first ten lines of a pod's logs should
// not be able to miss that this worker is drawing circles.
func newRoutingClient(cfg config.Config, log *logger.Logger) valhalla.Client {
	if cfg.ValhallaURL == "" {
		log.Printf("routing: VALHALLA_URL is unset — serving SYNTHETIC isochrones, not real ones")
		return valhalla.NewSyntheticClient()
	}

	client := valhalla.NewHTTPClient(cfg.ValhallaURL).WithTimeout(cfg.ValhallaTimeout)
	log.Printf("routing: using Valhalla at %s (contour ceiling %d min, matrix pairs %d, concurrency %d)",
		cfg.ValhallaURL, cfg.Limits.MaxIsoContourMins, cfg.Limits.MaxMatrixPairs, cfg.Limits.MaxConcurrency)
	reportTileset(client, log)
	return client
}

// reportTileset logs the tiles the server is serving, once, at startup.
//
// It is a diagnostic rather than a health check, and it deliberately does not
// fail startup: a routing server that is not up yet is exactly the transient
// condition the retry path exists for, and refusing to boot would turn a
// recoverable situation into a crash loop.
func reportTileset(client valhalla.Client, log *logger.Logger) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	status, err := client.Status(ctx)
	if err != nil {
		log.Printf("routing: could not read Valhalla /status: %v", err)
		return
	}
	if status.TilesetAt.IsZero() {
		log.Printf("routing: Valhalla %s reports no tileset timestamp", status.Version)
		return
	}
	log.Printf("routing: Valhalla %s, tileset built %s", status.Version, status.TilesetAt.Format(time.RFC3339))
}
