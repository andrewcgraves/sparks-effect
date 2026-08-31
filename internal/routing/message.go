// Package routing is this worker's half of the contract with sparks-effect-api:
// the shape of a queued isochrone, and the rules for rejecting one it was not
// built to read.
//
// The API resolves everything before publishing — authentication, ownership,
// the target slug, and the stale-graph check — and the message it emits names
// one specific immutable compiled graph and carries it inline. That is why
// there is no repository access anywhere in this package and no notion of a
// scenario, a user, or a permission: by the time a Message exists, every
// question of that kind has already been answered on the other side of the
// boundary.
package routing

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
)

// SchemaVersion is the wire format this worker understands. It rides on every
// message so a shape the worker was not built for is rejected loudly rather
// than read as zero values out of fields that moved.
//
// It must match the constant of the same name in sparks-effect-api. Nothing
// checks that at compile time — the two repositories share no Go code — so the
// golden fixture asserted in message_test.go is what actually holds them
// together.
const SchemaVersion = 1

// ErrUnsupportedSchema reports a message whose schema_version this worker does
// not implement. It is permanent by nature: redelivering a message from the
// future does not bring the code that reads it any closer.
var ErrUnsupportedSchema = errors.New("routing: unsupported schema version")

// ErrMalformedMessage reports a message that decoded but does not describe a
// computable isochrone — no graph, an unknown mode, a budget of zero. Also
// permanent: the bytes on the queue will not improve on redelivery.
var ErrMalformedMessage = errors.New("routing: malformed message")

// Message is one isochrone to compute.
//
// This struct is a hand-maintained copy of the one in sparks-effect-api. The
// two repositories share no Go code, so nothing but testdata/message.golden.json
// — the same file, byte for byte, that the API asserts it produces — stops the
// copies drifting. Change a field here and that test fails until the fixture is
// updated on both sides deliberately. `make check-contract` fetches the API's
// copy of that fixture and diffs it, so a change made on only one side is
// caught by CI rather than by whoever next reads both files (SPA-227).
//
// Graph travels inline rather than by reference. A compiled graph is immutable
// once its compile job succeeds, so there is no staleness to worry about, and
// the sizes are ordinary: a few kilobytes for the seeded scenario, tens for a
// large authored one. Carrying it saves this worker a database it would
// otherwise need to read graphs out of.
type Message struct {
	SchemaVersion int    `json:"schema_version"`
	RoutingJobID  string `json:"routing_job_id"`
	// CompileJobID identifies the graph below: a compiled graph's identity is
	// the job that produced it. The result cache keys on it.
	CompileJobID string                `json:"compile_job_id"`
	Graph        *transit.TransitGraph `json:"graph"`
	Lat          float64               `json:"lat"`
	Lng          float64               `json:"lng"`
	BudgetMins   int                   `json:"budget_mins"`
	Mode         transit.TravelMode    `json:"mode"`
	// TraceID is the trace of the request that created this job, minted by the
	// API. Logging under it is what makes one isochrone followable across both
	// services instead of stopping at the queue, so the handler tags its
	// context with it (see internal/worker).
	//
	// It is the only trace source that actually fires in production: the
	// x-trace-id delivery header this worker also reads is set by nothing the
	// API publishes. Optional — a message published before the field existed,
	// or sent by hand, decodes with it empty and is processed unchanged, which
	// is why adding it needed no SchemaVersion bump.
	TraceID string `json:"trace_id"`
}

// Decode parses a message body and checks it describes work this worker can
// actually do.
//
// Every failure it returns is permanent, and that is the point of validating
// here rather than discovering the same problems part-way through a chain. A
// caller that gets an error from Decode can fail the job and ack immediately,
// without a Valhalla call, a database write, or a redelivery that would only
// arrive at the same conclusion more slowly.
//
// RoutingJobID is checked before anything else because it is the only field
// whose absence cannot be reported: with no job id there is no row to mark
// failed, so such a message can only be dropped.
func Decode(body []byte) (Message, error) {
	var msg Message
	if err := json.Unmarshal(body, &msg); err != nil {
		return Message{}, fmt.Errorf("%w: %v", ErrMalformedMessage, err)
	}

	if msg.SchemaVersion != SchemaVersion {
		return msg, fmt.Errorf("%w: got %d, want %d", ErrUnsupportedSchema, msg.SchemaVersion, SchemaVersion)
	}
	if msg.RoutingJobID == "" {
		return msg, fmt.Errorf("%w: no routing_job_id", ErrMalformedMessage)
	}
	if msg.CompileJobID == "" {
		return msg, fmt.Errorf("%w: no compile_job_id", ErrMalformedMessage)
	}
	if msg.Graph == nil {
		return msg, fmt.Errorf("%w: no graph", ErrMalformedMessage)
	}
	if !msg.Mode.Valid() {
		return msg, fmt.Errorf("%w: unknown mode %q", ErrMalformedMessage, msg.Mode)
	}
	if msg.BudgetMins <= 0 {
		return msg, fmt.Errorf("%w: budget_mins must be positive, got %d", ErrMalformedMessage, msg.BudgetMins)
	}
	return msg, nil
}
