package isochrone

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
)

var (
	ErrInvalidMode        = errors.New("isochrone: invalid mode")
	ErrRoutingUnavailable = errors.New("isochrone: routing unavailable")
	ErrRoutingRejected    = errors.New("isochrone: routing request rejected")
	ErrRoutingRateLimit   = errors.New("isochrone: routing rate limit exceeded")
)

type ChainRequest struct {
	Lat          float64
	Lng          float64
	BudgetMins   int
	Mode         transit.TravelMode
	CompileJobID string
	DepartAt     time.Time
}

type Leg struct {
	FromSlug  string `json:"from"`
	ToSlug    string `json:"to"`
	ServiceID string `json:"service_id,omitempty"`
	Secs      int    `json:"secs"`
	DwellS    int    `json:"dwell_s,omitempty"`
}

type AccessBreakdown struct {
	AccessWalkSecs int `json:"access_walk_secs"`
	AccessWaitSecs int `json:"access_wait_secs"`
	AccessRideSecs int `json:"access_ride_secs"`
}

type ReachableStation struct {
	StationSlug     string `json:"station_slug"`
	AccessMins      int    `json:"access_mins"`
	AccessSecs      int    `json:"access_secs"`
	RemainingMins   int    `json:"remaining_mins"`
	RemainingSecs   int    `json:"remaining_secs"`
	ViaService      string `json:"via_service,omitempty"`
	PredecessorSlug string `json:"predecessor_slug,omitempty"`
	BoardSlug       string `json:"board_slug,omitempty"`
	BoardWaitSecs   int    `json:"board_wait_secs,omitempty"`
	Legs            []Leg  `json:"legs,omitempty"`
	// A pointer so a walked transit leg is an explicit false on the wire:
	// absent means the leg was not a transit access leg at all.
	AccessRodeTransit *bool `json:"access_rode_transit,omitempty"`
	// Embedded by pointer so its three fields sit flat beside access_secs and
	// board_wait_secs, and arrive all together or not at all: a walked leg
	// carries explicit zeros for wait and ride, while a leg with no split —
	// walk, bike and drive, or a split that did not reconcile — carries none.
	*AccessBreakdown
}

type TripProgress struct {
	FromSlug      string  `json:"from"`
	ToSlug        string  `json:"to"`
	ServiceID     string  `json:"service_id,omitempty"`
	RouteID       string  `json:"route_id"`
	FromChainageM float64 `json:"from_chainage_m"`
	ToChainageM   float64 `json:"to_chainage_m"`
	Fraction      float64 `json:"fraction"`
	RemainingSecs int     `json:"remaining_secs"`
	RideSecs      int     `json:"ride_secs"`
}

type StarterWalk struct {
	StationSlug string          `json:"station_slug"`
	Geometry    json.RawMessage `json:"geometry"`
}

type ChainMetadata struct {
	ReachableStations  []ReachableStation `json:"reachable_stations"`
	OriginBudgetMins   int                `json:"origin_budget_mins"`
	CompileJobID       string             `json:"compile_job_id"`
	Mode               string             `json:"mode"`
	WaitModel          string             `json:"wait_model"`
	OriginIsoClamped   bool               `json:"origin_iso_clamped,omitempty"`
	OriginIsoAvailable bool               `json:"origin_iso_available"`
	StarterWalk        *StarterWalk       `json:"starter_walk,omitempty"`
	TripProgress       []TripProgress     `json:"trip_progress,omitempty"`
	DroppedEgress      []string           `json:"dropped_egress,omitempty"`
}

type ChainResponse struct {
	Type     string            `json:"type"`
	Features []json.RawMessage `json:"features"`
	Metadata ChainMetadata     `json:"metadata"`
}

type Chainer interface {
	Chain(ctx context.Context, req ChainRequest) (*ChainResponse, error)
}
