package isochrone_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/isochrone"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

// fakeGraph implements transit.GraphData directly, rather than building a
// TransitGraph and letting CompiledGraph read it.
//
// It carried over from sparks-effect-api, where it stood in for a scenario
// store, and it earns its place here for the same reason it did there: most of
// these cases are about what the chainer *asks Valhalla for*, and stating a
// travel time as one map entry keeps that the subject. transit's own Dijkstra
// is exercised against a real graph in internal/routing, over the golden
// fixture the API actually publishes.
type fakeGraph struct {
	nodes       []transit.Node
	travelTimes map[[2]string]int
	// waitTimes is per-pair boarding wait in seconds, defaulting to zero. A
	// graph with no entry here declares no wait at all, which is what
	// hasWait reports and what the chainer turns into wait_model "none".
	waitTimes map[[2]string]int
}

var _ transit.GraphData = (*fakeGraph)(nil)

func (f *fakeGraph) Nodes() []transit.Node { return f.nodes }

func (f *fakeGraph) TravelTimeBetween(fromSlug, toSlug string) (int, int, string, bool) {
	key := [2]string{fromSlug, toSlug}
	rev := [2]string{toSlug, fromSlug}
	if v, ok := f.travelTimes[key]; ok {
		return v, f.waitTimes[key], "", true
	}
	if v, ok := f.travelTimes[rev]; ok {
		return v, f.waitTimes[rev], "", true
	}
	return 0, 0, "", false
}

func (f *fakeGraph) HasWait() bool {
	for _, w := range f.waitTimes {
		if w > 0 {
			return true
		}
	}
	return false
}

func node(slug string, lng, lat float64) transit.Node {
	return transit.Node{Slug: slug, Lat: lat, Lng: lng}
}

func newTestGraph() *fakeGraph {
	return &fakeGraph{
		nodes: []transit.Node{
			node("station-a", -122.39, 37.71),
			node("station-b", -122.41, 37.69),
		},
		travelTimes: map[[2]string]int{
			{"station-a", "station-b"}: 1800,
		},
	}
}

func cannedIso() *valhalla.IsochroneResponse {
	return &valhalla.IsochroneResponse{
		Type: "FeatureCollection",
		Features: []json.RawMessage{
			json.RawMessage(`{"type":"Feature","geometry":{"type":"Polygon","coordinates":[]},"properties":{}}`),
		},
	}
}

// newChainer builds a chainer at the limits the deployed container enforces,
// caching nothing. Cases that are *about* a limit override the one they
// exercise; cases about reuse live in cache_test.go and build their own.
func newChainer(client valhalla.Client, graph transit.GraphData) isochrone.Chainer {
	return isochrone.New(client, graph, valhalla.DefaultLimits(), isochrone.EgressCache{}, logger.Discard())
}

func req(budgetMins int, mode transit.TravelMode) isochrone.ChainRequest {
	return isochrone.ChainRequest{
		Lat: 37.7, Lng: -122.4, BudgetMins: budgetMins, Mode: mode,
		CompileJobID: "compile-1",
	}
}

func TestChainer_happyPath_twoStations(t *testing.T) {
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{
					{Time: 600, Distance: 1.0},  // station-a: 10 mins
					{Time: 3000, Distance: 5.0}, // station-b: 50 mins
				},
			},
		},
	}
	graph := newTestGraph()
	graph.waitTimes = map[[2]string]int{{"station-a", "station-b"}: 300}
	chainer := newChainer(fc, graph)

	resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if resp.Type != "FeatureCollection" {
		t.Errorf("Type: want FeatureCollection, got %q", resp.Type)
	}

	// 1 origin feature + 2 egress features (one per station)
	if len(resp.Features) != 3 {
		t.Errorf("Features len: want 3, got %d", len(resp.Features))
	}
	if len(resp.Metadata.ReachableStations) != 2 {
		t.Errorf("ReachableStations len: want 2, got %d", len(resp.Metadata.ReachableStations))
	}
	if resp.Metadata.WaitModel != "headway_over_2_peak" {
		t.Errorf("WaitModel: want headway_over_2_peak, got %q", resp.Metadata.WaitModel)
	}
	if resp.Metadata.OriginBudgetMins != 90 {
		t.Errorf("OriginBudgetMins: want 90, got %d", resp.Metadata.OriginBudgetMins)
	}
	// The compile job replaces the scenario slug the API's chainer reported:
	// it is the only identity the queue message carries for a graph.
	if resp.Metadata.CompileJobID != "compile-1" {
		t.Errorf("CompileJobID: want compile-1, got %q", resp.Metadata.CompileJobID)
	}
}

