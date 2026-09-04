// Package valhalla is the routing seam: the isochrone and matrix calls the
// chainer makes, the limits the server enforces on them, and two stand-in
// implementations for when there is no server to call.
//
// It descends from the Stadia Maps client in sparks-effect-api. Stadia's API is
// Valhalla's, so the request and response shapes carried over unchanged; what
// changed is that there is no vendor to authenticate to any more. The server is
// a ClusterIP service inside the home cluster, reachable only from the pod this
// worker runs in, which is the whole reason the worker exists.
package valhalla

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// The three failure modes a caller has to tell apart, because they decide
// whether a routing job is retried or abandoned.
//
// The split is by cause, not by status code, and getting it wrong is the
// failure mode that leaves a user watching a spinner forever: a permanent
// failure requeued loops against a server with room for one chain, and a
// transient failure abandoned fails a job that would have succeeded a second
// later.
var (
	// ErrBadRequest is a request Valhalla refused to answer — a contour past
	// the configured ceiling, too many matrix pairs, locations further apart
	// than the mode allows. Permanent: the same bytes will be refused again.
	ErrBadRequest = errors.New("valhalla: bad request")
	// ErrRateLimit is the server declining to take more work right now.
	// Transient by definition.
	ErrRateLimit = errors.New("valhalla: rate limit exceeded")
	// ErrUpstream is the server failing to answer — a 5xx, an unreachable
	// address, a timeout. Transient: the server may well be back.
	ErrUpstream = errors.New("valhalla: upstream error")
)

// Costing is Valhalla's word for a travel mode, and this package is the only
// place the translation from the domain's own vocabulary happens. See
// transit.TravelMode: walk → pedestrian, bike → bicycle, drive → auto,
// transit → multimodal.
type Costing string

const (
	CostingPedestrian Costing = "pedestrian"
	CostingBicycle    Costing = "bicycle"
	CostingAuto       Costing = "auto"
	// CostingMultimodal is walk plus scheduled transit. Domain
	// TravelModeTransit maps here, never to costing=transit, which is
	// transit-only and of no use for origins that are not already at a GTFS
	// stop (SPA-246). The Matrix endpoint rejects this costing (error 140);
	// see Costing.SupportsMatrix.
	CostingMultimodal Costing = "multimodal"
)

// SupportsMatrix reports whether Valhalla's /sources_to_targets will accept
// this costing. Multimodal is hard-rejected (error 140) regardless of tiles;
// the chainer's access-leg workaround is pairwise /route instead.
func (c Costing) SupportsMatrix() bool {
	switch c {
	case CostingPedestrian, CostingBicycle, CostingAuto:
		return true
	default:
		return false
	}
}

// LatLng is a WGS84 position. Lng, not Lon: the field is spelled the way the
// rest of this codebase spells it, and the wire spelling is applied at the
// point of marshalling.
type LatLng struct {
	Lat float64
	Lng float64
}

type IsochroneRequest struct {
	Origin     LatLng
	Costing    Costing
	BudgetSecs int
	// DepartAt is when the rider is standing at Origin. It is read only under
	// multimodal costing, where it decides which GTFS service day the answer is
	// computed against and which departures are running when the rider arrives;
	// the other costings have no timetable and ignore it.
	//
	// The zero value means "the caller has no opinion", and falls back to the
	// representative weekday morning (see WeekdayDepartAt). A caller that does
	// have one is expected to say so: an egress polygon is drawn from a station
	// the rider reaches hours after setting out, and stamping it with the same
	// morning peak as the origin gives it a service level that will not be
	// running by the time they arrive (SPA-274).
	DepartAt time.Time
}

// IsochroneResponse is the raw GeoJSON FeatureCollection Valhalla returns.
// Features carry polygon geometry and are preserved as raw JSON: nothing here
// reads inside one, and parsing a polygon only to re-emit it would be a lossy
// round trip for no gain.
type IsochroneResponse struct {
	Type     string            `json:"type"`
	Features []json.RawMessage `json:"features"`
}

type MatrixRequest struct {
	Origins      []LatLng
	Destinations []LatLng
	Costing      Costing
}

// MatrixCell is Valhalla's answer for one origin→destination pair: the travel
// time in seconds and the road distance in kilometres, or nothing at all where
// it found no route between them.
//
// Both fields are pointers because "no route" is spelled null on the wire —
// {"time": null, "distance": null} — and decoding that into a plain int gives
// zero, which is what a pair the rider is already standing on looks like. A
// station two hours away admitted at zero seconds is then the nearest thing in
// the plot and the access leg is drawn to it, which is exactly what SPA-243
// was. The -1 this used to test for was Stadia's sentinel, not Valhalla's;
// Routed still rejects it, so a server that does spell it that way is handled
// too.
type MatrixCell struct {
	Time       *int     `json:"time"`
	DistanceKm *float64 `json:"distance"`
}

