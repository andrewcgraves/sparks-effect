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
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/isochrone"
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

	routingClient, tilesetAt := newRoutingClient(cfg, log)

	// One repo, two roles: it is where results are written and where egress
	// polygons are reused from. The tileset stamp is read once, here, because
	// it describes the server rather than any one request.
	cache := isochrone.EgressCache{Store: repo, TilesetAt: tilesetAt}

	handler := worker.NewHandler(repo, routingClient, cfg.Limits, cache, log)
	consumer := worker.NewConsumer(cfg.AMQPURL, cfg.RoutingQueue, cfg.DeadLetterQueue, cfg.MaxRetries, handler, log)
	defer consumer.Close()

	return consumer.Run(ctx)
}

// newRoutingClient picks the routing backend, says loudly which one it picked,
// and reports the tileset it is serving.
//
// Falling back to synthetic geometry is the right default for local
// development and a serious problem in production, and the two are told apart
// only by an environment variable. So the fallback is logged at Printf rather
// than Debugf: an operator reading the first ten lines of a pod's logs should
// not be able to miss that this worker is drawing circles.
//
// The synthetic client returns a zero tileset time because it has never seen a
// tile. That is stored as NULL on the cache rows it produces, which is exactly
// what it means.
func newRoutingClient(cfg config.Config, log *logger.Logger) (valhalla.Client, time.Time) {
	if cfg.ValhallaURL == "" {
		log.Printf("routing: VALHALLA_URL is unset — serving SYNTHETIC isochrones, not real ones")
		return valhalla.NewSyntheticClient(), time.Time{}
	}

	client := valhalla.NewHTTPClient(cfg.ValhallaURL).WithTimeout(cfg.ValhallaTimeout)
	log.Printf("routing: using Valhalla at %s (contour ceiling %d min, matrix pairs %d, concurrency %d)",
		cfg.ValhallaURL, cfg.Limits.MaxIsoContourMins, cfg.Limits.MaxMatrixPairs, cfg.Limits.MaxConcurrency)
	return client, reportTileset(client, log)
}

// reportTileset logs the tiles the server is serving, once, at startup, and
// returns the stamp for the cache to record beside every polygon it stores.
//
// Reading it once is deliberate. It is a property of the server, not of a
// request, and it is recorded as data rather than keyed on — so a chain that
// re-read it per job would pay a call per message to learn a number nothing
// looks up. The cost is that a tile rebuild under a running worker keeps
// stamping the old timestamp until the pod restarts, which is the accepted risk
// this column exists to make diagnosable rather than one it claims to remove.
//
// It is a diagnostic rather than a health check, and it deliberately does not
// fail startup: a routing server that is not up yet is exactly the transient
// condition the retry path exists for, and refusing to boot would turn a
// recoverable situation into a crash loop. A zero time is the honest answer to
// "which tiles?" when the server could not say.
//
// That honesty has a cost worth stating plainly: a worker that booted while
// Valhalla was still coming up stamps NULL on every row it writes for the rest
// of its life, and the diagnostic is simply absent for that pod. Restarting it
// once Valhalla is up is the fix. Re-reading per chain would close the gap and
// is not worth a call per message for a field nothing looks up — but if these
// NULLs ever turn up in an investigation, this is why.
func reportTileset(client valhalla.Client, log *logger.Logger) time.Time {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	status, err := client.Status(ctx)
	if err != nil {
		log.Printf("routing: could not read Valhalla /status: %v", err)
		return time.Time{}
	}
	if status.TilesetAt.IsZero() {
		log.Printf("routing: Valhalla %s reports no tileset timestamp", status.Version)
		return time.Time{}
	}
	log.Printf("routing: Valhalla %s, tileset built %s", status.Version, status.TilesetAt.Format(time.RFC3339))
	return status.TilesetAt
}