func TestChainer_noStations(t *testing.T) {
	fc := &valhalla.StubClient{IsochroneResp: cannedIso()}
	chainer := newChainer(fc, &fakeGraph{})

	resp, err := chainer.Chain(context.Background(), req(30, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	// Only origin feature
	if len(resp.Features) != 1 {
		t.Errorf("Features len: want 1, got %d", len(resp.Features))
	}
	if len(resp.Metadata.ReachableStations) != 0 {
		t.Errorf("ReachableStations: want 0, got %d", len(resp.Metadata.ReachableStations))
	}
	// An empty graph has nothing to put in a matrix, so no matrix call should
	// go out at all — Valhalla rejects a matrix with no targets.
	if _, matrix := fc.Calls(); len(matrix) != 0 {
		t.Errorf("MatrixCalls: want 0 for an empty graph, got %d", len(matrix))
	}
}

func TestChainer_allUnreachable(t *testing.T) {
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{
					{Time: -1, Distance: 0},
					{Time: -1, Distance: 0},
				},
			},
		},
	}
	chainer := newChainer(fc, newTestGraph())

	resp, err := chainer.Chain(context.Background(), req(30, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if len(resp.Features) != 1 {
		t.Errorf("Features len: want 1 (origin only), got %d", len(resp.Features))
	}
	if len(resp.Metadata.ReachableStations) != 0 {
		t.Errorf("ReachableStations: want 0, got %d", len(resp.Metadata.ReachableStations))
	}
}

func TestChainer_zeroRemainingExcludesStation(t *testing.T) {
	// budget=30 min=1800s, station-a access=600s, transit a→b=1800s: 600+1800=2400 > 1800 budget
	// station-b direct: unreachable (-1)
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{
					{Time: 600, Distance: 1.0}, // station-a: 10 mins
					{Time: -1, Distance: 0},    // station-b: unreachable
				},
			},
		},
	}
	chainer := newChainer(fc, newTestGraph())

	resp, err := chainer.Chain(context.Background(), req(30, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	// origin + station-a egress only; station-b excluded (1800-600-1800 = -600s ≤ 0)
	if len(resp.Metadata.ReachableStations) != 1 {
		t.Fatalf("ReachableStations: want 1, got %d", len(resp.Metadata.ReachableStations))
	}
	if resp.Metadata.ReachableStations[0].StationSlug != "station-a" {
		t.Errorf("station slug: want station-a, got %q", resp.Metadata.ReachableStations[0].StationSlug)
	}
}

func TestChainer_directAccess_AequalsB(t *testing.T) {
	// Single station, directly reachable; no transit leg.
	graph := &fakeGraph{nodes: []transit.Node{node("station-a", -122.39, 37.71)}}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{{Time: 600, Distance: 1.0}}, // 10 mins
			},
		},
	}
	chainer := newChainer(fc, graph)

	resp, err := chainer.Chain(context.Background(), req(60, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if len(resp.Metadata.ReachableStations) != 1 {
		t.Fatalf("ReachableStations: want 1, got %d", len(resp.Metadata.ReachableStations))
	}
	rs := resp.Metadata.ReachableStations[0]
	if rs.StationSlug != "station-a" {
		t.Errorf("slug: want station-a, got %q", rs.StationSlug)
	}
	if rs.RemainingMins != 50 { // 60 - 10
		t.Errorf("remaining: want 50, got %d", rs.RemainingMins)
	}
	// origin feature + egress feature
	if len(resp.Features) != 2 {
		t.Errorf("Features len: want 2, got %d", len(resp.Features))
	}
}

func TestChainer_concurrentFanOut_noRace(t *testing.T) {
	// 5 reachable stations to trigger concurrent egress fan-out.
	nodes := make([]transit.Node, 5)
	matrixCells := make([]valhalla.MatrixCell, 5)
	tt := make(map[[2]string]int)
	for i := range 5 {
		slug := string(rune('a' + i))
		nodes[i] = node("st-"+slug, -122.40+0.005*float64(i), 37.70+0.005*float64(i))
		matrixCells[i] = valhalla.MatrixCell{Time: 600, Distance: 1.0}
		for j := range 5 {
			if i != j {
				tt[[2]string{"st-" + slug, "st-" + string(rune('a'+j))}] = 600
			}
		}
	}

	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp:    &valhalla.MatrixResponse{SourcesToTargets: [][]valhalla.MatrixCell{matrixCells}},
	}
	chainer := newChainer(fc, &fakeGraph{nodes: nodes, travelTimes: tt})

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}
}

