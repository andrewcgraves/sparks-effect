package isochrone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync/atomic"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/geo"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

// minIsoContourSecs is the smallest contour worth requesting. Valhalla's
// contour is minute-granular, so anything under a minute serializes to
// {"time": 0} and is rejected — which would fail the whole fan-out, not just
// that one polygon.
const minIsoContourSecs = 60

// Wait model names, reported in the metadata so a caller can tell whether the
// surface it is looking at charges for boarding.
const (
	waitModelHeadway = "headway_over_2_peak"
	waitModelNone    = "none"
)

// modes is the one table mapping a travel mode onto everything the chainer
// needs to know about it: the costing to ask Valhalla for, the speed to size
// the destination pre-filter with, and — for transit — a reach cap tighter
// than any matrix b-line, because transit never calls Matrix. One table rather
// than a switch apiece so a fourth mode cannot be added to some of them and
// forgotten in the rest.
var modes = map[transit.TravelMode]struct {
	costing    valhalla.Costing
	speedKmH   float64
	maxReachKm float64
}{
	transit.TravelModeWalk:    {valhalla.CostingPedestrian, geo.WalkSpeedKmH, 0},
	transit.TravelModeBike:    {valhalla.CostingBicycle, geo.BikeSpeedKmH, 0},
	transit.TravelModeDrive:   {valhalla.CostingAuto, geo.DriveSpeedKmH, 0},
	transit.TravelModeTransit: {valhalla.CostingMultimodal, geo.TransitSpeedKmH, transitAccessReachKm},
}

// transitAccessReachKm caps how far a transit-mode access pair is worth asking
// /route about. Matrix's 200 km pedestrian b-line is not a target list:
// pairwise /route is the access-leg workaround (SPA-246), and 80 km is plenty
// for a local-transit ride to an HSR station.
const transitAccessReachKm = 80

func modeToCosting(m transit.TravelMode) (valhalla.Costing, error) {
	spec, ok := modes[m]
	if !ok {
		return "", ErrInvalidMode
	}
	return spec.costing, nil
}

// wrapRoutingErr translates the routing client's failure modes into this
// package's, preserving the transient/permanent distinction the consumer needs
// to decide whether to requeue.
func wrapRoutingErr(err error) error {
	switch {
	case errors.Is(err, valhalla.ErrBadRequest):
		return fmt.Errorf("%w: %v", ErrRoutingRejected, err)
	case errors.Is(err, valhalla.ErrRateLimit):
		return fmt.Errorf("%w: %v", ErrRoutingRateLimit, err)
	default:
		return fmt.Errorf("%w: %v", ErrRoutingUnavailable, err)
	}
}

type chainImpl struct {
	routing valhalla.Client
	graph   transit.GraphData
	limits  valhalla.Limits
	cache   EgressCache
	log     *logger.Logger
}

// New constructs a chainer over one compiled graph.
//
// The graph is a constructor argument rather than a per-request one because a
// chainer's whole life is one message: the worker builds one, computes one
// isochrone with it, and discards it. There is no store to consult and nothing
// to resolve, which is what the queue message carrying the graph inline bought.
//
// The cache outlives the chainer, unlike everything else here — it is shared
// across messages, which is the entire point of it. A zero EgressCache disables
// reuse; see its documentation.
func New(routingClient valhalla.Client, graph transit.GraphData, limits valhalla.Limits, cache EgressCache, log *logger.Logger) Chainer {
	return &chainImpl{routing: routingClient, graph: graph, limits: limits, cache: cache, log: log}
}

// approxAccessReachKm bounds how far a station can be and still be worth asking
// the matrix about.
//
// Two ceilings apply and the tighter wins. The budget one is what the rider
// could cover at all; the b-line one is Valhalla's own matrix limit, and
// exceeding it on any single pair rejects the entire request rather than
// dropping that pair — so this filter is what keeps a distant station from
// failing the whole chain.
func (c *chainImpl) approxAccessReachKm(mode transit.TravelMode, costing valhalla.Costing, budgetMins int) float64 {
	spec := modes[mode]
	budgetReachKm := spec.speedKmH * float64(budgetMins) / 60.0 / geo.DetourFactor
	capKm := c.limits.BLineLimitKm(costing)
	if spec.maxReachKm > 0 {
		capKm = spec.maxReachKm
	}
	return math.Min(budgetReachKm, capKm)
}

// clampToContourCeilingSecs caps a contour at the configured isochrone ceiling.
// Mode is irrelevant — the limit is on the contour value, not on distance
// covered.
func (c *chainImpl) clampToContourCeilingSecs(budgetSecs int) int {
	ceiling := c.limits.MaxIsoContourMins * 60
	if budgetSecs > ceiling {
		return ceiling
	}
	return budgetSecs
}

