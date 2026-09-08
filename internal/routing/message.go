package routing

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
)

const SchemaVersion = 1

var ErrUnsupportedSchema = errors.New("routing: unsupported schema version")

var ErrMalformedMessage = errors.New("routing: malformed message")

type Message struct {
	SchemaVersion int                   `json:"schema_version"`
	RoutingJobID  string                `json:"routing_job_id"`
	CompileJobID  string                `json:"compile_job_id"`
	Graph         *transit.TransitGraph `json:"graph"`
	Lat           float64               `json:"lat"`
	Lng           float64               `json:"lng"`
	BudgetMins    int                   `json:"budget_mins"`
	Mode          transit.TravelMode    `json:"mode"`
	TraceID       string                `json:"trace_id"`
}

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
