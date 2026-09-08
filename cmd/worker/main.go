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
	_ = godotenv.Load()

	cfg := config.Load()
	log := logger.Default(cfg.Debug)

	if err := run(cfg, log); err != nil {
		log.Error(context.Background(), "fatal", "err", err.Error())
		os.Exit(1)
	}
}

func run(cfg config.Config, log *logger.Logger) error {
	if cfg.AMQPURL == "" {
		return errors.New("AMQP_URL is required")
	}
	if cfg.APIURL == "" {
		return errors.New("API_URL is required")
	}
	if cfg.WorkerToken == "" {
		return errors.New("WORKER_TOKEN is required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	connectCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	repo, err := store.Connect(connectCtx, cfg.APIURL, cfg.WorkerToken, cfg.APITimeout)
	if err != nil {
		return err
	}

	routingClient, tilesetAt := newRoutingClient(ctx, cfg, log)

	cache := isochrone.EgressCache{Store: repo, TilesetAt: tilesetAt}

	handler := worker.NewHandler(repo, routingClient, cfg.Limits, cache, log)
	consumer := worker.NewConsumer(cfg.AMQPURL, cfg.RoutingQueue, cfg.DeadLetterQueue, cfg.MaxRetries, handler, log)
	defer consumer.Close()

	return consumer.Run(ctx)
}

func newRoutingClient(ctx context.Context, cfg config.Config, log *logger.Logger) (valhalla.Client, time.Time) {
	if cfg.ValhallaURL == "" {
		log.Warn(ctx, "routing: VALHALLA_URL is unset — serving SYNTHETIC isochrones, not real ones")
		return valhalla.NewSyntheticClient(), time.Time{}
	}

	client := valhalla.NewHTTPClient(cfg.ValhallaURL).WithTimeout(cfg.ValhallaTimeout)
	log.Info(ctx, "routing: using Valhalla",
		"valhalla_url", cfg.ValhallaURL,
		"max_iso_contour_mins", cfg.Limits.MaxIsoContourMins,
		"max_matrix_pairs", cfg.Limits.MaxMatrixPairs,
		"max_concurrency", cfg.Limits.MaxConcurrency,

		"service_area_zone", valhalla.ServiceAreaZone().String())
	return client, reportTileset(ctx, client, log)
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