// The egress fan-out is as wide as the graph has reachable stations, and the
// routing server is a single small container. Without a bound, one large
// scenario would open hundreds of concurrent requests against a server sized
// for a handful — so the bound is measured, not just configured.
func TestChainer_egressFanOutIsBoundedByConfiguration(t *testing.T) {
	const nodeCount = 40
	const concurrency = 3

	nodes := make([]transit.Node, nodeCount)
	cells := make([]valhalla.MatrixCell, nodeCount)
	for i := range nodeCount {
		nodes[i] = node(fmt.Sprintf("station-%d", i), -122.40+0.001*float64(i), 37.70)
		cells[i] = valhalla.MatrixCell{Time: 600, Distance: 1.0}
	}

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	fc := &valhalla.StubClient{
		MatrixResp: &valhalla.MatrixResponse{SourcesToTargets: [][]valhalla.MatrixCell{cells}},
		IsochroneFn: func(valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()

			// Long enough that genuinely concurrent calls overlap here rather
			// than completing one after another and never revealing the peak.
			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			inFlight--
			mu.Unlock()
			return cannedIso(), nil
		},
	}

	limits := valhalla.DefaultLimits()
	limits.MaxConcurrency = concurrency
	chainer := isochrone.New(fc, &fakeGraph{nodes: nodes}, limits, isochrone.EgressCache{}, logger.Discard())

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if peak > concurrency {
		t.Errorf("peak concurrent routing calls = %d, want at most %d", peak, concurrency)
	}
	// A peak of one would mean the fan-out had become serial, which passes the
	// bound while losing the whole point of running it concurrently.
	if peak < 2 {
		t.Errorf("peak concurrent routing calls = %d; the fan-out is not running concurrently at all", peak)
	}
}

func TestChainer_ErrInvalidMode(t *testing.T) {
	fc := &valhalla.StubClient{}
	chainer := newChainer(fc, newTestGraph())

	_, err := chainer.Chain(context.Background(), req(30, transit.TravelMode("teleport")))
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, isochrone.ErrInvalidMode) {
		t.Errorf("err: want ErrInvalidMode, got %v", err)
	}
	if iso, _ := fc.Calls(); len(iso) != 0 {
		t.Error("expected no routing calls on invalid mode")
	}
}