// Routed reports whether the cell describes a journey Valhalla actually found.
// The zero MatrixCell is not one, which is what makes a missing pair and a
// declined pair the same thing to a caller.
func (c MatrixCell) Routed() bool {
	return c.Time != nil && *c.Time >= 0 && c.DistanceKm != nil && *c.DistanceKm >= 0
}

// Cell builds a routed cell. Stand-in clients and tests are the only callers:
// a decoded response has its pointers set by encoding/json, and taking the
// address of a loop variable at every one of those call sites is noise that
// hides what the cell is meant to say.
func Cell(secs int, distanceKm float64) MatrixCell {
	return MatrixCell{Time: &secs, DistanceKm: &distanceKm}
}

type MatrixResponse struct {
	// SourcesToTargets[i][j] is origin i → destination j.
	SourcesToTargets [][]MatrixCell `json:"sources_to_targets"`
}

// RouteRequest is one origin/destination pair to route between.
//
// It exists because a matrix answers with time and distance and nothing else:
// Valhalla genuinely walks the street network to produce that number, then
// throws the shape away. Where the shape is the point — the walking leg drawn
// on the map — the same pair has to be asked as a route instead.
type RouteRequest struct {
	Origin      LatLng
	Destination LatLng
	Costing     Costing
	// DepartAt is when the rider leaves Origin, read under multimodal costing
	// only. See IsochroneRequest.DepartAt, which it means the same thing as.
	DepartAt time.Time
}

// RouteResponse is the routed path, as positions.
//
// Decoded rather than raw, unlike IsochroneResponse's features: Valhalla sends
// the shape as an encoded polyline rather than as geometry, so there is no
// GeoJSON here to pass through untouched. Decoding is what this package is for
// — the wire format is Valhalla's business, and a caller that had to know about
// polyline6 would be reaching through the seam.
//
// The endpoints are Valhalla's own. It snaps each location to the nearest
// routable edge before routing, so Shape[0] is where the walk actually began
// rather than the point that was asked about, which may be inside a building or
// a lake.
type RouteResponse struct {
	Shape []LatLng
	// Time is the journey duration in seconds — Valhalla's trip.summary.time.
	// The chainer uses it as the access-leg seed when Matrix cannot answer
	// (multimodal costing, SPA-246).
	Time int
	// DistanceKm is the journey length in kilometres — trip.summary.length
	// with units=kilometers. Used the same way MatrixCell.DistanceKm is: a
	// route shorter than the straight line is not a real journey.
	DistanceKm float64
	// RodeTransit reports whether the rider boarded a scheduled service at any
	// point, read off the maneuvers' travel_mode.
	//
	// It is the one thing that distinguishes a multimodal answer from a walking
	// one. Valhalla does not refuse a multimodal request it can find no service
	// for — it walks the whole way and answers 200 — so a corridor whose GTFS
	// calendars have all expired is indistinguishable from one with a genuinely
	// poor service unless this is read (SPA-274). Always false under the other
	// costings, which have nothing to board. SyntheticClient reports it true
	// for multimodal, since its answer is timed as though a service had been
	// ridden — see its Route.
	RodeTransit bool
}

// Status is what /status reports about the server and the tiles it is serving.
//
// TilesetAt is the reason this call exists. It is the timestamp the isochrone
// cache records alongside a cached polygon, so "is this shape from before the
// last OSM refresh?" can be answered after the fact. Valhalla exposes it in the
// default (non-verbose) response, so no server-side configuration is needed to
// get at it.
type Status struct {
	Version   string
	TilesetAt time.Time
}

// Client is the routing seam. Four calls, all of them read-only.
type Client interface {
	Isochrone(ctx context.Context, req IsochroneRequest) (*IsochroneResponse, error)
	Matrix(ctx context.Context, req MatrixRequest) (*MatrixResponse, error)
	// Route walks one pair and returns the shape it took. It is the matrix
	// call's answer to a single pair, kept rather than discarded.
	Route(ctx context.Context, req RouteRequest) (*RouteResponse, error)
	// Status reports the server version and tileset timestamp. It is the one
	// call that may legitimately be unsupported — the stand-in client has no
	// tiles to describe — in which case it returns ErrNoStatus.
	Status(ctx context.Context) (Status, error)
}