func (c *chainImpl) Chain(ctx context.Context, req ChainRequest) (*ChainResponse, error) {
	costing, err := modeToCosting(req.Mode)
	if err != nil {
		return nil, ErrInvalidMode
	}

	// One chain, one departure. req is this function's own copy, so writing the
	// resolved value back onto it hands the same moment to every call below
	// without threading it through six signatures — and makes it impossible for
	// two of them to resolve their own (SPA-274).
	if req.DepartAt.IsZero() {
		req.DepartAt = valhalla.WeekdayDepartAt(time.Now(), valhalla.ServiceAreaZone())
	}

	c.log.Info(ctx, "chain: starting",
		"compile_job_id", req.CompileJobID,
		"mode", req.Mode,
		"budget_mins", req.BudgetMins,
		"lat", req.Lat,
		"lng", req.Lng,
		"departs_at", req.DepartAt.Format(time.RFC3339))

	nodes := c.graph.Nodes()
	c.log.Debug(ctx, "chain: graph loaded", "node_count", len(nodes))

	nearbyNodes := c.nearbyNodes(ctx, req, costing, nodes)

	originContourSecs := c.clampToContourCeilingSecs(req.BudgetMins * 60)
	originIsoClamped := originContourSecs < req.BudgetMins*60
	if originIsoClamped {
		c.log.Info(ctx, "chain: origin contour clamped to ceiling",
			"compile_job_id", req.CompileJobID,
			"requested_secs", req.BudgetMins*60,
			"clamped_secs", originContourSecs)
	}

	originIso, matrixResp, err := c.originAndAccess(ctx, req, costing, originContourSecs, nearbyNodes)
	if err != nil {
		return nil, err
	}
	c.log.Debug(ctx, "chain: origin isochrone and access times done")

	accessSecs := c.accessTimes(ctx, req, nearbyNodes, matrixResp)
	c.log.Debug(ctx, "chain: access reachability", "reachable", len(accessSecs), "total_nodes", len(nodes))

	egressCandidates := c.bestEgressPerNode(req, nodes, accessSecs)
	c.log.Debug(ctx, "chain: egress candidates", "count", len(egressCandidates))

	egressIsos, starterWalk, err := c.egressAndStarterWalk(ctx, req, costing, egressCandidates)
	if err != nil {
		return nil, err
	}

	return c.assemble(ctx, req, originIso, originIsoClamped, egressCandidates, egressIsos, starterWalk)
}