func TestChainer_haversineFilterExcludesFarStations(t *testing.T) {
	graph := &fakeGraph{nodes: []transit.Node{
		node("station-near", -122.39, 37.71),
		node("station-far", -121.0, 38.5),
	}}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{{Time: 600, Distance: 1.0}}},
		},
	}
	chainer := newChainer(fc, graph)

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}
	_, matrix := fc.Calls()
	if len(matrix) != 1 {
		t.Fatalf("MatrixCalls: want 1, got %d", len(matrix))
	}
	if len(matrix[0].Destinations) != 1 {
		t.Errorf("Destinations: want 1 (far station excluded), got %d", len(matrix[0].Destinations))
	}
}

// A 90-minute drive budget reaches ~86 km of b-line distance, so a station 30 km
// out is a legitimate access candidate. The matrix b-line limit for auto is
// 400 km, not 20 km, so nothing should cut the reach short here (SPA-136).
func TestChainer_driveReachFollowsBudgetNotA20kmFloor(t *testing.T) {
	graph := &fakeGraph{nodes: []transit.Node{
		node("station-near", -122.39, 37.71),
		node("station-30km", -122.06, 37.71),
	}}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{{Time: 600, Distance: 1.0}, {Time: 1800, Distance: 30.0}},
			},
		},
	}
	chainer := newChainer(fc, graph)

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeDrive)); err != nil {
		t.Fatalf("Chain: %v", err)
	}
	_, matrix := fc.Calls()
	if len(matrix) != 1 {
		t.Fatalf("MatrixCalls: want 1, got %d", len(matrix))
	}
	if got := len(matrix[0].Destinations); got != 2 {
		t.Errorf("Destinations: want 2 (30 km station is within a 90-min drive), got %d", got)
	}
}

// Valhalla rejects the whole matrix when any source→target b-line distance
// exceeds max_matrix_distance — 400 km for auto — so the pre-filter must still
// cap reach there however large the budget.
func TestChainer_driveReachCappedAtAutoMatrixBLineLimit(t *testing.T) {
	graph := &fakeGraph{nodes: []transit.Node{
		node("station-near", -122.39, 37.71),
		// ~5.5° of latitude north ≈ 610 km, beyond the 400 km auto cap.
		node("station-610km", -122.40, 43.20),
	}}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{{Time: 600, Distance: 1.0}}},
		},
	}
	chainer := newChainer(fc, graph)

	if _, err := chainer.Chain(context.Background(), req(9999, transit.TravelModeDrive)); err != nil {
		t.Fatalf("Chain: %v", err)
	}
	_, matrix := fc.Calls()
	if len(matrix) != 1 {
		t.Fatalf("MatrixCalls: want 1, got %d", len(matrix))
	}
	if got := len(matrix[0].Destinations); got != 1 {
		t.Errorf("Destinations: want 1 (610 km station beyond 400 km auto b-line limit), got %d", got)
	}
}

// Valhalla enforces max_matrix_location_pairs as sources × targets. With one
// source that is a destination cap, and exceeding it is a 400 for the whole
// request — so the chainer truncates to the nearest rather than finding out.
func TestChainer_matrixPairCap_truncates(t *testing.T) {
	const cap = 600
	const nodeCount = 620

	nodes := make([]transit.Node, nodeCount)
	cells := make([]valhalla.MatrixCell, nodeCount)
	for i := range nodeCount {
		// All within ~1 km of origin — well within walk budget haversine reach.
		nodes[i] = node(
			fmt.Sprintf("station-%d", i),
			-122.40+0.001*float64(i%30),
			37.70+0.001*float64(i/30),
		)
		cells[i] = valhalla.MatrixCell{Time: -1, Distance: 0}
	}

	limits := valhalla.DefaultLimits()
	limits.MaxMatrixPairs = cap

	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp:    &valhalla.MatrixResponse{SourcesToTargets: [][]valhalla.MatrixCell{cells[:cap]}},
	}
	chainer := isochrone.New(fc, &fakeGraph{nodes: nodes}, limits, isochrone.EgressCache{}, logger.Discard())

	if _, err := chainer.Chain(context.Background(), req(9999, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}
	_, matrix := fc.Calls()
	if len(matrix) != 1 {
		t.Fatalf("MatrixCalls: want 1, got %d", len(matrix))
	}
	if got := len(matrix[0].Destinations); got != cap {
		t.Errorf("Matrix destinations: want %d, got %d", cap, got)
	}
}

