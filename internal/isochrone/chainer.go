package isochrone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"

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
// needs to know about it: the costing to ask Valhalla for, and the speed to
// size the destination pre-filter with. One table rather than a switch apiece
// so a fourth mode cannot be added to some of them and forgotten in the rest.
var modes = map[transit.TravelMode]struct {
	costing  valhalla.Costing
	speedKmH float64
}{
	transit.TravelModeWalk:  {valhalla.CostingPedestrian, geo.WalkSpeedKmH},
	transit.TravelModeBike:  {valhalla.CostingBicycle, geo.BikeSpeedKmH},
	transit.TravelModeDrive: {valhalla.CostingAuto, geo.DriveSpeedKmH},
}

func modeSpeedKmH(mode transit.TravelMode) float64 {
	return modes[mode].speedKmH
}

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
	budgetReachKm := modeSpeedKmH(mode) * float64(budgetMins) / 60.0 / geo.DetourFactor
	return math.Min(budgetReachKm, c.limits.BLineLimitKm(costing))
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

	c.log.Debugf("chain: lat=%.6f lng=%.6f budget_mins=%d mode=%s compile_job=%s",
		req.Lat, req.Lng, req.BudgetMins, req.Mode, req.CompileJobID)

	nodes := c.graph.Nodes()
	c.log.Debugf("chain: %d nodes in graph", len(nodes))

	nearbyNodes := c.nearbyNodes(req, costing, nodes)

	originContourSecs := c.clampToContourCeilingSecs(req.BudgetMins * 60)
	originIsoClamped := originContourSecs < req.BudgetMins*60

	originIso, matrixResp, err := c.originAndMatrix(ctx, req, costing, originContourSecs, nearbyNodes)
	if err != nil {
		return nil, err
	}
	c.log.Debugf("chain: routing calls done (origin iso + matrix)")

	accessSecs := make(map[string]int)
	if matrixResp != nil && len(matrixResp.SourcesToTargets) > 0 {
		row := matrixResp.SourcesToTargets[0]
		for i, n := range nearbyNodes {
			if i < len(row) && row[i].Time >= 0 {
				accessSecs[n.Slug] = row[i].Time
			}
		}
	}
	c.log.Debugf("chain: matrix done, %d/%d nodes reachable", len(accessSecs), len(nodes))

	egressCandidates := c.bestEgressPerNode(req, nodes, accessSecs)
	c.log.Debugf("chain: egress fan-out %d candidates", len(egressCandidates))

	egressIsos, err := c.egressIsochrones(ctx, req, costing, egressCandidates)
	if err != nil {
		return nil, err
	}

	return c.assemble(req, originIso, originIsoClamped, egressCandidates, egressIsos)
}

// nearbyNodes narrows the graph to the stations worth putting in the matrix:
// those within reach, and no more of them than the matrix will accept.
func (c *chainImpl) nearbyNodes(req ChainRequest, costing valhalla.Costing, nodes []transit.Node) []transit.Node {
	reachKm := c.approxAccessReachKm(req.Mode, costing, req.BudgetMins)

	var nearby []transit.Node
	for _, n := range nodes {
		if geo.HaversineKm(req.Lat, req.Lng, n.Lat, n.Lng) <= reachKm {
			nearby = append(nearby, n)
		}
	}
	c.log.Debugf("chain: %d/%d nodes within haversine reach (%.1f km)", len(nearby), len(nodes), reachKm)

	// One source, so the pair limit is a destination limit. Truncating to the
	// nearest is the least-bad way to lose stations: the ones dropped are the
	// ones with the least access budget left, hence the smallest egress
	// polygons.
	if max := c.limits.MaxMatrixPairs; max > 0 && len(nearby) > max {
		sort.Slice(nearby, func(i, j int) bool {
			di := geo.HaversineKm(req.Lat, req.Lng, nearby[i].Lat, nearby[i].Lng)
			dj := geo.HaversineKm(req.Lat, req.Lng, nearby[j].Lat, nearby[j].Lng)
			return di < dj
		})
		nearby = nearby[:max]
		c.log.Debugf("chain: truncated to %d nodes (matrix pair cap)", max)
	}
	return nearby
}