// egressAndStarterWalk runs the egress fan-out and the starter station's route
// call together.
//
// The route cannot go out any earlier than this. Which station the rider walks
// to is decided by the access times, and those are the matrix's answer, so the
// destination is not known until the matrix has returned — SPA-196 hoped to
// issue it alongside the matrix, and that is the reason it cannot be. Running
// it against the fan-out instead costs nothing measurable: the fan-out is one
// call per station reached by transit and is what a chain's latency is made of,
// so one more call beside it does not show.
func (c *chainImpl) egressAndStarterWalk(
	ctx context.Context,
	req ChainRequest,
	costing valhalla.Costing,
	candidates []egressCandidate,
) ([]*valhalla.IsochroneResponse, *StarterWalk, error) {
	var (
		egressIsos  []*valhalla.IsochroneResponse
		starterWalk *StarterWalk
	)

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		isos, err := c.egressIsochrones(gctx, req, costing, candidates)
		if err != nil {
			return err
		}
		egressIsos = isos
		return nil
	})

	if starter := pickStarter(candidates); starter != nil {
		g.Go(func() error {
			starterWalk = c.starterWalk(gctx, req, costing, *starter)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, err
	}
	return egressIsos, starterWalk, nil
}

// pickStarter names the station the rider walks to before boarding anything.
//
// It is the shortest walk among the stations reached on foot — every other
// candidate was arrived at by riding — with the slug breaking ties so that the
// same plot names the same station every time rather than following whatever
// order the graph happened to list its nodes in.
//
// The comparison is on whole minutes, which is what makes those ties common:
// accessMins is the access time truncated, so 119 seconds and 60 seconds are
// both one minute. That is the number the response reports and the number the
// frontend used to sort on, so ranking on anything finer here would have the
// worker name a station a reader of the response could not arrive at. SPA-196
// records the truncation as a wrinkle of its own, left alone here.
func pickStarter(candidates []egressCandidate) *egressCandidate {
	var starter *egressCandidate
	for i := range candidates {
		ec := &candidates[i]
		if ec.viaTransit {
			continue
		}
		if starter == nil ||
			ec.accessMins < starter.accessMins ||
			(ec.accessMins == starter.accessMins && ec.node.Slug < starter.node.Slug) {
			starter = ec
		}
	}
	return starter
}

// starterWalk routes the access leg and shapes it into a drawable LineString,
// reporting nil rather than an error when it cannot.
//
// A failed route loses the line, never the plot. The surface is complete
// without it — the origin polygon already covers how far that walk reaches —
// and the routing failures most likely here are the ones a whole class of
// origins provokes: a point dropped in a lake or on an island is a permanent
// 400 from Valhalla, and failing the job over it would turn a plot that used to
// render into one that does not.
//
// This is the one place in the chain that does not act on the transient /
// permanent split the routing client is at pains to draw, and dropping it is
// deliberate rather than an oversight. That distinction exists to decide
// whether to requeue a job, and there is no job to requeue here: every other
// call in the chain has already succeeded by the time this one is judged, so
// retrying would mean re-running the whole chain to recover an annotation on a
// surface that is already correct. The next plot asks again anyway.
func (c *chainImpl) starterWalk(
	ctx context.Context,
	req ChainRequest,
	costing valhalla.Costing,
	starter egressCandidate,
) *StarterWalk {
	route, err := c.routing.Route(ctx, valhalla.RouteRequest{
		Origin:      valhalla.LatLng{Lat: req.Lat, Lng: req.Lng},
		Destination: valhalla.LatLng{Lat: starter.node.Lat, Lng: starter.node.Lng},
		Costing:     costing,
		DepartAt:    req.DepartAt,
	})
	if err != nil {
		c.log.Debug(ctx, "chain: starter walk not routed, drawing none", "station_slug", starter.node.Slug, "err", err.Error())
		return nil
	}
	// A shape of fewer than two positions is not a line. The HTTP client
	// rejects one already; this is what keeps a stand-in client that does not
	// from putting a degenerate LineString on the wire.
	if route == nil || len(route.Shape) < 2 {
		c.log.Debug(ctx, "chain: starter walk came back without a line, drawing none", "station_slug", starter.node.Slug)
		return nil
	}

	geometry, err := lineStringFrom(route.Shape)
	if err != nil {
		c.log.Debug(ctx, "chain: starter walk not encodable, drawing none", "station_slug", starter.node.Slug, "err", err.Error())
		return nil
	}
	c.log.Debug(ctx, "chain: starter walk routed", "station_slug", starter.node.Slug, "points", len(route.Shape))

	return &StarterWalk{StationSlug: starter.node.Slug, Geometry: geometry}
}

// lineStringFrom renders a routed shape as GeoJSON, in the [lng, lat] order
// GeoJSON requires and the rest of this response already uses.
func lineStringFrom(shape []valhalla.LatLng) (json.RawMessage, error) {
	coords := make([][2]float64, len(shape))
	for i, p := range shape {
		coords[i] = [2]float64{p.Lng, p.Lat}
	}
	return json.Marshal(map[string]any{
		"type":        "LineString",
		"coordinates": coords,
	})
}

// nearbyNodes narrows the graph to the stations worth putting in the matrix:
// those within reach, and no more of them than the matrix will accept.
func (c *chainImpl) nearbyNodes(ctx context.Context, req ChainRequest, costing valhalla.Costing, nodes []transit.Node) []transit.Node {
	reachKm := c.approxAccessReachKm(req.Mode, costing, req.BudgetMins)

	var nearby []transit.Node
	for _, n := range nodes {
		if geo.HaversineKm(req.Lat, req.Lng, n.Lat, n.Lng) <= reachKm {
			nearby = append(nearby, n)
		}
	}
	c.log.Debug(ctx, "chain: nodes within haversine reach", "reachable", len(nearby), "total_nodes", len(nodes), "reach_km", reachKm)

	// One source, so the pair limit is a destination limit. Truncating to the
	// nearest is the least-bad way to lose stations: the ones dropped are the
	// ones with the least access budget left, hence the smallest egress
	// polygons. Transit never calls Matrix, so this cap does not apply — the
	// haversine reach (and MaxConcurrency on the pairwise /route fan-out) is
	// what bounds that path (SPA-246).
	if costing.SupportsMatrix() {
		if max := c.limits.MaxMatrixPairs; max > 0 && len(nearby) > max {
			sort.Slice(nearby, func(i, j int) bool {
				di := geo.HaversineKm(req.Lat, req.Lng, nearby[i].Lat, nearby[i].Lng)
				dj := geo.HaversineKm(req.Lat, req.Lng, nearby[j].Lat, nearby[j].Lng)
				return di < dj
			})
			nearby = nearby[:max]
			c.log.Info(ctx, "chain: nodes truncated to matrix pair cap",
				"compile_job_id", req.CompileJobID, "cap", max)
		}
	}
	return nearby
}

// originAndAccess fetches the origin polygon and the access times together.
// They do not depend on each other, and the access call is the slower of the
// two, so serialising them would add the origin call's latency to every
// request for nothing.
//
// Access times come from Matrix for costings that support it, and from
// pairwise /route for multimodal, which Matrix hard-rejects (SPA-246). The
// answer is shaped as a MatrixResponse either way so accessTimes has one
// reader.
func (c *chainImpl) originAndAccess(
	ctx context.Context,
	req ChainRequest,
	costing valhalla.Costing,
	originContourSecs int,
	nearbyNodes []transit.Node,
) (*valhalla.IsochroneResponse, *valhalla.MatrixResponse, error) {
	var (
		originIso  *valhalla.IsochroneResponse
		matrixResp *valhalla.MatrixResponse
	)

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		iso, isoErr := c.routing.Isochrone(gctx, valhalla.IsochroneRequest{
			Origin:     valhalla.LatLng{Lat: req.Lat, Lng: req.Lng},
			Costing:    costing,
			BudgetSecs: originContourSecs,
			DepartAt:   req.DepartAt,
		})
		if isoErr != nil {
			return wrapRoutingErr(isoErr)
		}
		originIso = iso
		return nil
	})

	if len(nearbyNodes) > 0 {
		g.Go(func() error {
			m, mErr := c.accessMatrix(gctx, req, costing, nearbyNodes)
			if mErr != nil {
				return mErr
			}
			matrixResp = m
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, err
	}
	return originIso, matrixResp, nil
}

func (c *chainImpl) accessMatrix(
	ctx context.Context,
	req ChainRequest,
	costing valhalla.Costing,
	nearbyNodes []transit.Node,
) (*valhalla.MatrixResponse, error) {
	if !costing.SupportsMatrix() {
		return c.pairwiseAccess(ctx, req, costing, nearbyNodes)
	}
	dests := make([]valhalla.LatLng, len(nearbyNodes))
	for i, n := range nearbyNodes {
		dests[i] = valhalla.LatLng{Lat: n.Lat, Lng: n.Lng}
	}
	m, mErr := c.routing.Matrix(ctx, valhalla.MatrixRequest{
		Origins:      []valhalla.LatLng{{Lat: req.Lat, Lng: req.Lng}},
		Destinations: dests,
		Costing:      costing,
	})
	if mErr != nil {
		return nil, wrapRoutingErr(mErr)
	}
	return m, nil
}

// pairwiseAccess is the Matrix workaround for multimodal costing: one /route
// per nearby station, bounded by MaxConcurrency, assembled into the same
// matrix shape accessTimes already reads.
//
// A 400 from /route drops that station the way a null Matrix cell does —
// "no transit near here" is a fact about that pair, not a reason to fail the
// chain. Rate-limits and upstream failures still fail the job; those are
// transient, and retrying is the right answer.
//
// How many of the answers actually boarded a service is counted and reported.
// A multimodal request Valhalla can find no service for is answered 200 with a
// walking itinerary, so a corridor whose GTFS calendars have expired produces a
// chain that succeeds, draws polygons, and is quietly no better than walking.
// Nothing in this worker used to look, which is why it took a manual
// reproduction to find (SPA-274). The count is not an error — an origin genuinely
// far from any service is a legitimate zero — so it is logged rather than
// enforced, and the acceptance checks in internal/valhalla are what assert it
// on a pair where the answer is known.
func (c *chainImpl) pairwiseAccess(
	ctx context.Context,
	req ChainRequest,
	costing valhalla.Costing,
	nearbyNodes []transit.Node,
) (*valhalla.MatrixResponse, error) {
	cells := make([]valhalla.MatrixCell, len(nearbyNodes))
	var routed, rode atomic.Int64

	concurrency := c.limits.MaxConcurrency
	if concurrency <= 0 {
		concurrency = valhalla.DefaultMaxConcurrency
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	origin := valhalla.LatLng{Lat: req.Lat, Lng: req.Lng}
	for i, n := range nearbyNodes {
		g.Go(func() error {
			route, err := c.routing.Route(gctx, valhalla.RouteRequest{
				Origin:      origin,
				Destination: valhalla.LatLng{Lat: n.Lat, Lng: n.Lng},
				Costing:     costing,
				DepartAt:    req.DepartAt,
			})
			if err != nil {
				if errors.Is(err, valhalla.ErrBadRequest) {
					c.log.Debug(gctx, "chain: pairwise access route declined, dropping station",
						"station_slug", n.Slug, "err", err.Error())
					return nil
				}
				return wrapRoutingErr(err)
			}
			if route == nil {
				return nil
			}
			routed.Add(1)
			if route.RodeTransit {
				rode.Add(1)
			}
			cells[i] = valhalla.Cell(route.Time, route.DistanceKm)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	c.log.Info(ctx, "chain: transit access legs routed",
		"compile_job_id", req.CompileJobID,
		"asked", len(nearbyNodes),
		"routed", routed.Load(),
		"rode_transit", rode.Load(),
		"departs_at", req.DepartAt.Format(time.RFC3339))

	return &valhalla.MatrixResponse{SourcesToTargets: [][]valhalla.MatrixCell{cells}}, nil
}

// snapSlackKm is how much shorter than the straight line a routed distance may
// honestly come out.
//
// Valhalla correlates each end of a pair to the nearest edge it will accept,
// up to valhalla.LocationRadiusM away, so both ends can move inward and the
// road it then measures spans slightly less than the line between the raw
// points. Twice the radius is the most that can account for.
const snapSlackKm = 2 * float64(valhalla.LocationRadiusM) / 1000.0

// accessTimes reads the matrix row into the access seconds the search is
// seeded with, keeping only the pairs Valhalla actually routed.
//
// Two things disqualify a cell, and both say the same thing in different
// words. One is Valhalla declining the pair outright — see valhalla.MatrixCell.
// The other is a cell that is arithmetically impossible: a road cannot be
// shorter than the straight line it spans, so a distance under that line is
// not a measurement of any journey, and the time beside it is not an access
// time. That is the shape a declined pair takes on a server that reports one
// as a pair of zeroes rather than a pair of nulls.
//
// Dropping them matters far beyond one wrong number. A zero-second access leg
// makes a station the closest thing in the plot however far away it is: it
// becomes the station the access leg is drawn to, it seeds the search with the
// rider standing on a platform two hours' drive away, and every station beyond
// it inherits a journey that never happened. Drive is where this shows,
// because drive is the only mode whose reach admits a station distant enough
// for the routing graph not to have it.
//
// Both drops are reported by name, and that is the point of them being here
// rather than folded into the guard above. A station within the rider's reach
// that the routing graph cannot answer for is a gap in the map data, not a
// fact about this plot: the site is greenfield, or its road is not in OSM yet,
// and the answer is a routing anchor on the station (see sparks-effect-api's
// Station.RoutingLocation, SPA-234) rather than anything this worker can
// compute. Dropped silently, that gap shows up only as a station quietly
// missing from every drive plot, which is how it went unnoticed until it
// produced a visibly wrong one.
func (c *chainImpl) accessTimes(
	ctx context.Context,
	req ChainRequest,
	nearbyNodes []transit.Node,
	matrixResp *valhalla.MatrixResponse,
) map[string]int {
	accessSecs := make(map[string]int)
	if matrixResp == nil || len(matrixResp.SourcesToTargets) == 0 {
		return accessSecs
	}

	var unrouted, implausible []string

	row := matrixResp.SourcesToTargets[0]
	for i, n := range nearbyNodes {
		if i >= len(row) {
			break
		}
		cell := row[i]
		if !cell.Routed() {
			unrouted = append(unrouted, n.Slug)
			continue
		}
		bLineKm := geo.HaversineKm(req.Lat, req.Lng, n.Lat, n.Lng)
		if *cell.DistanceKm+snapSlackKm < bLineKm {
			implausible = append(implausible, n.Slug)
			c.log.Debug(ctx, "chain: matrix pair shorter than the straight line",
				"station_slug", n.Slug,
				"secs", *cell.Time,
				"distance_km", *cell.DistanceKm,
				"b_line_km", bLineKm)
			continue
		}
		accessSecs[n.Slug] = *cell.Time
	}

	c.logDropped(ctx, req, "chain: routing graph found no route to these stations, dropping them", unrouted)
	c.logDropped(ctx, req, "chain: routing graph answered these stations with a road shorter than the straight line, dropping them", implausible)

	return accessSecs
}

// maxLoggedSlugs bounds the station list on the summary below. The count
// beside it is never truncated, so a run that dropped more than this still
// says how many: the list is there to name the handful an operator is expected
// to go and fix, not to be exhaustive on a graph where something has gone
// wrong systematically.
const maxLoggedSlugs = 20

// logDropped reports one reason's worth of dropped stations, and says nothing
// at all when there were none — the ordinary case, where every station within
// reach was answered for.
func (c *chainImpl) logDropped(ctx context.Context, req ChainRequest, msg string, slugs []string) {
	if len(slugs) == 0 {
		return
	}
	listed := slugs
	if len(listed) > maxLoggedSlugs {
		listed = listed[:maxLoggedSlugs]
	}
	c.log.Info(ctx, msg,
		"compile_job_id", req.CompileJobID,
		"mode", req.Mode,
		"count", len(slugs),
		"station_slugs", listed)
}

type egressCandidate struct {
	node          transit.Node
	remainingSecs int
	remainingMins int
	accessSecs    int
	accessMins    int
	// journey is how the rider got here: the predecessor and hop the search
	// settled on, and the one boarding that paid for it. legs is that same
	// journey retraced from the station the rider boarded at.
	journey transit.Reached
	legs    []Leg
	// viaTransit reports whether reaching this station's best path boards a
	// service, as opposed to walking straight to it from the origin. Only a
	// via-transit candidate gets an egress polygon computed for it — see
	// egressIsochrones.
	viaTransit bool
}

// bestEgressPerNode works out, for every node in the graph, the journey that
// leaves the most budget on arrival there.
//
// One search settles all of them at once, seeded with every access station the
// matrix reached at the cost of reaching it, so the answer accounts for the
// nearest station being on a slow service while one two minutes further away is
// on an express. What it adds over reporting a duration is the path: the search
// keeps the predecessor of every node it settles, which is what lets the
// response describe the trip rather than only its length.
//
// A node whose remaining budget is under a minute is dropped rather than
// carried, because a sub-minute contour is a request Valhalla rejects.
func (c *chainImpl) bestEgressPerNode(
	req ChainRequest,
	nodes []transit.Node,
	accessSecs map[string]int,
) []egressCandidate {
	budgetSecs := req.BudgetMins * 60
	reached := c.graph.Search(accessSecs)

	// Built by walking nodes rather than the map so the fan-out, the features
	// and the reported stations all come out in graph order, which is stable
	// across runs. Ranging the map directly would reorder the output on every
	// request.
	var candidates []egressCandidate
	for _, n := range nodes {
		journey, ok := reached[n.Slug]
		if !ok {
			continue
		}
		rSecs := budgetSecs - journey.TotalSecs
		if rSecs < minIsoContourSecs {
			continue
		}
		aSecs := accessSecs[journey.SourceSlug]
		candidates = append(candidates, egressCandidate{
			node:          n,
			remainingSecs: rSecs,
			remainingMins: rSecs / 60,
			accessSecs:    aSecs,
			accessMins:    aSecs / 60,
			journey:       journey,
			legs:          legsTo(n.Slug, reached),
			viaTransit:    journey.PredecessorSlug != "",
		})
	}
	return candidates
}

// legsTo retraces the journey to one station, hop by hop, from the station the
// rider boarded at to the station itself.
//
// The path is walked backwards off the predecessor pointers and then reversed,
// which is the only reason those pointers are kept. A journey that rode nothing
// has no legs at all.
//
// The bound is the settled set's own size: predecessors form a tree over
// settled nodes and cannot cycle, but a graph with zero-second edges could in
// principle produce one, and this response is built from bytes off a queue.
func legsTo(slug string, reached map[string]transit.Reached) []Leg {
	var reversed []Leg
	for cur := slug; len(reversed) <= len(reached); {
		r, ok := reached[cur]
		if !ok || r.PredecessorSlug == "" {
			break
		}
		reversed = append(reversed, Leg{
			FromSlug:  r.PredecessorSlug,
			ToSlug:    cur,
			ServiceID: r.HopService,
			Secs:      r.HopSecs,
			DwellS:    r.HopDwellS,
		})
		cur = r.PredecessorSlug
	}
	if len(reversed) == 0 {
		return nil
	}
	legs := make([]Leg, len(reversed))
	for i, l := range reversed {
		legs[len(reversed)-1-i] = l
	}
	return legs
}

// egressDepartAt is when this rider is standing at this station, ready to
// travel out from it: the chain's own departure plus everything the journey
// there cost — the access leg, the boarding wait, and every hop ridden.
//
// The origin's departure is the wrong clock for it. Every egress request used
// to be stamped 08:00, which is defensible for the origin polygon and the
// access legs and wrong for egress, which happens after both: compiled Local is
// 5 h 06 m SF→Anaheim, so a rider leaving at 08:00 steps off in the early
// afternoon and was nonetheless handed a peak-service reach (SPA-274, recorded
// as SPA-272 defect B).
//
// journey.TotalSecs is that whole cost, already worked out by the search that
// decided this station was reachable at all.
func (ec egressCandidate) egressDepartAt(chainDepartAt time.Time) time.Time {
	return chainDepartAt.Add(time.Duration(ec.journey.TotalSecs) * time.Second)
}

// serviceDate names the GTFS service day a cached polygon belongs to, and is
// empty for the costings with no timetable to belong to one.
//
// The gate is on the costing rather than the mode because the costing is what
// decides whether a departure was sent at all: a row keyed by a date its
// request never carried would be a row nothing can hit.
//
// A date, and not the departure itself, because that is what the cache column
// is — see the accepted risk in README's "Caching egress polygons", which this
// is the other half of.
func serviceDate(costing valhalla.Costing, departAt time.Time) string {
	if costing != valhalla.CostingMultimodal {
		return ""
	}
	return valhalla.ServiceDate(departAt)
}

// egressIsochrones produces one polygon per station reached by riding transit,
// reusing whatever the cache already holds and computing the rest.
//
// A walk-only candidate — reached without boarding any service — gets no
// polygon at all: it would just restate how far the rider can walk from a
// station they already walked to, which the origin polygon already covers.
// Excluding it here means it never enters the fan-out, is never asked of
// Valhalla, and is never written to the cache; assemble still lists it, since
// bestEgressPerNode already worked out its access time.
//
// This is where the caching pays: the fan-out is the bulk of a chain's routing
// work — one call per station reached by transit against two for everything
// else — and every one of those calls starts from a station the publication
// fixed in place. See EgressCache.
//
// The computed remainder is bounded rather than run as a plain errgroup: the
// fan-out is as wide as the graph has stations reached by transit, and the
// routing server is a single small container. Without the bound, one large
// scenario would open hundreds of concurrent requests against a server sized
// for a handful.
func (c *chainImpl) egressIsochrones(
	ctx context.Context,
	req ChainRequest,
	costing valhalla.Costing,
	candidates []egressCandidate,
) ([]*valhalla.IsochroneResponse, error) {
	isos := make([]*valhalla.IsochroneResponse, len(candidates))
	if len(candidates) == 0 {
		return isos, nil
	}

	var served []int
	for i, ec := range candidates {
		if ec.viaTransit {
			served = append(served, i)
		}
	}
	if len(served) == 0 {
		return isos, nil
	}

	// Truncation to the contour ceiling is silent and unflagged: it needs a
	// budget over the ceiling plus access time to trigger, and a ceiling-sized
	// egress polygon is a fine answer to an over-ceiling ask. Only the origin
	// reports clamping, since that is the number the caller actually chose.
	//
	// The clamped value is also what the key carries, so every budget that
	// clamps to the same contour shares one row rather than storing the same
	// shape once per budget. The key is therefore the only place the contour is
	// kept: what was asked of Valhalla and what was cached cannot drift apart
	// if there is only one of them.
	//
	// keys and subIsos are indexed by position in served, not in candidates —
	// walk-only candidates have no key at all, so candidates' own indices
	// would leave gaps for the cache and the fan-out to trip over.
	//
	// departs is parallel to them, and is the clock the polygon is drawn on:
	// see egressDepartAt. Working it out here rather than at the call below is
	// what lets the key carry the same date the request is stamped with, the
	// way the contour is kept in one place for the same reason.
	keys := make([]store.IsochroneKey, len(served))
	departs := make([]time.Time, len(served))
	for j, i := range served {
		departs[j] = candidates[i].egressDepartAt(req.DepartAt)
		keys[j] = store.IsochroneKey{
			CompileJobID: req.CompileJobID,
			StationSlug:  candidates[i].node.Slug,
			Mode:         string(req.Mode),
			ContourMins:  c.clampToContourCeilingSecs(candidates[i].remainingMins*60) / 60,
			DepartsOn:    serviceDate(costing, departs[j]),
		}
	}

	hits := c.cache.lookup(ctx, keys, c.log)

	// Scattered back into isos, at candidates' own indices, on every return
	// below — including the all-hits one, so served's callers see one
	// candidate-indexed result regardless of how much of it came from cache.
	subIsos := make([]*valhalla.IsochroneResponse, len(served))
	defer func() {
		for j, i := range served {
			isos[i] = subIsos[j]
		}
	}()

	var misses []int
	for j := range served {
		if iso, ok := hits[keys[j]]; ok {
			subIsos[j] = iso
			continue
		}
		misses = append(misses, j)
	}
	c.log.Debug(ctx, "chain: egress cache lookup", "hits", len(served)-len(misses), "misses", len(misses))
	if len(misses) == 0 {
		return isos, nil
	}

	concurrency := c.limits.MaxConcurrency
	if concurrency <= 0 {
		concurrency = valhalla.DefaultMaxConcurrency
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	for _, j := range misses {
		i := served[j]
		g.Go(func() error {
			iso, isoErr := c.routing.Isochrone(gctx, valhalla.IsochroneRequest{
				Origin:     valhalla.LatLng{Lat: candidates[i].node.Lat, Lng: candidates[i].node.Lng},
				Costing:    costing,
				BudgetSecs: keys[j].ContourMins * 60,
				DepartAt:   departs[j],
			})
			if isoErr != nil {
				return wrapRoutingErr(isoErr)
			}
			subIsos[j] = iso
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Written on ctx rather than gctx: the errgroup's context is cancelled the
	// moment Wait returns, and cancelling the write would discard work that has
	// already succeeded.
	c.cache.record(ctx, keys, subIsos, misses, c.log)

	return isos, nil
}

// assemble stitches the polygons into one FeatureCollection, tagging each
// feature with where it came from so a client can style origin and egress
// differently.
func (c *chainImpl) assemble(
	ctx context.Context,
	req ChainRequest,
	originIso *valhalla.IsochroneResponse,
	originIsoClamped bool,
	candidates []egressCandidate,
	egressIsos []*valhalla.IsochroneResponse,
	starterWalk *StarterWalk,
) (*ChainResponse, error) {
	features := make([]json.RawMessage, 0)
	if originIso != nil {
		for _, f := range originIso.Features {
			injected, err := injectProperties(f, map[string]any{"source": "origin"})
			if err != nil {
				return nil, fmt.Errorf("isochrone: inject origin: %w", err)
			}
			features = append(features, injected)
		}
	}

	reachableStations := make([]ReachableStation, 0, len(candidates))
	for i, ec := range candidates {
		// A walk-only candidate has no egress polygon by design (see
		// egressIsochrones) and stays out of the feature set, but it is still a
		// station the rider reached, so it stays in the list below.
		if egressIsos[i] != nil {
			for _, f := range egressIsos[i].Features {
				injected, err := injectProperties(f, map[string]any{
					"source":         "egress",
					"station_slug":   ec.node.Slug,
					"remaining_mins": ec.remainingMins,
				})
				if err != nil {
					return nil, fmt.Errorf("isochrone: inject egress: %w", err)
				}
				features = append(features, injected)
			}
		}
		reachableStations = append(reachableStations, ReachableStation{
			StationSlug:     ec.node.Slug,
			AccessMins:      ec.accessMins,
			AccessSecs:      ec.accessSecs,
			RemainingMins:   ec.remainingMins,
			RemainingSecs:   ec.remainingSecs,
			ViaService:      ec.journey.BoardService,
			PredecessorSlug: ec.journey.PredecessorSlug,
			BoardSlug:       ec.journey.BoardSlug,
			BoardWaitSecs:   ec.journey.BoardWaitSecs,
			Legs:            ec.legs,
		})
	}

	c.log.Info(ctx, "chain: complete",
		"compile_job_id", req.CompileJobID,
		"features", len(features),
		"reachable_stations", len(reachableStations))

	return &ChainResponse{
		Type:     "FeatureCollection",
		Features: features,
		Metadata: ChainMetadata{
			ReachableStations: reachableStations,
			OriginBudgetMins:  req.BudgetMins,
			CompileJobID:      req.CompileJobID,
			Mode:              string(req.Mode),
			WaitModel:         c.waitModel(),
			OriginIsoClamped:  originIsoClamped,
			// Always true in practice now that the origin polygon is never
			// skipped — a routing failure returns an error instead. Kept on the
			// wire for compatibility with clients that already read it.
			OriginIsoAvailable: originIso != nil,
			StarterWalk:        starterWalk,
		},
	}, nil
}

// waitModel reports whether boarding wait was charged, read off the graph.
//
// The API's version of this asked whether the scenario slug was the one
// wait-free scenario. The queue message carries no scenario identity, so the
// question is instead put to the thing that actually decides the answer: a
// graph whose services all declare a zero boarding wait charges none, whatever
// it is called.
//
// This is a deliberate behaviour change, and the only one in the migration. The
// API compiles ca-hsr with a non-zero wait_secs and then skipped it by name, so
// that scenario's egress budgets are now smaller by the boarding wait and its
// wait_model reads headway_over_2_peak rather than none. Restoring the old
// numbers is an API-side change — compile ca-hsr with a wait_secs of zero, so
// the graph states the intent the slug used to imply — not something this
// worker can infer from what it is sent.
func (c *chainImpl) waitModel() string {
	if c.graph.HasWait() {
		return waitModelHeadway
	}
	return waitModelNone
}

// injectProperties merges props into a GeoJSON feature's properties object,
// rewriting only that key and leaving the geometry bytes untouched.
func injectProperties(feature json.RawMessage, props map[string]any) (json.RawMessage, error) {
	var f map[string]json.RawMessage
	if err := json.Unmarshal(feature, &f); err != nil {
		return nil, err
	}

	existing := make(map[string]any)
	if raw, ok := f["properties"]; ok && string(raw) != "null" {
		var parsed map[string]any
		if err := json.Unmarshal(raw, &parsed); err == nil {
			existing = parsed
		}
	}

	for k, v := range props {
		existing[k] = v
	}

	propsBytes, err := json.Marshal(existing)
	if err != nil {
		return nil, err
	}
	f["properties"] = propsBytes

	return json.Marshal(f)
}