// The 600-destination cap the API carried was a Stadia plan allowance. Valhalla
// allows 2500 pairs, so a scenario between the two sizes must now go out whole
// rather than being silently cut down to its 600 nearest stations.
func TestChainer_defaultMatrixCapIsValhallasNotStadias(t *testing.T) {
	const nodeCount = 900

	nodes := make([]transit.Node, nodeCount)
	cells := make([]valhalla.MatrixCell, nodeCount)
	for i := range nodeCount {
		nodes[i] = node(
			fmt.Sprintf("station-%d", i),
			-122.40+0.001*float64(i%30),
			37.70+0.001*float64(i/30),
		)
		cells[i] = valhalla.MatrixCell{Time: -1, Distance: 0}
	}

	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp:    &valhalla.MatrixResponse{SourcesToTargets: [][]valhalla.MatrixCell{cells}},
	}
	chainer := newChainer(fc, &fakeGraph{nodes: nodes})

	if _, err := chainer.Chain(context.Background(), req(9999, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}
	_, matrix := fc.Calls()
	if got := len(matrix[0].Destinations); got != nodeCount {
		t.Errorf("Destinations: want all %d (under Valhalla's 2500-pair cap), got %d", nodeCount, got)
	}
}

// SPA-136: a 60-minute drive origin polygon is requested at its full 60 minutes.
// It used to be clamped to 15 min and then dropped entirely, which is what made
// the rendered drive isochrone look far too small.
func TestChainer_drive_largeBudget_originFeatureAtFullBudget(t *testing.T) {
	fc := &valhalla.StubClient{IsochroneResp: cannedIso()}
	chainer := newChainer(fc, &fakeGraph{})

	resp, err := chainer.Chain(context.Background(), req(60, transit.TravelModeDrive))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if !resp.Metadata.OriginIsoAvailable {
		t.Error("OriginIsoAvailable: want true for drive+60min")
	}
	if resp.Metadata.OriginIsoClamped {
		t.Error("OriginIsoClamped: want false — 60 min is under the contour ceiling")
	}
	var originFeatures int
	for _, f := range resp.Features {
		var feat map[string]any
		if err := json.Unmarshal(f, &feat); err != nil {
			t.Fatalf("unmarshal feature: %v", err)
		}
		props, _ := feat["properties"].(map[string]any)
		if props["source"] == "origin" {
			originFeatures++
		}
	}
	if originFeatures != 1 {
		t.Errorf("origin features: want 1, got %d", originFeatures)
	}
	iso, _ := fc.Calls()
	if len(iso) != 1 {
		t.Fatalf("IsochroneCalls: want 1, got %d", len(iso))
	}
	if got := iso[0].BudgetSecs; got != 3600 {
		t.Errorf("origin BudgetSecs: want 3600 (full 60 min), got %d", got)
	}
}

// SPA-136: egress polygons were silently clamped to 15 min for drive with no
// metadata flag, so every station's drive coverage was undersized too.
func TestChainer_drive_egressIsoUsesFullRemainingBudget(t *testing.T) {
	graph := &fakeGraph{nodes: []transit.Node{node("station-a", -122.39, 37.71)}}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{{Time: 600, Distance: 1.0}}, // 10 min access
			},
		},
	}
	chainer := newChainer(fc, graph)

	// budget 90 min − 10 min access = 80 min remaining at station-a.
	resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeDrive))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if len(resp.Metadata.ReachableStations) != 1 {
		t.Fatalf("ReachableStations: want 1, got %d", len(resp.Metadata.ReachableStations))
	}
	if got := resp.Metadata.ReachableStations[0].RemainingMins; got != 80 {
		t.Fatalf("RemainingMins: want 80, got %d", got)
	}
	// Match on origin coordinates rather than call order, which errgroup doesn't fix.
	iso, _ := fc.Calls()
	var egressBudget int
	for _, call := range iso {
		if call.Origin.Lat == 37.71 && call.Origin.Lng == -122.39 {
			egressBudget = call.BudgetSecs
		}
	}
	if egressBudget != 4800 {
		t.Errorf("egress BudgetSecs: want 4800 (full 80 min remaining), got %d", egressBudget)
	}
}

