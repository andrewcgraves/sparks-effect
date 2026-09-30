package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

var ErrJobNotFound = errors.New("store: routing job not found")

type RoutingJobStore interface {
	MarkRunning(ctx context.Context, jobID string) error
	MarkSucceeded(ctx context.Context, jobID string, result json.RawMessage) error
	MarkFailed(ctx context.Context, jobID, message string) error
}

type IsochroneKey struct {
	CompileJobID string `json:"compile_job_id"`
	StationSlug  string `json:"station_slug"`
	Mode         string `json:"mode"`
	ContourMins  int    `json:"contour_mins"`
	DepartsOn    string `json:"departs_on,omitempty"`
	BudgetMins   int    `json:"budget_mins,omitempty"`
}

type CachedIsochrone struct {
	Key       IsochroneKey    `json:"key"`
	Geometry  json.RawMessage `json:"geometry"`
	TilesetAt time.Time       `json:"tileset_at,omitempty"`
}

type IsochroneCache interface {
	Get(ctx context.Context, keys []IsochroneKey) (map[IsochroneKey]json.RawMessage, error)
	Put(ctx context.Context, entries []CachedIsochrone) error
}
