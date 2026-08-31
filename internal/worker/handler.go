// Package worker turns a queued message into a completed routing job.
//
// It is split in two on purpose. Handler owns what a job *is* — decode it,
// compute it, record it — and reports how its failure should be treated.
// Consumer owns the broker conversation: prefetch, acknowledgement, redelivery,
// dead-lettering. Keeping them apart is what lets the whole of the job
// lifecycle be tested without a broker, which is most of what there is to get
// wrong here.
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

// Outcome says what the broker should do with a delivery.
//
// The three-way split exists because the two failure cases need opposite
// handling, and confusing them is the failure mode that leaves a user watching
// a spinner forever. A permanent failure requeued loops against a server with
// room for one chain at a time; a transient failure abandoned fails a job that
// would have succeeded on the next attempt.
type Outcome int

const (
	// OutcomeSucceeded: the result is written. Ack.
	OutcomeSucceeded Outcome = iota
	// OutcomePermanent: the job cannot be completed, ever, and has been marked
	// failed. Ack — redelivering it would only reach the same conclusion.
	OutcomePermanent
	// OutcomeTransient: the job might succeed on another attempt. Requeue, up
	// to a bound; on exhaustion the consumer dead-letters it and calls Fail.
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

// Result is what one delivery came to.
//
// JobID is reported separately from the outcome because the consumer needs it
// to mark the job failed once retries are exhausted, and by then the message
// has already been decoded and discarded. It is empty only when the message was
// too malformed to name a job — the one case where nothing can be recorded
// anywhere, since there is no row to record it against.
//
// TraceID rides back out for the same reason: the trace lives in the message
// body, so only the handler has seen it, and the consumer's own logging about
// the delivery — requeued, dead-lettered, acked — would otherwise fall outside
// the trail it belongs to. Empty when the message carried no trace.
type Result struct {
	Outcome Outcome
	JobID   string
	TraceID string
	Err     error
}

// Handler computes one routing job per delivery.
type Handler struct {
	store   store.RoutingJobStore
	routing valhalla.Client
	limits  valhalla.Limits
	cache   isochrone.EgressCache
	log     *logger.Logger
}

// NewHandler builds the job handler.
//
// The cache is passed in rather than derived from s, even though in production
// both are the same *store.Repo. The two are separate concerns — one is where
// the answer goes, the other is a saving on how it is arrived at — and a
// handler wired for one is legitimately run without the other, which a shared
// argument would make impossible to express. A zero EgressCache computes every
// polygon; see its documentation.
func NewHandler(
	s store.RoutingJobStore,
	routingClient valhalla.Client,
	limits valhalla.Limits,
	cache isochrone.EgressCache,
	log *logger.Logger,
) *Handler {
	return &Handler{store: s, routing: routingClient, limits: limits, cache: cache, log: log}
}

// Handle runs one message end to end: decode, mark running, chain, record.
//
// It never returns an error for the caller to interpret — the Result says what
// to do, and Err is there for the log. Every path that abandons a job has
// already marked it failed by the time it returns, so a caller that only acts
// on Outcome cannot leave a row stuck in running.
//
// The message's trace is applied to ctx and stamped onto the Result here, in
// one place, rather than at each of the returns below — a return path added
// later cannot forget to carry it.
func (h *Handler) Handle(ctx context.Context, body []byte) Result {
	msg, err := routing.Decode(body)

	// The body's trace wins over the delivery header the consumer tagged with,
	// and is in practice the only one there is: sparks-effect-api publishes
	// trace_id in the message and sets no x-trace-id header. Tagging before the
	// error is judged means a message rejected for its schema or its mode is
	// still logged under the trace of the request that queued it — the case
	// where following one job across both services matters most. A message
	// carrying no trace, or one too malformed to parse at all, leaves the
	// header's tag in place: WithTraceID ignores an empty id.
	ctx = logger.WithTraceID(ctx, msg.TraceID)

	res := h.handle(ctx, msg, err)
	res.TraceID = msg.TraceID
	return res
}

// handle is the body of Handle, taking the already-decoded message and whatever
// Decode made of it. msg is populated even when decodeErr is non-nil — Decode
// rejects a message it has read, not one it failed to read — so the failure
// paths below can still name the job.
func (h *Handler) handle(ctx context.Context, msg routing.Message, decodeErr error) Result {
	if decodeErr != nil {
		// A message with no job id cannot be recorded against anything, so the
		// log is the only place its death is visible. Everything else gets a
		// failed row, which is what stops the caller polling.
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
		// A row that is gone is gone: its compile job cascaded away, and there
		// is nothing left to answer. Anything else — the database being down,
		// a connection dropped — is worth another attempt.
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
		// Deliberately not marked failed: the job is going back on the queue,
		// and marking it failed now would tell a polling client it was over
		// while another attempt was still to come. The consumer marks it failed
		// if and when the retries run out.
		h.log.Warn(ctx, "routing: job failed transiently", "job_id", msg.RoutingJobID, "err", err.Error())
		return Result{Outcome: OutcomeTransient, JobID: msg.RoutingJobID, Err: err}
	}

	result, err := json.Marshal(resp)
	if err != nil {
		// A result that will not serialise will not serialise on retry either.
		return h.fail(ctx, msg.RoutingJobID, fmt.Errorf("routing: marshal result: %w", err))
	}

	if err := h.store.MarkSucceeded(ctx, msg.RoutingJobID, result); err != nil {
		if errors.Is(err, store.ErrJobNotFound) {
			h.log.Warn(ctx, "routing: job has no row to complete; discarding", "job_id", msg.RoutingJobID)
			return Result{Outcome: OutcomePermanent, JobID: msg.RoutingJobID, Err: err}
		}
		// The chain succeeded and only the write failed, so retrying is
		// worthwhile — and cheap to get wrong in the other direction, since a
		// job acked here would be lost with its answer computed.
		return Result{Outcome: OutcomeTransient, JobID: msg.RoutingJobID, Err: err}
	}

	h.log.Info(ctx, "routing: job succeeded",
		"job_id", msg.RoutingJobID,
		"features", len(resp.Features),
		"reachable_stations", len(resp.Metadata.ReachableStations))
	return Result{Outcome: OutcomeSucceeded, JobID: msg.RoutingJobID}
}

// Fail marks a job failed, for the consumer to call when retries are exhausted.
//
// It exists on Handler rather than being done by the consumer against the store
// directly so that there is one spelling of "this job is over" — the same
// wording, the same logging, whether the cause was permanent from the start or
// became permanent by running out of attempts.
func (h *Handler) Fail(ctx context.Context, jobID string, cause error) {
	h.fail(ctx, jobID, cause)
}

func (h *Handler) fail(ctx context.Context, jobID string, cause error) Result {
	h.log.Error(ctx, "routing: job failed", "job_id", jobID, "err", cause.Error())
	if err := h.store.MarkFailed(ctx, jobID, cause.Error()); err != nil {
		// The job cannot be marked failed, so a client will keep polling a row
		// that says running. Requeueing gives the write another chance; the
		// original cause is preserved in the log above either way.
		if !errors.Is(err, store.ErrJobNotFound) {
			h.log.Error(ctx, "routing: recording job failure failed", "job_id", jobID, "err", err.Error())
			return Result{Outcome: OutcomeTransient, JobID: jobID, Err: err}
		}
	}
	return Result{Outcome: OutcomePermanent, JobID: jobID, Err: cause}
}

// isPermanent reports whether an error will still be an error next time.
//
// The list is explicit rather than defaulting to permanent, because the
// asymmetry matters: an unrecognised error retried a bounded number of times
// costs a few seconds and then fails the job anyway, while an unrecognised
// error treated as permanent throws away work that might well have succeeded.
// Unknown therefore means transient.
func isPermanent(err error) bool {
	switch {
	case errors.Is(err, routing.ErrUnsupportedSchema),
		errors.Is(err, routing.ErrMalformedMessage),
		errors.Is(err, isochrone.ErrInvalidMode),
		// A request Valhalla refused: a contour past the ceiling, too many
		// matrix pairs, locations too far apart. The same bytes will be
		// refused again.
		errors.Is(err, isochrone.ErrRoutingRejected),
		errors.Is(err, store.ErrJobNotFound):
		return true
	default:
		return false
	}
}