// Valhalla's contour is minute-granular, so a station with under a minute of
// budget left would go out as {"time": 0} — which Valhalla rejects, failing the
// whole errgroup and with it the entire request. Such stations are dropped.
func TestChainer_subMinuteRemainingBudget_stationDropped(t *testing.T) {
	graph := &fakeGraph{nodes: []transit.Node{node("station-a", -122.39, 37.71)}}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			// 600 s budget − 570 s access = 30 s remaining → 0 whole minutes.
			SourcesToTargets: [][]valhalla.MatrixCell{{{Time: 570, Distance: 1.0}}},
		},
	}
	chainer := newChainer(fc, graph)

	resp, err := chainer.Chain(context.Background(), req(10, transit.TravelModeDrive))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if len(resp.Metadata.ReachableStations) != 0 {
		t.Errorf("ReachableStations: want 0 (under a minute left), got %d", len(resp.Metadata.ReachableStations))
	}
	iso, _ := fc.Calls()
	for _, call := range iso {
		if call.BudgetSecs < 60 {
			t.Errorf("BudgetSecs=%d would send a 0-minute contour and trigger a 400", call.BudgetSecs)
		}
	}
}

// The three routing failure modes have to stay distinguishable all the way out
// of the chainer: the consumer decides whether to requeue a job by asking which
// one it was, and collapsing them would either loop forever on a bad request or
// abandon a job the server would have answered a second later.
func TestChainer_routingErrorsPropagateByCause(t *testing.T) {
	tests := []struct {
		name string
		stub *valhalla.StubClient
		want error
	}{
		{
			name: "isochrone bad request",
			stub: &valhalla.StubClient{IsochroneErr: valhalla.ErrBadRequest},
			want: isochrone.ErrRoutingRejected,
		},
		{
			name: "isochrone rate limit",
			stub: &valhalla.StubClient{IsochroneErr: valhalla.ErrRateLimit},
			want: isochrone.ErrRoutingRateLimit,
		},
		{
			name: "isochrone upstream",
			stub: &valhalla.StubClient{IsochroneErr: valhalla.ErrUpstream},
			want: isochrone.ErrRoutingUnavailable,
		},
		{
			name: "matrix rate limit",
			stub: &valhalla.StubClient{IsochroneResp: cannedIso(), MatrixErr: valhalla.ErrRateLimit},
			want: isochrone.ErrRoutingRateLimit,
		},
		{
			name: "matrix bad request",
			stub: &valhalla.StubClient{IsochroneResp: cannedIso(), MatrixErr: valhalla.ErrBadRequest},
			want: isochrone.ErrRoutingRejected,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			chainer := newChainer(tc.stub, newTestGraph())

			_, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk))
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("want %v, got %v", tc.want, err)
			}
		})
	}
}

// A failure part-way through the egress fan-out fails the whole chain rather
// than returning a surface with holes in it. A partial isochrone is worse than
// none: it looks like an answer and understates reachability.
func TestChainer_egressFailureFailsTheChain(t *testing.T) {
	graph := &fakeGraph{nodes: []transit.Node{
		node("station-a", -122.39, 37.71),
		node("station-b", -122.41, 37.69),
	}}
	fc := &valhalla.StubClient{
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{{Time: 600, Distance: 1.0}, {Time: 600, Distance: 1.0}},
			},
		},
		IsochroneFn: func(r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
			if r.Origin.Lat == 37.69 {
				return nil, valhalla.ErrUpstream
			}
			return cannedIso(), nil
		},
	}
	chainer := newChainer(fc, graph)

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk)); !errors.Is(err, isochrone.ErrRoutingUnavailable) {
		t.Errorf("want ErrRoutingUnavailable, got %v", err)
	}
}

