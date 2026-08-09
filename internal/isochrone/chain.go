// Package isochrone chains an access leg, a transit ride, and an egress leg
// into one reachability surface.
//
// The shape of the answer: from where the caller is standing, walk/cycle/drive
// to any station within the budget, ride to anywhere the graph reaches, and
// spend whatever budget is left travelling out from there. The union of the
// origin polygon and one polygon per station reached by riding transit is
// where a person could actually get to. A station reached without boarding
// anything contributes no polygon of its own — the origin polygon already
// covers how far a walk from it would go, since that is a walk from the same
// starting point — but it still appears among the reachable stations.
//
// This package moved here from sparks-effect-api unchanged in behaviour. What
// changed is where its inputs come from: the graph arrives inline on a queue
// message rather than being read from a store by scenario slug, and the request
// limits are configuration rather than a vendor's plan allowances.
package isochrone

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
)

var (
	// ErrInvalidMode reports a travel mode with no costing. Permanent.
	ErrInvalidMode = errors.New("isochrone: invalid mode")
	// ErrRoutingUnavailable reports Valhalla failing to answer. Transient.
	ErrRoutingUnavailable = errors.New("isochrone: routing unavailable")
	// ErrRoutingRejected reports Valhalla refusing a request. Permanent.
	ErrRoutingRejected = errors.New("isochrone: routing request rejected")
	// ErrRoutingRateLimit reports Valhalla declining more work. Transient.
	ErrRoutingRateLimit = errors.New("isochrone: routing rate limit exceeded")
)

// ChainRequest is one isochrone to compute over one already-resolved graph.
//
// It carries no scenario slug. The queue message names a compile job and
// carries the graph itself, so there is no scenario identity on this side of
// the repository boundary to resolve or to special-case on.
type ChainRequest struct {
	Lat        float64
	Lng        float64
	BudgetMins int
	Mode       transit.TravelMode
	// CompileJobID identifies the graph, and is reported back in the metadata
	// so a result can be traced to the graph it was plotted over.
	CompileJobID string
}

// Leg is one hop of a journey: the ride from one station to the next on one
// service, and the dwell served on arrival.
//
// Secs is the whole hop, DwellS the part of it the vehicle spends standing at
// ToSlug — so the ride itself is the difference. DwellS is absent on a graph
// compiled before the API reported dwell, which is indistinguishable from a hop
// that genuinely has none; such a journey reads as one with no dwell anywhere,
// which is incomplete but never wrong.
type Leg struct {
	FromSlug  string `json:"from"`
	ToSlug    string `json:"to"`
	ServiceID string `json:"service_id,omitempty"`
	Secs      int    `json:"secs"`
	DwellS    int    `json:"dwell_s,omitempty"`
}

// ReachableStation is one station the rider can get to and out from, with the
// budget accounting that got them there and the journey it took.
//
// The seconds fields sit alongside the whole-minute ones rather than replacing
// them: a caller adding hop to hop down a long journey accumulates a minute of
// error per stop if it works in truncated minutes, and the minute fields are
// what everything reading this response today already uses.
//
// PredecessorSlug is what makes the set a tree rather than a list. It names the
// station ridden from on the last hop, and is empty exactly when the rider
// walked here from the origin — so following it from any station arrives at one
// the rider walked to, and the stations with none are the roots.
//
// ViaService reports only the first service boarded. It is kept for callers
// that already read it and superseded by Legs, which names the service ridden
// on every hop and so stays right across an interchange.
type ReachableStation struct {
	StationSlug     string `json:"station_slug"`
	AccessMins      int    `json:"access_mins"`
	AccessSecs      int    `json:"access_secs"`
	RemainingMins   int    `json:"remaining_mins"`
	RemainingSecs   int    `json:"remaining_secs"`
	ViaService      string `json:"via_service,omitempty"`
	PredecessorSlug string `json:"predecessor_slug,omitempty"`
	// BoardSlug is where the rider boarded and BoardWaitSecs what they waited
	// there. The wait is charged once for the whole journey, so it belongs to
	// that station and not to any later one — a transfer costs nothing.
	BoardSlug     string `json:"board_slug,omitempty"`
	BoardWaitSecs int    `json:"board_wait_secs,omitempty"`
	// Legs is the journey retraced, boarding station first. It is empty for a
	// station the rider walked to and rode nothing from.
	Legs []Leg `json:"legs,omitempty"`
}

// StarterWalk is the routed access leg: the walk from the request's origin to
// the station the rider boards at.
//
// It exists because the matrix that timed that leg reports time and distance
// and nothing else. Valhalla did follow the street network to produce the
// number, and this is the same leg asked again as a route so that the shape is
// kept. Drawing it rather than a straight segment closes three gaps at once:
// the line follows streets instead of cutting across them, it starts where
// Valhalla snapped the origin rather than at the raw point the user gave, and
// it ends at the graph node the timing was measured to rather than at whatever
// the station's row says now.
//
// Only the starter station has one. Every other station is reached by riding,
// and its egress isochrone already says what that buys.
type StarterWalk struct {
	StationSlug string `json:"station_slug"`
	// Geometry is a GeoJSON LineString in [lng, lat] order, ready to draw.
	Geometry json.RawMessage `json:"geometry"`
}

// ChainMetadata describes how the surface below it was arrived at.
//
// CompileJobID replaces the scenario slug the API's version reported. The
// worker has no scenario identity — only the compile job that produced the
// graph — and that id is the more useful of the two anyway: it names one
// immutable graph rather than a scenario whose latest compile may since have
// changed.
type ChainMetadata struct {
	ReachableStations  []ReachableStation `json:"reachable_stations"`
	OriginBudgetMins   int                `json:"origin_budget_mins"`
	CompileJobID       string             `json:"compile_job_id"`
	Mode               string             `json:"mode"`
	WaitModel          string             `json:"wait_model"`
	OriginIsoClamped   bool               `json:"origin_iso_clamped,omitempty"`
	OriginIsoAvailable bool               `json:"origin_iso_available"`
	// StarterWalk is absent when there was no station to walk to, and also
	// when the route call for one failed. Its absence is never a reason to
	// distrust the surface above it — see chainer.starterWalk.
	StarterWalk *StarterWalk `json:"starter_walk,omitempty"`
}

// ChainResponse is a GeoJSON FeatureCollection plus the accounting behind it.
// It is what lands in routing_jobs.result, opaque to the API that reads it.
type ChainResponse struct {
	Type     string            `json:"type"`
	Features []json.RawMessage `json:"features"`
	Metadata ChainMetadata     `json:"metadata"`
}

// Chainer is the deep module interface.
type Chainer interface {
	Chain(ctx context.Context, req ChainRequest) (*ChainResponse, error)
}
