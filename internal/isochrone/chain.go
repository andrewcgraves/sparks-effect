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

// ReachableStation is one station the rider can get to and out from, with the
// budget accounting that got them there.
type ReachableStation struct {
	StationSlug   string `json:"station_slug"`
	AccessMins    int    `json:"access_mins"`
	RemainingMins int    `json:"remaining_mins"`
	ViaService    string `json:"via_service,omitempty"`
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