// The contour ceiling is patched to 320 minutes on the deployed container
// (kustomize-config sets ISOCHRONE_MAX_TIME_MINUTES=320). Nothing may go out
// above it, whatever the budget, because that is a 400 for the whole request.
func TestChainer_isochroneBudgetNeverExceedsContourCeiling(t *testing.T) {
	limits := valhalla.DefaultLimits()
	maxContourSecs := limits.MaxIsoContourMins * 60

	for _, mode := range []transit.TravelMode{transit.TravelModeWalk, transit.TravelModeBike, transit.TravelModeDrive} {
		t.Run(string(mode), func(t *testing.T) {
			fc := &valhalla.StubClient{IsochroneResp: cannedIso()}
			chainer := newChainer(fc, &fakeGraph{})

			resp, err := chainer.Chain(context.Background(), req(limits.MaxIsoContourMins+60, mode))
			if err != nil {
				t.Fatalf("Chain: %v", err)
			}
			if !resp.Metadata.OriginIsoClamped {
				t.Error("OriginIsoClamped: want true for a budget over the ceiling")
			}
			iso, _ := fc.Calls()
			for _, call := range iso {
				if call.BudgetSecs > maxContourSecs {
					t.Errorf("BudgetSecs=%d exceeds the %d s contour ceiling — would trigger a 400", call.BudgetSecs, maxContourSecs)
				}
			}
		})
	}
}

// The 120-minute ceiling the API hard-coded was Stadia's, and it is nearly
// three times too strict against the deployed container. A 300-minute budget
// must now go out whole.
func TestChainer_contourCeilingIsThePatched320NotStadias120(t *testing.T) {
	fc := &valhalla.StubClient{IsochroneResp: cannedIso()}
	chainer := newChainer(fc, &fakeGraph{})

	resp, err := chainer.Chain(context.Background(), req(300, transit.TravelModeDrive))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if resp.Metadata.OriginIsoClamped {
		t.Error("OriginIsoClamped: want false — 300 min is under the 320-min ceiling")
	}
	iso, _ := fc.Calls()
	if len(iso) != 1 {
		t.Fatalf("IsochroneCalls: want 1, got %d", len(iso))
	}
	if got := iso[0].BudgetSecs; got != 300*60 {
		t.Errorf("BudgetSecs: want %d (full 300 min), got %d", 300*60, got)
	}
}

func TestChainer_bikeIsoBudgetUnclampedBelowCeiling(t *testing.T) {
	fc := &valhalla.StubClient{IsochroneResp: cannedIso()}
	chainer := newChainer(fc, &fakeGraph{})

	resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeBike))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	iso, _ := fc.Calls()
	if len(iso) != 1 {
		t.Fatalf("IsochroneCalls: want 1, got %d", len(iso))
	}
	if got := iso[0].BudgetSecs; got != 5400 {
		t.Errorf("BudgetSecs: want 5400 (full 90 min), got %d", got)
	}
	if resp.Metadata.OriginIsoClamped {
		t.Error("OriginIsoClamped: want false for a 90-min bike budget")
	}
}

