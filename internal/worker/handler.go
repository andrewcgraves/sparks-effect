package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/isochrone"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/routing"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

type Outcome int

const (
	OutcomeSucceeded Outcome = iota
	OutcomePermanent
	OutcomeTransient
)

func (o Outcome) String() string {
	switch o {
	case OutcomeSucceeded:
		return "succeeded"
	case OutcomePermanent:
		return "permanent"
	case OutcomeTransient:
		return "transient"
	default:
		return "unknown"
	}
}

type Result struct {
	Outcome Outcome
	JobID   string
	TraceID string
	Err     error
}

type Handler struct {
	store   store.RoutingJobStore
	routing valhalla.Client
	limits  valhalla.Limits
	cache   isochrone.EgressCache
	log     *logger.Logger
}

func NewHandler(
	s store.RoutingJobStore,
	routingClient valhalla.Client,
	limits valhalla.Limits,
	cache isochrone.EgressCache,
	log *logger.Logger,
) *Handler {
	return &Handler{store: s, routing: routingClient, limits: limits, cache: cache, log: log}
}

func (h *Handler) Handle(ctx context.Context, body []byte) Result {
	msg, err := routing.Decode(body)

	ctx = logger.WithTraceID(ctx, msg.TraceID)

	res := h.handle(ctx, msg, err)
	res.TraceID = msg.TraceID
	return res
}

func (h *Handler) handle(ctx context.Context, msg routing.Message, decodeErr error) Result {
	if decodeErr != nil {
		if msg.RoutingJobID == "" {
			h.log.Error(ctx, "routing: discarding unreadable message", "err", decodeErr.Error())
			return Result{Outcome: OutcomePermanent, Err: decodeErr}
		}
		return h.fail(ctx, msg.RoutingJobID, decodeErr)
	}

	h.log.Info(ctx, "routing: job received",
		"job_id", msg.RoutingJobID,
		"mode", msg.Mode,
		"budget_mins", msg.BudgetMins,
		"compile_job_id", msg.CompileJobID)

	if err := h.store.MarkRunning(ctx, msg.RoutingJobID); err != nil {
		if errors.Is(err, store.ErrJobNotFound) {
			h.log.Warn(ctx, "routing: job has no row; discarding", "job_id", msg.RoutingJobID)
			return Result{Outcome: OutcomePermanent, JobID: msg.RoutingJobID, Err: err}
		}
		return Result{Outcome: OutcomeTransient, JobID: msg.RoutingJobID, Err: err}
	}

	graph := transit.CompiledGraph{Graph: msg.Graph}
	chainer := isochrone.New(h.routing, graph, h.limits, h.cache, h.log)

	resp, err := chainer.Chain(ctx, isochrone.ChainRequest{
		Lat:          msg.Lat,
		Lng:          msg.Lng,
		BudgetMins:   msg.BudgetMins,
		Mode:         msg.Mode,
		CompileJobID: msg.CompileJobID,
	})
	if err != nil {
		if isPermanent(err) {
			return h.fail(ctx, msg.RoutingJobID, err)
		}

		h.log.Warn(ctx, "routing: job failed transiently", "job_id", msg.RoutingJobID, "err", err.Error())
		return Result{Outcome: OutcomeTransient, JobID: msg.RoutingJobID, Err: err}
	}

	result, err := json.Marshal(resp)
	if err != nil {
		return h.fail(ctx, msg.RoutingJobID, fmt.Errorf("routing: marshal result: %w", err))
	}

	if err := h.store.MarkSucceeded(ctx, msg.RoutingJobID, result); err != nil {
		if errors.Is(err, store.ErrJobNotFound) {
			h.log.Warn(ctx, "routing: job has no row to complete; discarding", "job_id", msg.RoutingJobID)
			return Result{Outcome: OutcomePermanent, JobID: msg.RoutingJobID, Err: err}
		}

		return Result{Outcome: OutcomeTransient, JobID: msg.RoutingJobID, Err: err}
	}

	h.log.Info(ctx, "routing: job succeeded",
		"job_id", msg.RoutingJobID,
		"features", len(resp.Features),
		"reachable_stations", len(resp.Metadata.ReachableStations))
	return Result{Outcome: OutcomeSucceeded, JobID: msg.RoutingJobID}
}

func (h *Handler) Fail(ctx context.Context, jobID string, cause error) {
	h.fail(ctx, jobID, cause)
}

func (h *Handler) fail(ctx context.Context, jobID string, cause error) Result {
	h.log.Error(ctx, "routing: job failed", "job_id", jobID, "err", cause.Error())
	if err := h.store.MarkFailed(ctx, jobID, cause.Error()); err != nil {
		if !errors.Is(err, store.ErrJobNotFound) {
			h.log.Error(ctx, "routing: recording job failure failed", "job_id", jobID, "err", err.Error())
			return Result{Outcome: OutcomeTransient, JobID: jobID, Err: err}
		}
	}
	return Result{Outcome: OutcomePermanent, JobID: jobID, Err: cause}
}

func isPermanent(err error) bool {
	switch {
	case errors.Is(err, routing.ErrUnsupportedSchema),
		errors.Is(err, routing.ErrMalformedMessage),
		errors.Is(err, isochrone.ErrInvalidMode),

		errors.Is(err, isochrone.ErrRoutingRejected),
		errors.Is(err, store.ErrJobNotFound):
		return true
	default:
		return false
	}
}
