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
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/metrics"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

const minIsoContourSecs = 60

const (
	waitModelHeadway = "headway_over_2_peak"
	waitModelNone    = "none"
)

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

const transitAccessReachKm = 80

func modeToCosting(m transit.TravelMode) (valhalla.Costing, error) {
	spec, ok := modes[m]
	if !ok {
		return "", ErrInvalidMode
	}
	return spec.costing, nil
}

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
	metrics *metrics.Set
}

func New(routingClient valhalla.Client, graph transit.GraphData, limits valhalla.Limits, cache EgressCache, log *logger.Logger, metricSet *metrics.Set) Chainer {
	return &chainImpl{routing: routingClient, graph: graph, limits: limits, cache: cache, log: log, metrics: metricSet}
}

func (c *chainImpl) approxAccessReachKm(mode transit.TravelMode, costing valhalla.Costing, budgetMins int) float64 {
	spec := modes[mode]
	budgetReachKm := spec.speedKmH * float64(budgetMins) / 60.0 / geo.DetourFactor
	capKm := c.limits.BLineLimitKm(costing)
	if spec.maxReachKm > 0 {
		capKm = spec.maxReachKm
	}
	return math.Min(budgetReachKm, capKm)
}

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

	egressCandidates, reached := c.bestEgressPerNode(req, nodes, accessSecs)
	c.log.Debug(ctx, "chain: egress candidates", "count", len(egressCandidates))

	progress, omittedProgress := tripProgress(nodes, reached, req.BudgetMins*60)
	if omittedProgress > 0 {
		c.log.Info(ctx, "chain: trip progress omitted, hops carry no corridor",
			"compile_job_id", req.CompileJobID,
			"omitted_legs", omittedProgress)
	}

	egressIsos, starterWalk, droppedEgress, err := c.egressAndStarterWalk(ctx, req, costing, egressCandidates)
	if err != nil {
		return nil, err
	}

	return c.assemble(ctx, req, originIso, originIsoClamped, egressCandidates, egressIsos, starterWalk, progress, droppedEgress)
}

func (c *chainImpl) egressAndStarterWalk(
	ctx context.Context,
	req ChainRequest,
	costing valhalla.Costing,
	candidates []egressCandidate,
) ([]*valhalla.IsochroneResponse, *StarterWalk, []string, error) {
	var (
		egressIsos    []*valhalla.IsochroneResponse
		starterWalk   *StarterWalk
		droppedEgress []string
	)

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		isos, dropped, err := c.egressIsochrones(gctx, req, costing, candidates)
		if err != nil {
			return err
		}
		egressIsos = isos
		droppedEgress = dropped
		return nil
	})

	if starter := pickStarter(candidates); starter != nil {
		g.Go(func() error {
			starterWalk = c.starterWalk(gctx, req, costing, *starter)
			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, nil, nil, err
	}
	return egressIsos, starterWalk, droppedEgress, nil
}

func pickStarter(candidates []egressCandidate) *egressCandidate {
	var starter *egressCandidate
	for i := range candidates {
		ec := &candidates[i]
		if ec.viaTransit {
			continue
		}
		// Seconds, not accessMins: truncating to whole minutes made every
		// station in the same minute a tie, and the slug tie-break then
		// silently preferred one over another up to 59 seconds closer.
		if starter == nil ||
			ec.accessSecs < starter.accessSecs ||
			(ec.accessSecs == starter.accessSecs && ec.node.Slug < starter.node.Slug) {
			starter = ec
		}
	}
	return starter
}

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

func (c *chainImpl) nearbyNodes(ctx context.Context, req ChainRequest, costing valhalla.Costing, nodes []transit.Node) []transit.Node {
	reachKm := c.approxAccessReachKm(req.Mode, costing, req.BudgetMins)

	var nearby []transit.Node
	for _, n := range nodes {
		if geo.HaversineKm(req.Lat, req.Lng, n.Lat, n.Lng) <= reachKm {
			nearby = append(nearby, n)
		}
	}
	c.log.Debug(ctx, "chain: nodes within haversine reach", "reachable", len(nearby), "total_nodes", len(nodes), "reach_km", reachKm)

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
	nRode := rode.Load()
	c.metrics.TransitAccess(true, int(nRode))
	c.metrics.TransitAccess(false, int(routed.Load()-nRode))

	c.log.Info(ctx, "chain: transit access legs routed",
		"compile_job_id", req.CompileJobID,
		"asked", len(nearbyNodes),
		"routed", routed.Load(),
		"rode_transit", rode.Load(),
		"departs_at", req.DepartAt.Format(time.RFC3339))

	return &valhalla.MatrixResponse{SourcesToTargets: [][]valhalla.MatrixCell{cells}}, nil
}

const snapSlackKm = 2 * float64(valhalla.LocationRadiusM) / 1000.0

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

const maxLoggedSlugs = 20

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
	journey       transit.Reached
	legs          []Leg
	viaTransit    bool
}