// A graph whose services declare no boarding wait charges none, and says so.
//
// The API decided this by scenario slug — ca-hsr was the wait-free one. The
// queue message carries no scenario identity, so the question is put to the
// graph itself, which is the thing that actually determines the answer. The
// arithmetic is unchanged: budget 5400s − 600s access − 1800s transit − 0 wait
// = 3000s = 50 min.
func TestChainer_graphWithoutWait_waitModelNone(t *testing.T) {
	graph := &fakeGraph{
		nodes: []transit.Node{
			node("hsr-a", -122.39, 37.71),
			node("hsr-b", -122.41, 37.69),
		},
		travelTimes: map[[2]string]int{{"hsr-a", "hsr-b"}: 1800},
	}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{
					{Time: 600, Distance: 1.0},  // hsr-a: 10 min
					{Time: 3000, Distance: 5.0}, // hsr-b: 50 min direct
				},
			},
		},
	}
	chainer := newChainer(fc, graph)

	resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}

	if resp.Metadata.WaitModel != "none" {
		t.Errorf("WaitModel: want %q, got %q", "none", resp.Metadata.WaitModel)
	}

	hb := findStation(t, resp, "hsr-b")
	if hb.RemainingMins != 50 {
		t.Errorf("hsr-b RemainingMins: want 50 (no wait charged), got %d", hb.RemainingMins)
	}
}

// A graph whose services declare a boarding wait charges it, and says so:
// budget 5400s − 600s access − 1800s transit − 900s wait = 2100s = 35 min.
func TestChainer_graphWithWait_waitModelHeadway(t *testing.T) {
	graph := &fakeGraph{
		nodes: []transit.Node{
			node("station-a", -122.39, 37.71),
			node("station-b", -122.41, 37.69),
		},
		travelTimes: map[[2]string]int{{"station-a", "station-b"}: 1800},
		waitTimes:   map[[2]string]int{{"station-a", "station-b"}: 900},
	}
	// station-b is not directly reachable from origin (-1); the only path is
	// via station-a, so the wait is unavoidable.
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{
					{Time: 600, Distance: 1.0}, // station-a: 10 min
					{Time: -1, Distance: 0},    // station-b: not directly reachable
				},
			},
		},
	}
	chainer := newChainer(fc, graph)

	resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}

	if resp.Metadata.WaitModel != "headway_over_2_peak" {
		t.Errorf("WaitModel: want %q, got %q", "headway_over_2_peak", resp.Metadata.WaitModel)
	}

	sb := findStation(t, resp, "station-b")
	if sb.RemainingMins != 35 {
		t.Errorf("station-b RemainingMins: want 35 (wait applied), got %d", sb.RemainingMins)
	}
}

// Every egress feature carries the station it was cut from, so a client can
// attribute a polygon without matching it back by geometry.
func TestChainer_egressFeaturesCarryTheirStation(t *testing.T) {
	graph := &fakeGraph{nodes: []transit.Node{node("station-a", -122.39, 37.71)}}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{{Time: 600, Distance: 1.0}}},
		},
	}
	chainer := newChainer(fc, graph)

	resp, err := chainer.Chain(context.Background(), req(60, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}

	var egress int
	for _, f := range resp.Features {
		var feat map[string]any
		if err := json.Unmarshal(f, &feat); err != nil {
			t.Fatalf("unmarshal feature: %v", err)
		}
		props, _ := feat["properties"].(map[string]any)
		if props["source"] != "egress" {
			continue
		}
		egress++
		if props["station_slug"] != "station-a" {
			t.Errorf("station_slug: want station-a, got %v", props["station_slug"])
		}
		if props["remaining_mins"] != float64(50) {
			t.Errorf("remaining_mins: want 50, got %v", props["remaining_mins"])
		}
	}
	if egress != 1 {
		t.Errorf("egress features: want 1, got %d", egress)
	}
}

func findStation(t *testing.T, resp *isochrone.ChainResponse, slug string) isochrone.ReachableStation {
	t.Helper()
	for _, rs := range resp.Metadata.ReachableStations {
		if rs.StationSlug == slug {
			return rs
		}
	}
	t.Fatalf("%q not in ReachableStations (%+v)", slug, resp.Metadata.ReachableStations)
	return isochrone.ReachableStation{}
}