// originAndMatrix fetches the origin polygon and the access-time matrix
// together. They do not depend on each other, and the matrix is the slower of
// the two, so serialising them would add the origin call's latency to every
// request for nothing.
func (c *chainImpl) originAndMatrix(
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
		})
		if isoErr != nil {
			return wrapRoutingErr(isoErr)
		}
		originIso = iso
		return nil
	})

	if len(nearbyNodes) > 0 {
		g.Go(func() error {
			dests := make([]valhalla.LatLng, len(nearbyNodes))
			for i, n := range nearbyNodes {
				dests[i] = valhalla.LatLng{Lat: n.Lat, Lng: n.Lng}
			}
			m, mErr := c.routing.Matrix(gctx, valhalla.MatrixRequest{
				Origins:      []valhalla.LatLng{{Lat: req.Lat, Lng: req.Lng}},
				Destinations: dests,
				Costing:      costing,
			})
			if mErr != nil {
				return wrapRoutingErr(mErr)
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

type egressCandidate struct {
	node          transit.Node
	remainingMins int
	accessMins    int
	serviceID     string
	// viaTransit reports whether reaching this station's best path boards a
	// service, as opposed to walking straight to it from the origin. Only a
	// via-transit candidate gets an egress polygon computed for it — see
	// egressIsochrones.
	viaTransit bool
}

// bestEgressPerNode works out, for every node in the graph, the access station
// that leaves the most budget on arrival there.
//
// Every reachable access station is tried against every node rather than
// picking one nearest station and riding from it: the nearest station may be on
// a slow service, and a station two minutes further away on an express reaches
// more. Ties go to whichever leaves more budget, which is the whole point.
//
// A node whose best remaining budget is under a minute is dropped rather than
// carried, because a sub-minute contour is a request Valhalla rejects.
func (c *chainImpl) bestEgressPerNode(
	req ChainRequest,
	nodes []transit.Node,
	accessSecs map[string]int,
) []egressCandidate {
	budgetSecs := req.BudgetMins * 60

	type pathResult struct {
		accessMins    int
		remainingMins int
		serviceID     string
		viaTransit    bool
	}

	bestPaths := make(map[string]pathResult, len(nodes))
	for _, egress := range nodes {
		var best *pathResult
		bestRSecs := 0
		for accessSlug, aSecs := range accessSecs {
			var rSecs int
			var serviceID string
			viaTransit := accessSlug != egress.Slug
			if !viaTransit {
				rSecs = budgetSecs - aSecs
			} else {
				transitSecs, transitWait, transitService, ok := c.graph.TravelTimeBetween(accessSlug, egress.Slug)
				if !ok {
					continue
				}
				rSecs = budgetSecs - aSecs - transitSecs - transitWait
				serviceID = transitService
			}
			if rSecs >= minIsoContourSecs && (best == nil || rSecs > bestRSecs) {
				bestRSecs = rSecs
				p := pathResult{accessMins: aSecs / 60, remainingMins: rSecs / 60, serviceID: serviceID, viaTransit: viaTransit}
				best = &p
			}
		}
		if best != nil {
			bestPaths[egress.Slug] = *best
		}
	}

	// Built by walking nodes rather than the map so the fan-out, the features
	// and the reported stations all come out in graph order, which is stable
	// across runs. Ranging the map directly would reorder the output on every
	// request.
	var candidates []egressCandidate
	for _, n := range nodes {
		if p, ok := bestPaths[n.Slug]; ok {
			candidates = append(candidates, egressCandidate{
				node:          n,
				remainingMins: p.remainingMins,
				accessMins:    p.accessMins,
				serviceID:     p.serviceID,
				viaTransit:    p.viaTransit,
			})
		}
	}
	return candidates
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
	keys := make([]store.IsochroneKey, len(served))
	for j, i := range served {
		keys[j] = store.IsochroneKey{
			CompileJobID: req.CompileJobID,
			StationSlug:  candidates[i].node.Slug,
			Mode:         string(req.Mode),
			ContourMins:  c.clampToContourCeilingSecs(candidates[i].remainingMins*60) / 60,
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
	c.log.Debugf("chain: egress cache %d hits, %d misses", len(served)-len(misses), len(misses))
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
	req ChainRequest,
	originIso *valhalla.IsochroneResponse,
	originIsoClamped bool,
	candidates []egressCandidate,
	egressIsos []*valhalla.IsochroneResponse,
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
			StationSlug:   ec.node.Slug,
			AccessMins:    ec.accessMins,
			RemainingMins: ec.remainingMins,
			ViaService:    ec.serviceID,
		})
	}

	c.log.Debugf("chain: complete features=%d reachable_stations=%d", len(features), len(reachableStations))

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