func (c *chainImpl) bestEgressPerNode(
	req ChainRequest,
	nodes []transit.Node,
	accessSecs map[string]int,
) ([]egressCandidate, map[string]transit.Reached) {
	budgetSecs := req.BudgetMins * 60
	reached := c.graph.Search(accessSecs)

	var candidates []egressCandidate
	for _, n := range nodes {
		journey, ok := reached[n.Slug]
		if !ok {
			continue
		}
		if !withinBudget(journey, budgetSecs) {
			continue
		}
		rSecs := budgetSecs - journey.TotalSecs
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
	return candidates, reached
}

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

func (ec egressCandidate) egressDepartAt(chainDepartAt time.Time) time.Time {
	return chainDepartAt.Add(time.Duration(ec.journey.TotalSecs) * time.Second)
}

func serviceDate(costing valhalla.Costing, departAt time.Time) string {
	if costing != valhalla.CostingMultimodal {
		return ""
	}
	return valhalla.ServiceDate(departAt)
}

func departureBudget(costing valhalla.Costing, budgetMins int) int {
	// Below the contour ceiling, budget and contour together fix the egress
	// departure to the minute: the journey to the station is budget minus
	// contour, and the chain always departs at the same clock. Without it two
	// budgets whose journeys differ by their difference in budget shared a
	// row cut for a departure that far apart (SPA-326). Only a timetable
	// makes the departure matter, so every other mode keys on zero — NULL —
	// and keeps one row per contour, exactly as serviceDate does.
	if costing != valhalla.CostingMultimodal {
		return 0
	}
	return budgetMins
}

func (c *chainImpl) egressIsochrones(
	ctx context.Context,
	req ChainRequest,
	costing valhalla.Costing,
	candidates []egressCandidate,
) ([]*valhalla.IsochroneResponse, []string, error) {
	isos := make([]*valhalla.IsochroneResponse, len(candidates))
	if len(candidates) == 0 {
		return isos, nil, nil
	}

	var served []int
	for i, ec := range candidates {
		if ec.viaTransit && ec.remainingSecs >= minIsoContourSecs {
			served = append(served, i)
		}
	}
	if len(served) == 0 {
		return isos, nil, nil
	}

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
			BudgetMins:   departureBudget(costing, req.BudgetMins),
		}
	}

	hits := c.cache.lookup(ctx, keys, c.log)

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
		return isos, nil, nil
	}

	concurrency := c.limits.MaxConcurrency
	if concurrency <= 0 {
		concurrency = valhalla.DefaultMaxConcurrency
	}

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	droppedAt := make([]string, len(served))
	dropErrs := make([]error, len(served))
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
				// 400 and timeout/5xx drop that polygon; rate limit and
				// cancel fail the chain; if every egress miss failed
				// transiently and nothing was cached, fail the chain.
				// Rate limit first: wrapping isoErr keeps ErrRoutingRateLimit.
				// Then gctx.Err(), not isoErr — a sibling/parent cancel plus
				// an in-flight 400 would otherwise become ErrRoutingRejected
				// (permanent). wrapRoutingErr(context.Canceled) is
				// ErrRoutingUnavailable (transient).
				if errors.Is(isoErr, valhalla.ErrRateLimit) {
					return wrapRoutingErr(isoErr)
				}
				if gctx.Err() != nil {
					return wrapRoutingErr(gctx.Err())
				}
				slug := candidates[i].node.Slug
				c.log.Warn(gctx, "chain: egress isochrone failed, dropping station polygon",
					"station_slug", slug, "err", isoErr.Error())
				droppedAt[j] = slug
				dropErrs[j] = isoErr
				return nil
			}
			subIsos[j] = iso
			return nil
		})
	}

	err := g.Wait()
	c.cache.record(ctx, keys, subIsos, misses, c.log)
	if err != nil {
		return nil, nil, err
	}

	var dropped []string
	var transient error
	for j, slug := range droppedAt {
		if slug == "" {
			continue
		}
		dropped = append(dropped, slug)
		if transient == nil && !errors.Is(dropErrs[j], valhalla.ErrBadRequest) {
			transient = dropErrs[j]
		}
	}
	sort.Strings(dropped)
	c.logDropped(ctx, req, "chain: egress isochrone failed, dropping those stations' polygons", dropped)

	if transient != nil {
		ok := false
		for j, iso := range subIsos {
			if ok, _, _ = usable(iso, keys[j]); ok {
				break
			}
		}
		if !ok {
			return nil, nil, wrapRoutingErr(transient)
		}
	}

	return isos, dropped, nil
}

func (c *chainImpl) assemble(
	ctx context.Context,
	req ChainRequest,
	originIso *valhalla.IsochroneResponse,
	originIsoClamped bool,
	candidates []egressCandidate,
	egressIsos []*valhalla.IsochroneResponse,
	starterWalk *StarterWalk,
	progress []TripProgress,
	droppedEgress []string,
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
		"reachable_stations", len(reachableStations),
		"dropped_egress", len(droppedEgress))

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

			OriginIsoAvailable: originIso != nil,
			StarterWalk:        starterWalk,
			TripProgress:       progress,
			DroppedEgress:      droppedEgress,
		},
	}, nil
}

func (c *chainImpl) waitModel() string {
	if c.graph.HasWait() {
		return waitModelHeadway
	}
	return waitModelNone
}

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
