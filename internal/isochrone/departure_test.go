package isochrone_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/isochrone"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

func departure(t *testing.T) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load America/Los_Angeles: %v", err)
	}
	return time.Date(2026, 9, 2, 8, 0, 0, 0, loc)
}

func departingAt(budgetMins int, mode transit.TravelMode, at time.Time) isochrone.ChainRequest {
	r := req(budgetMins, mode)
	r.DepartAt = at
	return r
}

func hubOnlyAccess(accessSecs int) *valhalla.StubClient {
	hub := viaTransitGraph().nodes[0]
	return &valhalla.StubClient{
		IsochroneResp: transitShapedIso(),
		RouteFn: func(r valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
			if r.Destination.Lat != hub.Lat || r.Destination.Lng != hub.Lng {
				return nil, fmt.Errorf("%w: no matching transit service near this location", valhalla.ErrBadRequest)
			}
			return routedAccess(accessSecs), nil
		},
	}
}

func TestChainer_oneChainUsesOneDeparture(t *testing.T) {
	at := departure(t)
	fc := hubOnlyAccess(600)

	if _, err := newChainer(fc, viaTransitGraph()).Chain(
		context.Background(), departingAt(90, transit.TravelModeTransit, at)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	iso, _ := fc.Calls()
	if len(iso) == 0 {
		t.Fatal("no isochrone calls")
	}
	if !iso[0].DepartAt.Equal(at) {
		t.Errorf("origin isochrone DepartAt = %s, want %s", iso[0].DepartAt, at)
	}
	routes := fc.RouteCalls()
	if len(routes) == 0 {
		t.Fatal("no access routes")
	}
	for _, r := range routes {
		if !r.DepartAt.Equal(at) {
			t.Errorf("access route to %v DepartAt = %s, want %s", r.Destination, r.DepartAt, at)
		}
	}
}

func TestChainer_defaultDepartureIsResolvedOncePerChain(t *testing.T) {
	fc := hubOnlyAccess(600)

	if _, err := newChainer(fc, viaTransitGraph()).Chain(
		context.Background(), req(90, transit.TravelModeTransit)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	iso, _ := fc.Calls()
	if len(iso) == 0 {
		t.Fatal("no isochrone calls")
	}
	origin := iso[0].DepartAt
	if origin.IsZero() {
		t.Fatal("origin isochrone carried no departure; multimodal costing needs one")
	}
	if got := origin.Format("15:04"); got != "08:00" {
		t.Errorf("default departure = %s, want the 08:00 weekday morning", got)
	}
	for _, r := range fc.RouteCalls() {
		if !r.DepartAt.Equal(origin) {
			t.Errorf("access route departure %s disagrees with the origin's %s", r.DepartAt, origin)
		}
	}
}

func TestChainer_egressDepartsAfterTheAccessLegAndRide(t *testing.T) {
	at := departure(t)
	const accessSecs = 600
	fc := hubOnlyAccess(accessSecs)

	resp, err := newChainer(fc, viaTransitGraph()).Chain(
		context.Background(), departingAt(90, transit.TravelModeTransit, at))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}

	stations := stationsBySlug(t, resp)
	wantByStation := map[string]time.Time{}
	for slug, s := range stations {
		if s.PredecessorSlug == "" {
			continue
		}
		spent := time.Duration(90*60-s.RemainingSecs) * time.Second
		wantByStation[slug] = at.Add(spent)
	}
	if len(wantByStation) != 2 {
		t.Fatalf("expected two stations reached by riding, got %d", len(wantByStation))
	}

	iso, _ := fc.Calls()
	if len(iso) < 3 {
		t.Fatalf("isochrone calls = %d, want the origin plus one per station ridden to", len(iso))
	}

	byPosition := map[valhalla.LatLng]time.Time{}
	for _, call := range iso[1:] {
		byPosition[call.Origin] = call.DepartAt
	}
	for slug, want := range wantByStation {
		node := nodeBySlug(t, viaTransitGraph(), slug)
		got, ok := byPosition[valhalla.LatLng{Lat: node.Lat, Lng: node.Lng}]
		if !ok {
			t.Errorf("no egress isochrone centred on %s", slug)
			continue
		}
		if !got.Equal(want) {
			t.Errorf("%s egress DepartAt = %s, want %s (origin departure plus the journey)",
				slug, got.Format(time.RFC3339), want.Format(time.RFC3339))
		}
		if got.Equal(at) {
			t.Errorf("%s egress still departs at the origin's clock", slug)
		}
	}
}

func nodeBySlug(t *testing.T, graph transit.GraphData, slug string) transit.Node {
	t.Helper()
	for _, n := range graph.Nodes() {
		if n.Slug == slug {
			return n
		}
	}
	t.Fatalf("no node %q in the graph", slug)
	return transit.Node{}
}

func TestChainer_transitCacheKeysCarryTheServiceDate(t *testing.T) {
	at := departure(t)
	cache := newFakeCache()
	chainer := newCachingChainer(hubOnlyAccess(600), viaTransitGraph(), isochrone.EgressCache{Store: cache})

	if _, err := chainer.Chain(context.Background(), departingAt(90, transit.TravelModeTransit, at)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	written := cache.written()
	if len(written) == 0 {
		t.Fatal("nothing cached")
	}
	for _, e := range written {
		if e.Key.DepartsOn == "" {
			t.Errorf("%s cached with no service date", e.Key.StationSlug)
		}

		if e.Key.DepartsOn != "2026-09-02" {
			t.Errorf("%s DepartsOn = %q, want 2026-09-02", e.Key.StationSlug, e.Key.DepartsOn)
		}
	}
}

func TestChainer_transitCacheKeysCarryTheBudget(t *testing.T) {
	cache := newFakeCache()
	chainer := newCachingChainer(hubOnlyAccess(600), viaTransitGraph(), isochrone.EgressCache{Store: cache})

	if _, err := chainer.Chain(context.Background(), departingAt(90, transit.TravelModeTransit, departure(t))); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	written := cache.written()
	if len(written) == 0 {
		t.Fatal("nothing cached")
	}
	for _, e := range written {
		if e.Key.BudgetMins != 90 {
			t.Errorf("%s BudgetMins = %d, want 90", e.Key.StationSlug, e.Key.BudgetMins)
		}
	}
}

func TestChainer_nonTransitCacheKeysCarryNoServiceDateOrBudget(t *testing.T) {
	cache := newFakeCache()
	chainer := newCachingChainer(twoReachableStations(), viaTransitGraph(), isochrone.EgressCache{Store: cache})

	if _, err := chainer.Chain(context.Background(), departingAt(90, transit.TravelModeWalk, departure(t))); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	written := cache.written()
	if len(written) == 0 {
		t.Fatal("nothing cached")
	}
	for _, e := range written {
		if e.Key.DepartsOn != "" {
			t.Errorf("%s walk row keyed by service date %q", e.Key.StationSlug, e.Key.DepartsOn)
		}
		if e.Key.BudgetMins != 0 {
			t.Errorf("%s walk row keyed by budget %d; walk polygons take no departure", e.Key.StationSlug, e.Key.BudgetMins)
		}
	}
}

func TestChainer_twoServiceDatesAreTwoRows(t *testing.T) {
	cache := newFakeCache()
	egress := isochrone.EgressCache{Store: cache}
	graph := viaTransitGraph()
	wednesday := departure(t)
	thursday := wednesday.AddDate(0, 0, 1)

	first := hubOnlyAccess(600)
	if _, err := newCachingChainer(first, graph, egress).Chain(
		context.Background(), departingAt(90, transit.TravelModeTransit, wednesday)); err != nil {
		t.Fatalf("first Chain: %v", err)
	}
	rowsAfterFirst := len(cache.written())
	if rowsAfterFirst == 0 {
		t.Fatal("the first chain cached nothing")
	}

	second := hubOnlyAccess(600)
	if _, err := newCachingChainer(second, graph, egress).Chain(
		context.Background(), departingAt(90, transit.TravelModeTransit, thursday)); err != nil {
		t.Fatalf("second Chain: %v", err)
	}

	if got := isochroneCallCount(t, second); got <= 1 {
		t.Errorf("second chain made %d isochrone calls; it served Wednesday's polygons for Thursday", got)
	}
	if got := len(cache.written()); got != rowsAfterFirst*2 {
		t.Errorf("cache holds %d rows, want %d — one generation per service date", got, rowsAfterFirst*2)
	}

	dates := map[string]int{}
	for _, e := range cache.written() {
		dates[e.Key.DepartsOn]++
	}
	if len(dates) != 2 {
		t.Errorf("service dates in the cache: %v, want two", dates)
	}
}

func TestChainer_cachedDateMatchesTheRequestItDescribes(t *testing.T) {
	at := departure(t)
	cache := newFakeCache()
	fc := hubOnlyAccess(600)

	if _, err := newCachingChainer(fc, viaTransitGraph(), isochrone.EgressCache{Store: cache}).Chain(
		context.Background(), departingAt(90, transit.TravelModeTransit, at)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	byPosition := map[valhalla.LatLng]string{}
	iso, _ := fc.Calls()
	for _, call := range iso[1:] {
		byPosition[call.Origin] = valhalla.ServiceDate(call.DepartAt)
	}

	for _, e := range cache.written() {
		node := nodeBySlug(t, viaTransitGraph(), e.Key.StationSlug)
		want, ok := byPosition[valhalla.LatLng{Lat: node.Lat, Lng: node.Lng}]
		if !ok {
			t.Errorf("cached %s but never asked Valhalla for it", e.Key.StationSlug)
			continue
		}
		if e.Key.DepartsOn != want {
			t.Errorf("%s keyed on %q but requested for %q", e.Key.StationSlug, e.Key.DepartsOn, want)
		}
	}
}

func TestChainer_twoBudgetsAtOneContourAreTwoRows(t *testing.T) {
	// SPA-326's repro. Budget 90 riding from a 10-minute access leg and budget
	// 120 riding from a 40-minute one both reach station-a with 79 minutes
	// left, so they share a contour, a station and a date — but the second
	// rider is on the platform 30 minutes later. Budget in the key is what
	// stops the second chain being served the first one's timetable.
	at := departure(t)
	cache := newFakeCache()
	egress := isochrone.EgressCache{Store: cache}
	graph := viaTransitGraph()
	stationA := nodeBySlug(t, graph, "station-a")
	egressDepartureAtA := func(fc *valhalla.StubClient) (time.Time, bool) {
		iso, _ := fc.Calls()
		for _, call := range iso[1:] {
			if call.Origin == (valhalla.LatLng{Lat: stationA.Lat, Lng: stationA.Lng}) {
				return call.DepartAt, true
			}
		}
		return time.Time{}, false
	}

	cold := hubOnlyAccess(600)
	if _, err := newCachingChainer(cold, graph, egress).Chain(
		context.Background(), departingAt(90, transit.TravelModeTransit, at)); err != nil {
		t.Fatalf("cold Chain: %v", err)
	}
	coldDepart, ok := egressDepartureAtA(cold)
	if !ok {
		t.Fatal("the cold chain never asked for station-a's egress")
	}

	warm := hubOnlyAccess(2400)
	if _, err := newCachingChainer(warm, graph, egress).Chain(
		context.Background(), departingAt(120, transit.TravelModeTransit, at)); err != nil {
		t.Fatalf("warm Chain: %v", err)
	}
	warmDepart, ok := egressDepartureAtA(warm)
	if !ok {
		t.Fatal("budget 120 was served budget 90's station-a polygon; it never asked Valhalla for its own departure")
	}
	if got := warmDepart.Sub(coldDepart); got != 30*time.Minute {
		t.Errorf("the two egress departures are %s apart, want 30m — the repro no longer exercises the collision", got)
	}

	var rows []store.IsochroneKey
	for _, e := range cache.written() {
		if e.Key.StationSlug == "station-a" {
			rows = append(rows, e.Key)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("station-a rows = %+v, want one per budget", rows)
	}
	if rows[0].ContourMins != rows[1].ContourMins || rows[0].DepartsOn != rows[1].DepartsOn {
		t.Errorf("rows %+v and %+v differ by more than budget; the repro needs one contour and one date", rows[0], rows[1])
	}
	if rows[0].BudgetMins == rows[1].BudgetMins {
		t.Errorf("both rows keyed on budget %d", rows[0].BudgetMins)
	}
}

func TestChainer_twoBudgetsShareWalkRows(t *testing.T) {
	// The other half of SPA-326: walk takes no departure, so a different
	// budget reaching the same contour must still hit the row it left.
	cache := newFakeCache()
	egress := isochrone.EgressCache{Store: cache}

	if _, err := newCachingChainer(twoReachableStations(), viaTransitGraph(), egress).
		Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("cold Chain: %v", err)
	}

	warm := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{valhalla.Cell(2400, tokenRoadKm), {}, {}}},
		},
	}
	if _, err := newCachingChainer(warm, viaTransitGraph(), egress).
		Chain(context.Background(), req(120, transit.TravelModeWalk)); err != nil {
		t.Fatalf("warm Chain: %v", err)
	}
	if got := isochroneCallCount(t, warm); got != 1 {
		t.Errorf("warm chain made %d isochrone calls, want 1 (the origin) — walk rows must not fragment by budget", got)
	}
}
