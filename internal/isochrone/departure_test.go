package isochrone_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/isochrone"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

// departure is the moment every case below sets out at: a Wednesday morning in
// Pacific time, stated rather than taken from the clock so that what the chain
// stamps on each request is a fact the test can name.
func departure(t *testing.T) time.Time {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load America/Los_Angeles: %v", err)
	}
	return time.Date(2026, 9, 2, 8, 0, 0, 0, loc)
}

// departingAt is a chain request with a stated departure.
func departingAt(budgetMins int, mode transit.TravelMode, at time.Time) isochrone.ChainRequest {
	r := req(budgetMins, mode)
	r.DepartAt = at
	return r
}

// hubOnlyAccess answers the pairwise access /route for the hub of
// viaTransitGraph and declines every other station, so station-a and station-b
// are reached only by riding out of the hub — which is the only way a candidate
// gets an egress polygon, and therefore a cache row, at all (SPA-188).
func hubOnlyAccess(accessSecs int) *valhalla.StubClient {
	hub := viaTransitGraph().nodes[0]
	return &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteFn: func(r valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
			if r.Destination.Lat != hub.Lat || r.Destination.Lng != hub.Lng {
				return nil, fmt.Errorf("%w: no matching transit service near this location", valhalla.ErrBadRequest)
			}
			return routedAccess(accessSecs), nil
		},
	}
}

// One chain, one departure.
//
// The value used to be recomputed from time.Now() inside the routing client, at
// every call — so a chain straddling UTC midnight drew its origin polygon on
// one service date and its egress polygons on the next, and no two calls of one
// chain were guaranteed to agree on when the rider set out (SPA-274).
func TestChainer_oneChainUsesOneDeparture(t *testing.T) {
	at := departure(t)
	fc := hubOnlyAccess(600)

	if _, err := newChainer(fc, viaTransitGraph()).Chain(
		context.Background(), departingAt(90, transit.TravelModeTransit, at)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	// The origin polygon and every access leg leave when the rider does. Egress
	// polygons deliberately do not, and are asserted on their own below.
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

// A caller with no opinion still gets one departure for the whole chain,
// resolved once rather than per call.
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

// Egress is charged from the arrival clock, not the origin's.
//
// A rider leaving at 08:00 who spends an hour walking to a station and riding
// out of it is standing at that station at 09:00, and what they can reach from
// there is whatever is running at 09:00. Stamped with the origin's departure,
// every egress polygon was drawn at the morning peak however long the journey
// took — compiled Local is 5 h 06 m SF→Anaheim, so the rider stepped off in the
// early afternoon and was handed a peak-service reach (SPA-272 defect B).
func TestChainer_egressDepartsAfterTheAccessLegAndRide(t *testing.T) {
	at := departure(t)
	const accessSecs = 600
	fc := hubOnlyAccess(accessSecs)

	resp, err := newChainer(fc, viaTransitGraph()).Chain(
		context.Background(), departingAt(90, transit.TravelModeTransit, at))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}

	// viaTransitGraph rides 60 s from the hub to station-a and 300 s to
	// station-b, on top of the access leg to the hub. Those totals are what the
	// response reports, so the expected departures are read off it rather than
	// restated here.
	stations := stationsBySlug(t, resp)
	wantByStation := map[string]time.Time{}
	for slug, s := range stations {
		if s.PredecessorSlug == "" {
			continue // walked to, so it gets no egress polygon at all
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

	// Egress calls are every isochrone but the origin's, matched to a station
	// by the position they were centred on.
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

// A transit row is keyed by the service date it was computed for, because the
// polygon is a function of that day's timetable. Without it, the first request
// against a compile job froze that day's answer for as long as the job lived —
// which, for the seeded corridor, is indefinitely (SPA-269).
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
		// The date is the egress request's own, which is the origin's departure
		// plus the journey — the same day here, since nothing in this fixture
		// rides past midnight.
		if e.Key.DepartsOn != "2026-09-02" {
			t.Errorf("%s DepartsOn = %q, want 2026-09-02", e.Key.StationSlug, e.Key.DepartsOn)
		}
	}
}

// Walk, bike and drive have no timetable, so their rows carry no service date
// and keep one row per (graph, station, mode, contour). The column is NULL for
// them, which is what the cache's NULLS NOT DISTINCT uniqueness relies on.
func TestChainer_nonTransitCacheKeysCarryNoServiceDate(t *testing.T) {
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
	}
}

// Two chains over one graph and one station at two service dates are two rows,
// and the second does not serve the first.
//
// This is the whole point of the date being in the key. The polygon a station
// reaches on a Wednesday is not the polygon it reaches on a Thursday whose
// timetable differs — and after a GTFS calendar rolls over, it is not even
// close.
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

	// A second chain that hit the first day's rows would have made only the one
	// origin call it can never reuse.
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

// The date on the key is the date on the request. A key computed from the
// clock beside a request stamped from something else is a key that can
// disagree with the polygon it names.
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
