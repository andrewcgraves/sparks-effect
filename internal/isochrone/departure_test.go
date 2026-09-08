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
		IsochroneResp: cannedIso(),
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