// ErrNoStatus reports a client with no tileset to describe. It is not a
// failure of anything: the synthetic client computes polygons from arithmetic
// and has never seen a tile.
var ErrNoStatus = errors.New("valhalla: client reports no tileset status")

// Limits are the request ceilings the server enforces, expressed as
// configuration rather than constants.
//
// They were constants in sparks-effect-api, and the values there were Stadia
// plan allowances rather than anything Valhalla does. The deployed container
// enforces its own, some patched and some left at upstream defaults, and the
// defaults below are those actual values. A limit exceeded is a 400, which is
// permanent, so a wrong value here fails jobs rather than merely slowing them.
type Limits struct {
	// MaxMatrixPairs caps sources × targets in one matrix request. The chainer
	// sends a single source, so in practice this caps destinations.
	MaxMatrixPairs int
	// MaxIsoContourMins caps a single isochrone time contour.
	MaxIsoContourMins int
	// MatrixBLineAutoKm and MatrixBLineOtherKm cap the straight-line distance
	// between a matrix source and any target. Valhalla checks every pair and
	// rejects the whole request on the first one over, so the chainer
	// pre-filters destinations by this rather than discovering it as a 400.
	MatrixBLineAutoKm  float64
	MatrixBLineOtherKm float64
	// MaxConcurrency bounds how many requests one chain has in flight at once.
	// It is not a Valhalla limit but a courtesy to it: the egress fan-out is
	// unbounded in the number of stations, and the server is small.
	MaxConcurrency int
}

// Default limits, each matching what the deployed container actually enforces.
//
// The isochrone ceiling is patched. kustomize-config's valhalla.env sets
// ISOCHRONE_MAX_TIME_MINUTES=320, applied to
// service_limits.isochrone.max_time_contour by an init container on every pod
// start. The 120 that sparks-effect-api hard-coded was Stadia's plan limit and
// is nearly three times too strict here.
//
// The matrix limits are not patched, so Valhalla's own defaults apply.
// max_matrix_location_pairs is 2500 for auto, pedestrian and bicycle alike, and
// is enforced as sources × targets — so with one source, 2500 destinations. The
// 600 inherited from Stadia was *tighter*, not looser: raising it to Valhalla's
// real ceiling is what stops a large authored scenario being silently truncated
// to its 600 nearest stations.
//
// max_matrix_distance is 400 km for auto and 200 km for pedestrian and bicycle,
// which is exactly what the Stadia constants said. Those two numbers were right
// all along, for a different reason than the comment beside them gave.
//
// One caveat on the matrix ceiling, and it is the reason this is configuration.
// 2500 is read from current Valhalla — loki's matrix handler rejects
// sources x targets > max_matrix_location_pairs — while kustomize-config's
// deployment pins no version, running ghcr.io/valhalla/valhalla-scripted:latest.
// Older builds enforced max_matrix_locations, default 50. If the running
// container turns out to be one of those, this default converts what used to be
// silent truncation into a 400 that fails whole chains, and
// VALHALLA_MAX_MATRIX_PAIRS is how that is corrected without a release. Worth
// confirming against the deployed pod rather than left to inference.
const (
	DefaultMaxMatrixPairs     = 2500
	DefaultMaxIsoContourMins  = 320
	DefaultMatrixBLineAutoKm  = 400.0
	DefaultMatrixBLineOtherKm = 200.0
	// DefaultMaxConcurrency matches the eight threads the routing host is sized
	// for. Note that kustomize-config currently sets server_threads=2; a
	// deployment that keeps that should lower this to match, which is exactly
	// why it is configuration and not a constant.
	DefaultMaxConcurrency = 8
)

// DefaultLimits returns the limits the deployed container enforces.
func DefaultLimits() Limits {
	return Limits{
		MaxMatrixPairs:     DefaultMaxMatrixPairs,
		MaxIsoContourMins:  DefaultMaxIsoContourMins,
		MatrixBLineAutoKm:  DefaultMatrixBLineAutoKm,
		MatrixBLineOtherKm: DefaultMatrixBLineOtherKm,
		MaxConcurrency:     DefaultMaxConcurrency,
	}
}

// BLineLimitKm reports the matrix b-line ceiling for one costing.
func (l Limits) BLineLimitKm(c Costing) float64 {
	if c == CostingAuto {
		return l.MatrixBLineAutoKm
	}
	return l.MatrixBLineOtherKm
}
