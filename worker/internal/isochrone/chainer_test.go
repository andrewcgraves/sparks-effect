package isochrone_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/geo"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/isochrone"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/metrics"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

type fakeGraph struct {
	nodes       []transit.Node
	travelTimes map[[2]string]int
	waitTimes   map[[2]string]int
}

var _ transit.GraphData = (*fakeGraph)(nil)

func (f *fakeGraph) Nodes() []transit.Node { return f.nodes }

func (f *fakeGraph) travelTime(fromSlug, toSlug string) (secs, wait int, ok bool) {
	key := [2]string{fromSlug, toSlug}
	rev := [2]string{toSlug, fromSlug}
	if v, ok := f.travelTimes[key]; ok {
		return v, f.waitTimes[key], true
	}
	if v, ok := f.travelTimes[rev]; ok {
		return v, f.waitTimes[rev], true
	}
	return 0, 0, false
}

func (f *fakeGraph) Search(accessSecs map[string]int) map[string]transit.Reached {
	out := make(map[string]transit.Reached)
	consider := func(slug string, r transit.Reached) {
		if prev, seen := out[slug]; !seen || r.TotalSecs < prev.TotalSecs {
			out[slug] = r
		}
	}
	for _, src := range sortedSlugs(accessSecs) {
		aSecs := accessSecs[src]
		consider(src, transit.Reached{TotalSecs: aSecs, SourceSlug: src})
		for _, n := range f.nodes {
			if n.Slug == src {
				continue
			}
			secs, wait, ok := f.travelTime(src, n.Slug)
			if !ok {
				continue
			}
			consider(n.Slug, transit.Reached{
				TotalSecs:       aSecs + secs + wait,
				SourceSlug:      src,
				PredecessorSlug: src,
				HopSecs:         secs,
				BoardSlug:       src,
				BoardWaitSecs:   wait,
			})
		}
	}
	return out
}

func sortedSlugs(m map[string]int) []string {
	slugs := make([]string, 0, len(m))
	for k := range m {
		slugs = append(slugs, k)
	}
	sort.Strings(slugs)
	return slugs
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

const tokenRoadKm = 10.0

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

func isoFromRings(rings [][][]float64) *valhalla.IsochroneResponse {
	feature, err := json.Marshal(map[string]any{
		"type": "Feature",
		"geometry": map[string]any{
			"type":        "Polygon",
			"coordinates": rings,
		},
		"properties": map[string]any{},
	})
	if err != nil {
		panic(err)
	}
	return &valhalla.IsochroneResponse{
		Type:     "FeatureCollection",
		Features: []json.RawMessage{feature},
	}
}

func squareAround(lng, lat, halfSideDeg float64) [][][]float64 {
	return [][][]float64{{
		{lng - halfSideDeg, lat - halfSideDeg},
		{lng + halfSideDeg, lat - halfSideDeg},
		{lng + halfSideDeg, lat + halfSideDeg},
		{lng - halfSideDeg, lat + halfSideDeg},
		{lng - halfSideDeg, lat - halfSideDeg},
	}}
}

func circleRing(origin valhalla.LatLng, radiusKm float64) [][]float64 {
	const kmPerDegreeLat = 110.574
	const n = 36

	latDelta := radiusKm / kmPerDegreeLat
	cosLat := math.Cos(origin.Lat * math.Pi / 180)
	if cosLat < 1e-6 {
		cosLat = 1e-6
	}
	lngDelta := latDelta / cosLat

	ring := make([][]float64, 0, n+1)
	for i := 0; i < n; i++ {
		theta := 2 * math.Pi * float64(i) / n
		ring = append(ring, []float64{
			origin.Lng + lngDelta*math.Cos(theta),
			origin.Lat + latDelta*math.Sin(theta),
		})
	}
	ring = append(ring, []float64{ring[0][0], ring[0][1]})
	return ring
}

func walkShapedIso() *valhalla.IsochroneResponse {
	// Clearly under a 60-minute walk circle. A 36-gon of radius WalkSpeedKmH
	// overshoots that bound because circleRing and PolygonAreaKm2 disagree
	// on how to convert degrees to kilometres.
	return isoFromRings([][][]float64{circleRing(valhalla.LatLng{Lat: 37.7, Lng: -122.4}, geo.WalkSpeedKmH*0.9)})
}

func transitShapedIso() *valhalla.IsochroneResponse {
	// 1° square is thousands of km², above the walk-circle bound even at the
	// 320-minute contour ceiling.
	return isoFromRings(squareAround(-122.4, 37.7, 0.5))
}

func newChainer(client valhalla.Client, graph transit.GraphData) isochrone.Chainer {
	return isochrone.New(client, graph, valhalla.DefaultLimits(), isochrone.EgressCache{}, logger.Discard(), nil)
}

func req(budgetMins int, mode transit.TravelMode) isochrone.ChainRequest {
	return isochrone.ChainRequest{
		Lat: 37.7, Lng: -122.4, BudgetMins: budgetMins, Mode: mode,
		CompileJobID: "compile-1",
	}
}

func interchangeGraph() transit.CompiledGraph {
	return transit.CompiledGraph{Graph: &transit.TransitGraph{
		Nodes: []transit.GraphNode{
			{Slug: "alpha", Lat: 37.70, Lng: -122.40},
			{Slug: "beta", Lat: 37.71, Lng: -122.40},
			{Slug: "gamma", Lat: 37.72, Lng: -122.40},
			{Slug: "delta", Lat: 37.71, Lng: -122.41},
		},
		Services: []transit.ServiceGraph{
			{ServiceID: "trunk", WaitSecs: 600, Edges: []transit.Edge{
				{FromSlug: "alpha", ToSlug: "beta", Seconds: 900, DwellS: 60},
				{FromSlug: "beta", ToSlug: "gamma", Seconds: 900, DwellS: 45},
			}},
			{ServiceID: "spur", WaitSecs: 1200, Edges: []transit.Edge{
				{FromSlug: "beta", ToSlug: "delta", Seconds: 600, DwellS: 30},
			}},
		},
	}}
}

func walkableFrom(accessSecs int, index int) *valhalla.MatrixResponse {
	row := make([]valhalla.MatrixCell, 4)
	for i := range row {
		row[i] = valhalla.MatrixCell{}
	}
	row[index] = valhalla.Cell(accessSecs, tokenRoadKm)
	return &valhalla.MatrixResponse{SourcesToTargets: [][]valhalla.MatrixCell{row}}
}

func stationsBySlug(t *testing.T, resp *isochrone.ChainResponse) map[string]isochrone.ReachableStation {
	t.Helper()
	out := make(map[string]isochrone.ReachableStation, len(resp.Metadata.ReachableStations))
	for _, s := range resp.Metadata.ReachableStations {
		out[s.StationSlug] = s
	}
	return out
}

func TestChainer_reachableStationsFormATreeRootedAtTheAccessStations(t *testing.T) {
	fc := &valhalla.StubClient{IsochroneResp: cannedIso(), MatrixResp: walkableFrom(300, 0)}
	chainer := newChainer(fc, interchangeGraph())

	resp, err := chainer.Chain(context.Background(), req(120, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}

	stations := stationsBySlug(t, resp)
	if len(stations) != 4 {
		t.Fatalf("reached %d stations, want all 4: %+v", len(stations), resp.Metadata.ReachableStations)
	}
	want := map[string]string{"alpha": "", "beta": "alpha", "gamma": "beta", "delta": "beta"}
	for slug, wantPred := range want {
		if got := stations[slug].PredecessorSlug; got != wantPred {
			t.Errorf("%s came from %q, want %q", slug, got, wantPred)
		}
	}
	for slug, s := range stations {
		if s.PredecessorSlug == "" {
			continue
		}
		if _, ok := stations[s.PredecessorSlug]; !ok {
			t.Errorf("%s names predecessor %q, which is not itself reported", slug, s.PredecessorSlug)
		}
	}
}

func TestChainer_legsNameTheServiceRiddenOnEachHop(t *testing.T) {
	fc := &valhalla.StubClient{IsochroneResp: cannedIso(), MatrixResp: walkableFrom(300, 0)}
	chainer := newChainer(fc, interchangeGraph())

	resp, err := chainer.Chain(context.Background(), req(120, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	stations := stationsBySlug(t, resp)

	delta := stations["delta"]
	wantLegs := []isochrone.Leg{
		{FromSlug: "alpha", ToSlug: "beta", ServiceID: "trunk", Secs: 900, DwellS: 60},
		{FromSlug: "beta", ToSlug: "delta", ServiceID: "spur", Secs: 600, DwellS: 30},
	}
	if len(delta.Legs) != len(wantLegs) {
		t.Fatalf("delta legs = %+v, want two hops crossing two services", delta.Legs)
	}
	for i, want := range wantLegs {
		if delta.Legs[i] != want {
			t.Errorf("delta leg %d = %+v, want %+v", i, delta.Legs[i], want)
		}
	}

	if alpha := stations["alpha"]; len(alpha.Legs) != 0 {
		t.Errorf("alpha legs = %+v, want none — the rider walked there", alpha.Legs)
	}
}

func TestChainer_boardingWaitIsChargedOnceAndNotAgainOnAServiceChange(t *testing.T) {
	fc := &valhalla.StubClient{IsochroneResp: cannedIso(), MatrixResp: walkableFrom(300, 0)}
	chainer := newChainer(fc, interchangeGraph())

	resp, err := chainer.Chain(context.Background(), req(120, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	stations := stationsBySlug(t, resp)

	for _, slug := range []string{"beta", "gamma", "delta"} {
		s := stations[slug]
		if s.BoardSlug != "alpha" {
			t.Errorf("%s boarded at %q, want alpha", slug, s.BoardSlug)
		}
		if s.BoardWaitSecs != 600 {
			t.Errorf("%s paid %d s of boarding wait, want the trunk's 600 s once", slug, s.BoardWaitSecs)
		}
	}

	if got := stations["delta"].RemainingSecs; got != 120*60-2400 {
		t.Errorf("delta remaining = %d s, want %d — a transfer costs nothing", got, 120*60-2400)
	}
}

func TestChainer_reportsSecondsAlongsideWholeMinutes(t *testing.T) {
	fc := &valhalla.StubClient{IsochroneResp: cannedIso(), MatrixResp: walkableFrom(743, 0)}
	chainer := newChainer(fc, interchangeGraph())

	resp, err := chainer.Chain(context.Background(), req(120, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}

	for _, s := range resp.Metadata.ReachableStations {
		if s.AccessSecs != 743 {
			t.Errorf("%s access = %d s, want the matrix's own 743", s.StationSlug, s.AccessSecs)
		}
		if s.AccessMins != s.AccessSecs/60 {
			t.Errorf("%s access = %d mins against %d s", s.StationSlug, s.AccessMins, s.AccessSecs)
		}
		if s.RemainingMins != s.RemainingSecs/60 {
			t.Errorf("%s remaining = %d mins against %d s", s.StationSlug, s.RemainingMins, s.RemainingSecs)
		}
	}
}

func TestChainer_describesAJourneyOverAGraphWithNoDwell(t *testing.T) {
	g := interchangeGraph()
	for si := range g.Graph.Services {
		for ei := range g.Graph.Services[si].Edges {
			g.Graph.Services[si].Edges[ei].DwellS = 0
		}
	}
	fc := &valhalla.StubClient{IsochroneResp: cannedIso(), MatrixResp: walkableFrom(300, 0)}

	resp, err := newChainer(fc, g).Chain(context.Background(), req(120, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	delta := stationsBySlug(t, resp)["delta"]
	if len(delta.Legs) != 2 {
		t.Fatalf("delta legs = %+v, want the journey still described", delta.Legs)
	}
	for _, l := range delta.Legs {
		if l.DwellS != 0 {
			t.Errorf("leg %s->%s reported %d s of dwell from a graph carrying none", l.FromSlug, l.ToSlug, l.DwellS)
		}
	}
}

func TestChainer_happyPath_twoStations(t *testing.T) {
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{
					valhalla.Cell(600, tokenRoadKm),
					valhalla.Cell(3000, tokenRoadKm),
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

	if len(resp.Features) != 2 {
		t.Errorf("Features len: want 2, got %d", len(resp.Features))
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

	if len(resp.Features) != 1 {
		t.Errorf("Features len: want 1, got %d", len(resp.Features))
	}
	if len(resp.Metadata.ReachableStations) != 0 {
		t.Errorf("ReachableStations: want 0, got %d", len(resp.Metadata.ReachableStations))
	}

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
					{},
					{},
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
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{
					valhalla.Cell(600, tokenRoadKm),
					{},
				},
			},
		},
	}
	chainer := newChainer(fc, newTestGraph())

	resp, err := chainer.Chain(context.Background(), req(30, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}

	if len(resp.Metadata.ReachableStations) != 1 {
		t.Fatalf("ReachableStations: want 1, got %d", len(resp.Metadata.ReachableStations))
	}
	if resp.Metadata.ReachableStations[0].StationSlug != "station-a" {
		t.Errorf("station slug: want station-a, got %q", resp.Metadata.ReachableStations[0].StationSlug)
	}
}

func TestChainer_directAccess_AequalsB(t *testing.T) {
	graph := &fakeGraph{nodes: []transit.Node{node("station-a", -122.39, 37.71)}}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{valhalla.Cell(600, tokenRoadKm)},
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
	if rs.RemainingMins != 50 {
		t.Errorf("remaining: want 50, got %d", rs.RemainingMins)
	}
	if rs.ViaService != "" {
		t.Errorf("ViaService: want empty (no service boarded), got %q", rs.ViaService)
	}

	if len(resp.Features) != 1 {
		t.Errorf("Features len: want 1 (origin only), got %d", len(resp.Features))
	}
	for _, f := range resp.Features {
		var feat map[string]any
		if err := json.Unmarshal(f, &feat); err != nil {
			t.Fatalf("unmarshal feature: %v", err)
		}
		props, _ := feat["properties"].(map[string]any)
		if props["source"] == "egress" {
			t.Errorf("found an egress feature for a walk-only station: %v", props)
		}
	}
	if iso, _ := fc.Calls(); len(iso) != 1 {
		t.Errorf("isochrone calls: want 1 (origin only, no egress request for a walk-only station), got %d", len(iso))
	}
}

func TestChainer_concurrentFanOut_noRace(t *testing.T) {
	const stationCount = 5
	nodes := make([]transit.Node, stationCount+1)
	cells := make([]valhalla.MatrixCell, stationCount+1)
	tt := make(map[[2]string]int)

	nodes[0] = node("hub", -122.40, 37.70)
	cells[0] = valhalla.Cell(600, tokenRoadKm)
	for i := range stationCount {
		slug := "st-" + string(rune('a'+i))
		nodes[i+1] = node(slug, -122.40+0.005*float64(i+1), 37.70+0.005*float64(i+1))
		cells[i+1] = valhalla.MatrixCell{}
		tt[[2]string{"hub", slug}] = 600
	}

	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp:    &valhalla.MatrixResponse{SourcesToTargets: [][]valhalla.MatrixCell{cells}},
	}
	chainer := newChainer(fc, &fakeGraph{nodes: nodes, travelTimes: tt})

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}
}

func TestChainer_egressFanOutIsBoundedByConfiguration(t *testing.T) {
	const nodeCount = 40
	const concurrency = 3

	nodes := make([]transit.Node, nodeCount+1)
	cells := make([]valhalla.MatrixCell, nodeCount+1)
	tt := make(map[[2]string]int)
	nodes[0] = node("hub", -122.40, 37.70)
	cells[0] = valhalla.Cell(600, tokenRoadKm)
	for i := range nodeCount {
		slug := fmt.Sprintf("station-%d", i)
		nodes[i+1] = node(slug, -122.40+0.001*float64(i), 37.70)
		cells[i+1] = valhalla.MatrixCell{}
		tt[[2]string{"hub", slug}] = 600
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

			time.Sleep(5 * time.Millisecond)

			mu.Lock()
			inFlight--
			mu.Unlock()
			return cannedIso(), nil
		},
	}

	limits := valhalla.DefaultLimits()
	limits.MaxConcurrency = concurrency
	chainer := isochrone.New(fc, &fakeGraph{nodes: nodes, travelTimes: tt}, limits, isochrone.EgressCache{}, logger.Discard(), nil)

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if peak > concurrency {
		t.Errorf("peak concurrent routing calls = %d, want at most %d", peak, concurrency)
	}

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
			SourcesToTargets: [][]valhalla.MatrixCell{{valhalla.Cell(600, tokenRoadKm)}},
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

func TestChainer_driveReachFollowsBudgetNotA20kmFloor(t *testing.T) {
	graph := &fakeGraph{nodes: []transit.Node{
		node("station-near", -122.39, 37.71),
		node("station-30km", -122.06, 37.71),
	}}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{valhalla.Cell(600, tokenRoadKm), valhalla.Cell(1800, tokenRoadKm)},
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

func TestChainer_driveReachCappedAtAutoMatrixBLineLimit(t *testing.T) {
	graph := &fakeGraph{nodes: []transit.Node{
		node("station-near", -122.39, 37.71),

		node("station-610km", -122.40, 43.20),
	}}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{valhalla.Cell(600, tokenRoadKm)}},
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

func TestChainer_matrixPairCap_truncates(t *testing.T) {
	const cap = 600
	const nodeCount = 620

	nodes := make([]transit.Node, nodeCount)
	cells := make([]valhalla.MatrixCell, nodeCount)
	for i := range nodeCount {
		nodes[i] = node(
			fmt.Sprintf("station-%d", i),
			-122.40+0.001*float64(i%30),
			37.70+0.001*float64(i/30),
		)
		cells[i] = valhalla.MatrixCell{}
	}

	limits := valhalla.DefaultLimits()
	limits.MaxMatrixPairs = cap

	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp:    &valhalla.MatrixResponse{SourcesToTargets: [][]valhalla.MatrixCell{cells[:cap]}},
	}
	chainer := isochrone.New(fc, &fakeGraph{nodes: nodes}, limits, isochrone.EgressCache{}, logger.Discard(), nil)

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
		cells[i] = valhalla.MatrixCell{}
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

func TestChainer_drive_egressIsoUsesFullRemainingBudget(t *testing.T) {
	graph := &fakeGraph{
		nodes: []transit.Node{
			node("hub", -122.40, 37.70),
			node("station-a", -122.39, 37.71),
		},
		travelTimes: map[[2]string]int{{"hub", "station-a"}: 60},
	}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{valhalla.Cell(600, tokenRoadKm), {}},
			},
		},
	}
	chainer := newChainer(fc, graph)

	resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeDrive))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	sa := findStation(t, resp, "station-a")
	if sa.RemainingMins != 79 {
		t.Fatalf("RemainingMins: want 79, got %d", sa.RemainingMins)
	}

	iso, _ := fc.Calls()
	var egressBudget int
	for _, call := range iso {
		if call.Origin.Lat == 37.71 && call.Origin.Lng == -122.39 {
			egressBudget = call.BudgetSecs
		}
	}
	if egressBudget != 4740 {
		t.Errorf("egress BudgetSecs: want 4740 (full 79 min remaining), got %d", egressBudget)
	}
}

func TestChainer_subMinuteRemainingBudget_stationReachedWithoutAnEgressPolygon(t *testing.T) {
	graph := &fakeGraph{nodes: []transit.Node{node("station-a", -122.39, 37.71)}}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{valhalla.Cell(570, tokenRoadKm)}},
		},
	}
	chainer := newChainer(fc, graph)

	resp, err := chainer.Chain(context.Background(), req(10, transit.TravelModeDrive))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	stations := resp.Metadata.ReachableStations
	if len(stations) != 1 || stations[0].StationSlug != "station-a" {
		t.Fatalf("ReachableStations = %+v, want station-a listed: the rider did arrive", stations)
	}
	if stations[0].RemainingSecs != 30 {
		t.Errorf("RemainingSecs = %d, want the 30 s actually left", stations[0].RemainingSecs)
	}

	iso, _ := fc.Calls()
	for _, call := range iso {
		if call.BudgetSecs < 60 {
			t.Errorf("BudgetSecs=%d would send a 0-minute contour and trigger a 400", call.BudgetSecs)
		}
		if call.Origin.Lat == 37.71 && call.Origin.Lng == -122.39 {
			t.Error("an egress polygon was requested for a station with under a minute left")
		}
	}
	for _, f := range resp.Features {
		if bytes.Contains(f, []byte(`"station_slug":"station-a"`)) {
			t.Error("station-a contributed an egress feature; its remaining seconds cannot support one")
		}
	}
}

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

func TestChainer_egressFailureDropsThatStation(t *testing.T) {
	fc := twoReachableStations()
	fc.IsochroneFn = func(r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
		if r.Origin.Lat == 37.69 {
			return nil, valhalla.ErrUpstream
		}
		return cannedIso(), nil
	}

	var buf bytes.Buffer
	chainer := isochrone.New(fc, viaTransitGraph(), valhalla.DefaultLimits(), isochrone.EgressCache{}, logger.New(&buf, false), nil)

	resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v, want the chain to survive a per-station egress failure", err)
	}

	stations := stationsBySlug(t, resp)
	if _, ok := stations["station-a"]; !ok {
		t.Error("station-a missing from reachable_stations")
	}
	if _, ok := stations["station-b"]; !ok {
		t.Error("station-b missing from reachable_stations — a dropped egress polygon is not a dropped station")
	}

	var origin, egressA, egressB bool
	for _, f := range resp.Features {
		var feat map[string]any
		if err := json.Unmarshal(f, &feat); err != nil {
			t.Fatalf("unmarshal feature: %v", err)
		}
		props, _ := feat["properties"].(map[string]any)
		switch props["source"] {
		case "origin":
			origin = true
		case "egress":
			switch props["station_slug"] {
			case "station-a":
				egressA = true
			case "station-b":
				egressB = true
			}
		}
	}
	if !origin {
		t.Error("origin feature missing")
	}
	if !egressA {
		t.Error("station-a should still have an egress feature")
	}
	if egressB {
		t.Error("station-b contributed an egress feature after its Isochrone failed")
	}

	if got := resp.Metadata.DroppedEgress; len(got) != 1 || got[0] != "station-b" {
		t.Errorf("DroppedEgress = %v, want [station-b]", got)
	}

	if logged := buf.String(); !strings.Contains(logged, "station-b") {
		t.Errorf("station-b was dropped without being named in the log:\n%s", logged)
	}
}

func TestChainer_egressRateLimitStillFailsTheChain(t *testing.T) {
	fc := twoReachableStations()
	fc.IsochroneFn = func(r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
		if r.Origin.Lat == 37.69 {
			return nil, valhalla.ErrRateLimit
		}
		return cannedIso(), nil
	}

	_, err := newChainer(fc, viaTransitGraph()).Chain(context.Background(), req(90, transit.TravelModeWalk))
	if !errors.Is(err, isochrone.ErrRoutingRateLimit) {
		t.Errorf("err = %v, want ErrRoutingRateLimit", err)
	}
}

func TestChainer_allEgressUpstreamFailsTheChain(t *testing.T) {
	fc := twoReachableStations()
	fc.IsochroneFn = func(r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
		if r.Origin.Lat == 37.7 {
			return cannedIso(), nil
		}
		return nil, valhalla.ErrUpstream
	}

	_, err := newChainer(fc, viaTransitGraph()).Chain(context.Background(), req(90, transit.TravelModeWalk))
	if !errors.Is(err, isochrone.ErrRoutingUnavailable) {
		t.Errorf("err = %v, want ErrRoutingUnavailable", err)
	}
}

func TestChainer_walkShapedTransitSurvivorDoesNotRescueTransientEgressLoss(t *testing.T) {
	for name, tc := range map[string]struct {
		survivor *valhalla.IsochroneResponse
		wantErr  bool
	}{
		"walk-shaped survivor": {survivor: walkShapedIso(), wantErr: true},
		"transit survivor":     {survivor: transitShapedIso(), wantErr: false},
	} {
		t.Run(name, func(t *testing.T) {
			fc := twoStationsByTransit(nil)
			fc.IsochroneFn = func(r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
				switch r.Origin.Lat {
				case 37.7:
					return transitShapedIso(), nil
				case 37.69:
					return nil, valhalla.ErrUpstream
				}
				return tc.survivor, nil
			}

			_, err := newChainer(fc, viaTransitGraph()).Chain(context.Background(), req(90, transit.TravelModeTransit))
			if tc.wantErr && !errors.Is(err, isochrone.ErrRoutingUnavailable) {
				t.Errorf("err = %v, want ErrRoutingUnavailable — a polygon the cache would reject is not a usable egress surface", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Chain: %v, want the chain to survive a transient loss beside a genuine transit polygon", err)
			}
		})
	}
}

func TestChainer_allEgressBadRequestLeavesTheChainIntact(t *testing.T) {
	fc := twoReachableStations()
	fc.IsochroneFn = func(r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
		if r.Origin.Lat == 37.7 {
			return cannedIso(), nil
		}
		return nil, valhalla.ErrBadRequest
	}

	resp, err := newChainer(fc, viaTransitGraph()).Chain(context.Background(), req(90, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v, want the chain to survive unroutable egress stations", err)
	}

	stations := stationsBySlug(t, resp)
	if _, ok := stations["station-a"]; !ok {
		t.Error("station-a missing from reachable_stations")
	}
	if _, ok := stations["station-b"]; !ok {
		t.Error("station-b missing from reachable_stations")
	}

	got := append([]string(nil), resp.Metadata.DroppedEgress...)
	sort.Strings(got)
	if len(got) != 2 || got[0] != "station-a" || got[1] != "station-b" {
		t.Errorf("DroppedEgress = %v, want [station-a station-b]", resp.Metadata.DroppedEgress)
	}

	for _, f := range resp.Features {
		var feat map[string]any
		if err := json.Unmarshal(f, &feat); err != nil {
			t.Fatalf("unmarshal feature: %v", err)
		}
		props, _ := feat["properties"].(map[string]any)
		if props["source"] == "egress" {
			t.Errorf("found an egress feature after every station Isochrone 400: %v", props)
		}
	}
}

func TestChainer_egressCancelFailsUnavailable(t *testing.T) {
	originDone := make(chan struct{})
	fc := twoReachableStations()
	fc.IsochroneCtxFn = func(ctx context.Context, r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
		if r.Origin.Lat == 37.7 {
			select {
			case <-originDone:
			default:
				close(originDone)
			}
			return cannedIso(), nil
		}
		<-ctx.Done()
		return nil, fmt.Errorf("%w: %v", valhalla.ErrUpstream, ctx.Err())
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() {
		_, err := newChainer(fc, viaTransitGraph()).Chain(ctx, req(90, transit.TravelModeWalk))
		errCh <- err
	}()

	select {
	case <-originDone:
	case <-time.After(5 * time.Second):
		t.Fatal("origin isochrone never ran")
	}
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Chain succeeded after cancel, want ErrRoutingUnavailable")
		}
		if errors.Is(err, isochrone.ErrRoutingRejected) {
			t.Errorf("err = %v, cancel must not be ErrRoutingRejected", err)
		}
		if !errors.Is(err, isochrone.ErrRoutingUnavailable) {
			t.Errorf("err = %v, want ErrRoutingUnavailable", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Chain did not return after cancel")
	}
}

func TestChainer_isochroneBudgetNeverExceedsContourCeiling(t *testing.T) {
	limits := valhalla.DefaultLimits()
	maxContourSecs := limits.MaxIsoContourMins * 60

	for _, mode := range transit.TravelModes() {
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
					valhalla.Cell(600, tokenRoadKm),
					valhalla.Cell(3000, tokenRoadKm),
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

func TestChainer_graphWithWait_waitModelHeadway(t *testing.T) {
	graph := &fakeGraph{
		nodes: []transit.Node{
			node("station-a", -122.39, 37.71),
			node("station-b", -122.41, 37.69),
		},
		travelTimes: map[[2]string]int{{"station-a", "station-b"}: 1800},
		waitTimes:   map[[2]string]int{{"station-a", "station-b"}: 900},
	}

	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{
					valhalla.Cell(600, tokenRoadKm),
					{},
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

func TestChainer_egressFeaturesCarryTheirStation(t *testing.T) {
	graph := &fakeGraph{
		nodes: []transit.Node{
			node("hub", -122.40, 37.70),
			node("station-a", -122.39, 37.71),
		},
		travelTimes: map[[2]string]int{{"hub", "station-a"}: 60},
	}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{valhalla.Cell(540, tokenRoadKm), {}}},
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

func cannedRoute() *valhalla.RouteResponse {
	return &valhalla.RouteResponse{Shape: []valhalla.LatLng{
		{Lat: 37.7001, Lng: -122.4002},
		{Lat: 37.7040, Lng: -122.4002},
		{Lat: 37.7040, Lng: -122.3930},
		{Lat: 37.7098, Lng: -122.3901},
	}}
}

func starterWalkGeometry(t *testing.T, walk *isochrone.StarterWalk) [][]float64 {
	t.Helper()
	if walk == nil {
		t.Fatal("StarterWalk is nil")
	}
	var geom struct {
		Type        string      `json:"type"`
		Coordinates [][]float64 `json:"coordinates"`
	}
	if err := json.Unmarshal(walk.Geometry, &geom); err != nil {
		t.Fatalf("unmarshal starter walk geometry: %v", err)
	}
	if geom.Type != "LineString" {
		t.Errorf("geometry type = %q, want LineString", geom.Type)
	}
	return geom.Coordinates
}

func TestChainer_starterWalkCarriesTheRoutedShape(t *testing.T) {
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteResp:     cannedRoute(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{
				valhalla.Cell(600, tokenRoadKm),
				valhalla.Cell(3000, tokenRoadKm),
			}},
		},
	}
	chainer := newChainer(fc, newTestGraph())

	resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}

	walk := resp.Metadata.StarterWalk
	if walk == nil {
		t.Fatal("StarterWalk: want the walk to station-a, got nil")
	}
	if walk.StationSlug != "station-a" {
		t.Errorf("StarterWalk.StationSlug = %q, want station-a", walk.StationSlug)
	}

	coords := starterWalkGeometry(t, walk)
	if len(coords) != 4 {
		t.Fatalf("geometry has %d points, want the 4 Valhalla routed: %v", len(coords), coords)
	}

	if coords[0][0] != -122.4002 || coords[0][1] != 37.7001 {
		t.Errorf("first point = %v, want Valhalla's snapped origin [-122.4002 37.7001]", coords[0])
	}
	if coords[3][0] != -122.3901 || coords[3][1] != 37.7098 {
		t.Errorf("last point = %v, want Valhalla's snapped station [-122.3901 37.7098]", coords[3])
	}
}

func TestChainer_onlyTheStarterStationIsRouted(t *testing.T) {
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteResp:     cannedRoute(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{
				valhalla.Cell(600, tokenRoadKm),
				valhalla.Cell(3000, tokenRoadKm),
			}},
		},
	}
	chainer := newChainer(fc, newTestGraph())

	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	routes := fc.RouteCalls()
	if len(routes) != 1 {
		t.Fatalf("Route calls = %d, want exactly 1", len(routes))
	}

	if routes[0].Origin != (valhalla.LatLng{Lat: 37.7, Lng: -122.4}) {
		t.Errorf("route origin = %v, want the request origin", routes[0].Origin)
	}
	if routes[0].Destination != (valhalla.LatLng{Lat: 37.71, Lng: -122.39}) {
		t.Errorf("route destination = %v, want station-a's graph node", routes[0].Destination)
	}
	if routes[0].Costing != valhalla.CostingPedestrian {
		t.Errorf("route costing = %q, want pedestrian", routes[0].Costing)
	}
}

func TestChainer_noReachedStation_noStarterWalk(t *testing.T) {
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteResp:     cannedRoute(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{
				{},
				{},
			}},
		},
	}
	chainer := newChainer(fc, newTestGraph())

	resp, err := chainer.Chain(context.Background(), req(30, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if resp.Metadata.StarterWalk != nil {
		t.Errorf("StarterWalk = %+v, want nil", resp.Metadata.StarterWalk)
	}
	if len(fc.RouteCalls()) != 0 {
		t.Errorf("Route calls = %d, want 0", len(fc.RouteCalls()))
	}
}

func TestChainer_starterIsTheShortestWalkThenTheSlug(t *testing.T) {
	graph := &fakeGraph{
		nodes: []transit.Node{
			node("station-a", -122.39, 37.71),
			node("station-b", -122.41, 37.69),
		},
	}

	cases := []struct {
		name     string
		accessA  int
		accessB  int
		wantSlug string
	}{
		{
			name:     "seconds decide inside one whole minute",
			accessA:  612,
			accessB:  600,
			wantSlug: "station-b",
		},
		{
			name:     "shorter walk across a minute boundary",
			accessA:  660,
			accessB:  600,
			wantSlug: "station-b",
		},
		{
			name:     "equal seconds break on the slug",
			accessA:  600,
			accessB:  600,
			wantSlug: "station-a",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fc := &valhalla.StubClient{
				IsochroneResp: cannedIso(),
				RouteResp:     cannedRoute(),
				MatrixResp: &valhalla.MatrixResponse{
					SourcesToTargets: [][]valhalla.MatrixCell{{
						valhalla.Cell(tc.accessA, tokenRoadKm),
						valhalla.Cell(tc.accessB, tokenRoadKm),
					}},
				},
			}

			resp, err := newChainer(fc, graph).Chain(context.Background(), req(90, transit.TravelModeWalk))
			if err != nil {
				t.Fatalf("Chain: %v", err)
			}
			if resp.Metadata.StarterWalk == nil || resp.Metadata.StarterWalk.StationSlug != tc.wantSlug {
				t.Errorf("StarterWalk = %+v, want %s", resp.Metadata.StarterWalk, tc.wantSlug)
			}
		})
	}
}

func TestChainer_matrixPairThatWasNeverRoutedIsNotAnAccessStation(t *testing.T) {
	graph := &fakeGraph{
		nodes: []transit.Node{
			node("near-station", -122.39, 37.71),
			node("far-station", -121.0, 38.5),
		},
	}
	chainWithFarCell := func(t *testing.T, far valhalla.MatrixCell) *isochrone.ChainResponse {
		t.Helper()
		fc := &valhalla.StubClient{
			IsochroneResp: cannedIso(),
			RouteResp:     cannedRoute(),
			MatrixResp: &valhalla.MatrixResponse{
				SourcesToTargets: [][]valhalla.MatrixCell{{
					valhalla.Cell(600, tokenRoadKm),
					far,
				}},
			},
		}
		resp, err := newChainer(fc, graph).Chain(context.Background(), req(240, transit.TravelModeDrive))
		if err != nil {
			t.Fatalf("Chain: %v", err)
		}
		return resp
	}

	for _, tc := range []struct {
		name string
		far  valhalla.MatrixCell
	}{
		{"nulls, which is how Valhalla reports a pair it could not route", valhalla.MatrixCell{}},
		{"a pair of zeroes, which is how an older build reports the same", valhalla.Cell(0, 0)},
		{"a road shorter than the straight line it spans", valhalla.Cell(60, 0.5)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := chainWithFarCell(t, tc.far)

			if _, ok := stationsBySlug(t, resp)["far-station"]; ok {
				t.Error("far-station was reported reached, off a pair that was never routed")
			}
			if resp.Metadata.StarterWalk == nil || resp.Metadata.StarterWalk.StationSlug != "near-station" {
				t.Errorf("StarterWalk = %+v, want near-station — the one the rider can actually drive to",
					resp.Metadata.StarterWalk)
			}
		})
	}

	t.Run("a real drive to a distant station is kept", func(t *testing.T) {
		resp := chainWithFarCell(t, valhalla.Cell(7200, 190.0))

		if _, ok := stationsBySlug(t, resp)["far-station"]; !ok {
			t.Error("far-station was dropped, though its two-hour drive covers a plausible road")
		}
		if resp.Metadata.StarterWalk == nil || resp.Metadata.StarterWalk.StationSlug != "near-station" {
			t.Errorf("StarterWalk = %+v, want near-station as the shorter of the two drives",
				resp.Metadata.StarterWalk)
		}
	})
}

func TestChainer_droppedMatrixPairsAreLoggedByName(t *testing.T) {
	graph := &fakeGraph{
		nodes: []transit.Node{
			node("near-station", -122.39, 37.71),
			node("unrouted-station", -121.0, 38.5),
			node("implausible-station", -121.2, 38.4),
		},
	}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteResp:     cannedRoute(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{
				valhalla.Cell(600, tokenRoadKm),
				valhalla.MatrixCell{},
				valhalla.Cell(60, 0.5),
			}},
		},
	}

	var buf bytes.Buffer
	chainer := isochrone.New(fc, graph, valhalla.DefaultLimits(), isochrone.EgressCache{}, logger.New(&buf, false), nil)
	if _, err := chainer.Chain(context.Background(), req(240, transit.TravelModeDrive)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	logged := buf.String()
	for _, want := range []string{"unrouted-station", "implausible-station"} {
		if !strings.Contains(logged, want) {
			t.Errorf("%s was dropped without being named in the log:\n%s", want, logged)
		}
	}

	if strings.Contains(logged, "near-station") {
		t.Errorf("near-station was reached and should not appear in the log:\n%s", logged)
	}
}

func TestChainer_starterIsNeverAStationReachedByRiding(t *testing.T) {
	graph := &fakeGraph{
		nodes: []transit.Node{
			node("hub", -122.40, 37.70),
			node("station-a", -122.39, 37.71),
		},
		travelTimes: map[[2]string]int{{"hub", "station-a"}: 60},
	}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteResp:     cannedRoute(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{valhalla.Cell(540, tokenRoadKm), {}}},
		},
	}

	resp, err := newChainer(fc, graph).Chain(context.Background(), req(60, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if resp.Metadata.StarterWalk == nil || resp.Metadata.StarterWalk.StationSlug != "hub" {
		t.Errorf("StarterWalk = %+v, want hub — station-a was reached by riding", resp.Metadata.StarterWalk)
	}
}

func TestChainer_routeFailureLeavesTheChainIntact(t *testing.T) {
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteErr:      fmt.Errorf("%w: no path could be found", valhalla.ErrBadRequest),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{
				valhalla.Cell(600, tokenRoadKm),
				valhalla.Cell(3000, tokenRoadKm),
			}},
		},
	}
	chainer := newChainer(fc, newTestGraph())

	resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v, want the chain to survive a failed route", err)
	}
	if resp.Metadata.StarterWalk != nil {
		t.Errorf("StarterWalk = %+v, want nil after a failed route", resp.Metadata.StarterWalk)
	}

	if len(resp.Metadata.ReachableStations) != 2 {
		t.Errorf("ReachableStations = %d, want 2", len(resp.Metadata.ReachableStations))
	}
	if len(resp.Features) != 2 {
		t.Errorf("Features = %d, want 2", len(resp.Features))
	}
}

func TestChainer_starterRouteRunsAlongsideTheEgressFanOut(t *testing.T) {
	const stationCount = 4
	nodes := make([]transit.Node, stationCount+1)
	cells := make([]valhalla.MatrixCell, stationCount+1)
	tt := make(map[[2]string]int)

	nodes[0] = node("hub", -122.40, 37.70)
	cells[0] = valhalla.Cell(600, tokenRoadKm)
	for i := range stationCount {
		slug := fmt.Sprintf("station-%d", i)
		nodes[i+1] = node(slug, -122.40+0.005*float64(i+1), 37.70+0.005*float64(i+1))
		cells[i+1] = valhalla.MatrixCell{}
		tt[[2]string{"hub", slug}] = 600
	}

	routeIssued := make(chan struct{})
	var serialised atomic.Bool

	fc := &valhalla.StubClient{
		MatrixResp: &valhalla.MatrixResponse{SourcesToTargets: [][]valhalla.MatrixCell{cells}},
		RouteFn: func(valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
			close(routeIssued)
			return cannedRoute(), nil
		},
		IsochroneFn: func(r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
			if r.Origin.Lat == 37.7 && r.Origin.Lng == -122.4 {
				return cannedIso(), nil
			}
			select {
			case <-routeIssued:
			case <-time.After(2 * time.Second):
				serialised.Store(true)
			}
			return cannedIso(), nil
		},
	}

	if _, err := newChainer(fc, &fakeGraph{nodes: nodes, travelTimes: tt}).
		Chain(context.Background(), req(90, transit.TravelModeWalk)); err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if serialised.Load() {
		t.Error("the starter route did not go out until the egress fan-out had finished")
	}
}

func routedAccess(secs int) *valhalla.RouteResponse {
	return &valhalla.RouteResponse{
		Shape:      cannedRoute().Shape,
		Time:       secs,
		DistanceKm: tokenRoadKm,
	}
}

func transitAccessClient(accessSecs int) *valhalla.StubClient {
	return &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteFn: func(valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
			return routedAccess(accessSecs), nil
		},
	}
}

func TestChainer_everyValidModeHasACosting(t *testing.T) {
	want := map[transit.TravelMode]valhalla.Costing{
		transit.TravelModeWalk:    valhalla.CostingPedestrian,
		transit.TravelModeBike:    valhalla.CostingBicycle,
		transit.TravelModeDrive:   valhalla.CostingAuto,
		transit.TravelModeTransit: valhalla.CostingMultimodal,
	}
	for _, mode := range transit.TravelModes() {
		t.Run(string(mode), func(t *testing.T) {
			costing, ok := want[mode]
			if !ok {
				t.Fatalf("valid mode %q has no expected costing — add it to the modes table", mode)
			}
			fc := &valhalla.StubClient{IsochroneResp: cannedIso()}
			if _, err := newChainer(fc, &fakeGraph{}).Chain(context.Background(), req(30, mode)); err != nil {
				t.Fatalf("Chain: %v", err)
			}
			iso, matrix := fc.Calls()
			if len(iso) != 1 {
				t.Fatalf("IsochroneCalls: want 1, got %d", len(iso))
			}
			if iso[0].Costing != costing {
				t.Errorf("costing = %q, want %q", iso[0].Costing, costing)
			}
			if mode == transit.TravelModeTransit {
				if len(matrix) != 0 {
					t.Errorf("transit called Matrix %d times; multimodal is error 140", len(matrix))
				}
			}
		})
	}
}

func TestChainer_transitAccessUsesPairwiseRouteNotMatrix(t *testing.T) {
	fc := transitAccessClient(600)
	resp, err := newChainer(fc, newTestGraph()).Chain(context.Background(), req(90, transit.TravelModeTransit))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}

	iso, matrix := fc.Calls()
	if len(matrix) != 0 {
		t.Fatalf("Matrix calls = %d, want 0", len(matrix))
	}
	if len(iso) < 1 {
		t.Fatal("expected an origin isochrone")
	}
	if iso[0].Costing != valhalla.CostingMultimodal {
		t.Errorf("origin costing = %q, want multimodal", iso[0].Costing)
	}

	routes := fc.RouteCalls()
	if len(routes) < 2 {
		t.Fatalf("Route calls = %d, want at least one per nearby station", len(routes))
	}
	for _, r := range routes {
		if r.Costing != valhalla.CostingMultimodal {
			t.Errorf("route costing = %q, want multimodal (not transit)", r.Costing)
		}
	}

	stations := stationsBySlug(t, resp)
	if _, ok := stations["station-a"]; !ok {
		t.Error("station-a missing — pairwise /route should have seeded it")
	}
}

func rodeAccess(secs int) *valhalla.RouteResponse {
	route := routedAccess(secs)
	route.RodeTransit = true
	return route
}

func TestChainer_transitAccessCarriesWhetherTheLegRode(t *testing.T) {
	rodeTo := node("station-rode", -122.39, 37.71)
	beyond := node("station-beyond", -122.43, 37.67)
	graph := &fakeGraph{
		nodes: []transit.Node{
			rodeTo,
			node("station-walked", -122.41, 37.69),
			beyond,
		},
		travelTimes: map[[2]string]int{
			{"station-walked", "station-beyond"}: 600,
		},
	}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteFn: func(r valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
			switch r.Destination {
			case valhalla.LatLng{Lat: rodeTo.Lat, Lng: rodeTo.Lng}:
				return rodeAccess(600), nil
			case valhalla.LatLng{Lat: beyond.Lat, Lng: beyond.Lng}:
				// Rides, but slower than walking to station-walked and riding
				// on, so the journey that wins does not start here.
				return rodeAccess(3000), nil
			}
			return routedAccess(600), nil
		},
	}

	resp, err := newChainer(fc, graph).Chain(context.Background(), req(90, transit.TravelModeTransit))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}

	stations := stationsBySlug(t, resp)
	for slug, want := range map[string]bool{
		"station-rode":   true,
		"station-walked": false,
		// Reached by riding the scenario from a station that was walked to:
		// its access leg is that walk, not its own slower leg that rode.
		"station-beyond": false,
	} {
		s, ok := stations[slug]
		if !ok {
			t.Errorf("%s missing — a walked access leg must label the station, not drop it", slug)
			continue
		}
		if s.AccessRodeTransit == nil {
			t.Errorf("%s: access_rode_transit absent on a transit access leg", slug)
			continue
		}
		if *s.AccessRodeTransit != want {
			t.Errorf("%s: access_rode_transit = %v, want %v", slug, *s.AccessRodeTransit, want)
		}
	}
	if got := stations["station-beyond"].PredecessorSlug; got != "station-walked" {
		t.Fatalf("station-beyond came from %q, want station-walked", got)
	}

	wire, err := json.Marshal(resp.Metadata.ReachableStations)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(wire), `"access_rode_transit":false`) {
		t.Errorf("a walked transit leg must be an explicit false on the wire, got %s", wire)
	}
}

func splitAccess(walk, wait, ride int) *valhalla.RouteResponse {
	route := routedAccess(walk + wait + ride)
	route.RodeTransit = wait+ride > 0
	route.Breakdown = &valhalla.Breakdown{WalkSecs: walk, WaitSecs: wait, RideSecs: ride}
	return route
}

func TestChainer_transitAccessCarriesItsWalkWaitAndRide(t *testing.T) {
	rodeTo := node("station-rode", -122.39, 37.71)
	walkedTo := node("station-walked", -122.41, 37.69)
	beyond := node("station-beyond", -122.43, 37.67)
	graph := &fakeGraph{
		nodes: []transit.Node{rodeTo, walkedTo, beyond},
		travelTimes: map[[2]string]int{
			{"station-walked", "station-beyond"}: 600,
		},
		waitTimes: map[[2]string]int{
			{"station-walked", "station-beyond"}: 300,
		},
	}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteFn: func(r valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
			switch r.Destination {
			case valhalla.LatLng{Lat: rodeTo.Lat, Lng: rodeTo.Lng}:
				return splitAccess(1788, 462, 540), nil
			case valhalla.LatLng{Lat: beyond.Lat, Lng: beyond.Lng}:
				return splitAccess(600, 1200, 1200), nil
			}
			return splitAccess(900, 0, 0), nil
		},
	}

	resp, err := newChainer(fc, graph).Chain(context.Background(), req(90, transit.TravelModeTransit))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	stations := stationsBySlug(t, resp)
	for slug, want := range map[string]isochrone.AccessBreakdown{
		"station-rode":   {AccessWalkSecs: 1788, AccessWaitSecs: 462, AccessRideSecs: 540},
		"station-walked": {AccessWalkSecs: 900},
		// Reached by riding the scenario from station-walked: its access_secs
		// is station-walked's access leg, so its split is too, and the
		// authored ride's wait stays in board_wait_secs beside it.
		"station-beyond": {AccessWalkSecs: 900},
	} {
		s, ok := stations[slug]
		if !ok {
			t.Errorf("%s missing", slug)
			continue
		}
		if s.AccessBreakdown == nil {
			t.Errorf("%s: no access breakdown on a transit access leg that reconciled", slug)
			continue
		}
		if *s.AccessBreakdown != want {
			t.Errorf("%s: access breakdown = %+v, want %+v", slug, *s.AccessBreakdown, want)
		}
		if sum := s.AccessWalkSecs + s.AccessWaitSecs + s.AccessRideSecs; sum != s.AccessSecs {
			t.Errorf("%s: walk + wait + ride = %d s, want access_secs %d", slug, sum, s.AccessSecs)
		}
	}
	if b := stations["station-beyond"]; b.PredecessorSlug != "station-walked" || b.BoardWaitSecs != 300 {
		t.Fatalf("station-beyond came from %q with board_wait_secs %d, want station-walked and 300", b.PredecessorSlug, b.BoardWaitSecs)
	}

	wire, err := json.Marshal(resp.Metadata.ReachableStations)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, s := range decoded {
		if s["station_slug"] != "station-walked" {
			continue
		}
		for field, want := range map[string]float64{
			"access_secs": 900, "access_walk_secs": 900, "access_wait_secs": 0, "access_ride_secs": 0,
		} {
			if got, ok := s[field]; !ok || got != want {
				t.Errorf("station-walked %s = %v (present %v), want an explicit %v: a walked leg is all walk", field, got, ok, want)
			}
		}
		if s["access_rode_transit"] != false {
			t.Errorf("station-walked access_rode_transit = %v, want false", s["access_rode_transit"])
		}
	}
}

func TestChainer_unreconciledAccessKeepsTheStationWithoutASplit(t *testing.T) {
	notSumming := splitAccess(600, 300, 300)
	notSumming.Breakdown.RideSecs = 299
	refused := routedAccess(1200)
	refused.RodeTransit = true
	refused.BreakdownErr = fmt.Errorf("%w: departs 08:31, 426 s after reaching the platform, but the maneuver leaves a 522 s wait", valhalla.ErrUnreconciled)

	for name, route := range map[string]*valhalla.RouteResponse{
		"the client refused it":               refused,
		"the client's split is not the total": notSumming,
	} {
		t.Run(name, func(t *testing.T) {
			fc := &valhalla.StubClient{
				IsochroneResp: cannedIso(),
				RouteFn: func(valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
					return route, nil
				},
			}
			var buf bytes.Buffer
			chainer := isochrone.New(fc, &fakeGraph{nodes: []transit.Node{
				node("station-a", -122.39, 37.71),
				node("station-b", -122.41, 37.69),
			}}, valhalla.DefaultLimits(), isochrone.EgressCache{}, logger.New(&buf, false), nil)

			resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeTransit))
			if err != nil {
				t.Fatalf("Chain: %v — a split that does not reconcile must not fail the job", err)
			}
			stations := stationsBySlug(t, resp)
			for _, slug := range []string{"station-a", "station-b"} {
				s, ok := stations[slug]
				if !ok {
					t.Fatalf("%s missing — its access time still stands without a split", slug)
				}
				if s.AccessSecs != route.Time || s.AccessRodeTransit == nil || !*s.AccessRodeTransit {
					t.Errorf("%s: access_secs %d, access_rode_transit %v; want %d and true", slug, s.AccessSecs, s.AccessRodeTransit, route.Time)
				}
				if s.AccessBreakdown != nil {
					t.Errorf("%s carries %+v; a split that does not sum to its access time is withheld", slug, *s.AccessBreakdown)
				}
			}

			// One warning for the job, not one per station, carrying the
			// count and a sample of why.
			var warnings []map[string]any
			for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
				var rec map[string]any
				if err := json.Unmarshal([]byte(line), &rec); err != nil {
					t.Fatalf("log line %q: %v", line, err)
				}
				if rec["level"] == "WARN" {
					warnings = append(warnings, rec)
				}
			}
			if len(warnings) != 1 {
				t.Fatalf("%d warnings for two unreconciled stations, want exactly one per job:\n%s", len(warnings), buf.String())
			}
			w := warnings[0]
			if w["unreconciled"] != float64(2) {
				t.Errorf("warning counts unreconciled = %v, want 2", w["unreconciled"])
			}
			if sample, _ := w["unreconciled_sample"].(string); !strings.Contains(sample, valhalla.ErrUnreconciled.Error()) {
				t.Errorf("warning's unreconciled_sample = %q, want the first station's error", sample)
			}
		})
	}
}

func TestChainer_reconciledAccessLogsNoWarning(t *testing.T) {
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteFn: func(valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
			return splitAccess(1788, 462, 540), nil
		},
	}
	var buf bytes.Buffer
	chainer := isochrone.New(fc, &fakeGraph{nodes: []transit.Node{node("station-a", -122.39, 37.71)}},
		valhalla.DefaultLimits(), isochrone.EgressCache{}, logger.New(&buf, false), nil)
	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeTransit)); err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if strings.Contains(buf.String(), `"level":"WARN"`) {
		t.Errorf("a job whose every split reconciled logged a warning:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), `"unreconciled":0`) {
		t.Errorf("the access summary line should still count unreconciled splits:\n%s", buf.String())
	}
}

func TestChainer_nonTransitAccessOmitsWhetherTheLegRode(t *testing.T) {
	for _, mode := range []transit.TravelMode{transit.TravelModeWalk, transit.TravelModeBike, transit.TravelModeDrive} {
		t.Run(string(mode), func(t *testing.T) {
			fc := &valhalla.StubClient{IsochroneResp: cannedIso(), MatrixResp: walkableFrom(300, 0)}
			resp, err := newChainer(fc, newTestGraph()).Chain(context.Background(), req(90, mode))
			if err != nil {
				t.Fatalf("Chain: %v", err)
			}
			if len(resp.Metadata.ReachableStations) == 0 {
				t.Fatal("no reachable stations to check")
			}
			wire, err := json.Marshal(resp.Metadata.ReachableStations)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(wire), "access_rode_transit") {
				t.Errorf("%s cannot disagree with its own label, so it carries no access_rode_transit: %s", mode, wire)
			}
			for _, field := range []string{"access_walk_secs", "access_wait_secs", "access_ride_secs"} {
				if strings.Contains(string(wire), field) {
					t.Errorf("%s access comes from the matrix, which has no maneuvers to split, yet carries %s: %s", mode, field, wire)
				}
			}
		})
	}
}

func TestChainer_transitRoute400DropsThatStation(t *testing.T) {
	stationB := node("station-b", -122.41, 37.69)
	graph := &fakeGraph{nodes: []transit.Node{
		node("station-a", -122.39, 37.71),
		stationB,
	}}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteFn: func(r valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
			if r.Destination.Lat == stationB.Lat && r.Destination.Lng == stationB.Lng {
				return nil, fmt.Errorf("%w: no matching transit service near this location (error_code 171)", valhalla.ErrBadRequest)
			}
			return routedAccess(600), nil
		},
	}
	resp, err := newChainer(fc, graph).Chain(context.Background(), req(90, transit.TravelModeTransit))
	if err != nil {
		t.Fatalf("Chain: %v — a per-station 400 must not fail the job", err)
	}
	stations := stationsBySlug(t, resp)
	if _, ok := stations["station-a"]; !ok {
		t.Error("station-a missing")
	}
	if _, ok := stations["station-b"]; ok {
		t.Error("station-b should have been dropped after its access /route 400")
	}
}

func TestChainer_transitRouteUpstreamFailsTheChain(t *testing.T) {
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteFn: func(valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
			return nil, valhalla.ErrUpstream
		},
	}
	_, err := newChainer(fc, newTestGraph()).Chain(context.Background(), req(90, transit.TravelModeTransit))
	if !errors.Is(err, isochrone.ErrRoutingUnavailable) {
		t.Errorf("err = %v, want ErrRoutingUnavailable", err)
	}
}

func TestChainer_transitRouteRateLimitFailsTheChain(t *testing.T) {
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteFn: func(valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
			return nil, valhalla.ErrRateLimit
		},
	}
	_, err := newChainer(fc, newTestGraph()).Chain(context.Background(), req(90, transit.TravelModeTransit))
	if !errors.Is(err, isochrone.ErrRoutingRateLimit) {
		t.Errorf("err = %v, want ErrRoutingRateLimit", err)
	}
}

func TestChainer_transitReachFollowsBlendedSpeedNotThePedestrianBLine(t *testing.T) {
	graph := &fakeGraph{nodes: []transit.Node{
		node("station-near", -122.39, 37.71),

		node("station-50km", -122.40, 38.15),
	}}
	fc := transitAccessClient(600)
	if _, err := newChainer(fc, graph).Chain(context.Background(), req(90, transit.TravelModeTransit)); err != nil {
		t.Fatalf("Chain: %v", err)
	}
	var accessDests int
	origin := valhalla.LatLng{Lat: 37.7, Lng: -122.4}
	for _, r := range fc.RouteCalls() {
		if r.Origin == origin {
			accessDests++
		}
	}

	if accessDests < 1 {
		t.Fatal("expected a route to the nearby station")
	}
	for _, r := range fc.RouteCalls() {
		if r.Destination.Lat == 38.15 {
			t.Error("50 km station was asked as a transit access pair; 90 min at 40 km/h should have filtered it")
		}
	}
}

func TestChainer_transitReachCappedAt80kmNotThePedestrianBLine(t *testing.T) {
	graph := &fakeGraph{nodes: []transit.Node{
		node("station-near", -122.39, 37.71),

		node("station-90km", -122.40, 38.51),
	}}
	fc := transitAccessClient(600)
	if _, err := newChainer(fc, graph).Chain(context.Background(), req(9999, transit.TravelModeTransit)); err != nil {
		t.Fatalf("Chain: %v", err)
	}
	for _, r := range fc.RouteCalls() {
		if r.Destination.Lat == 38.51 {
			t.Error("90 km station was asked as a transit access pair; the 80 km cap should have dropped it")
		}
	}
}

func TestChainer_transitPairwiseAccessIsBoundedByConfiguration(t *testing.T) {
	const nodeCount = 20
	const concurrency = 3

	nodes := make([]transit.Node, nodeCount)
	for i := range nodeCount {
		nodes[i] = node(fmt.Sprintf("station-%d", i), -122.40+0.001*float64(i), 37.70)
	}

	var (
		mu       sync.Mutex
		inFlight int
		peak     int
	)
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteFn: func(valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
			mu.Lock()
			inFlight++
			if inFlight > peak {
				peak = inFlight
			}
			mu.Unlock()
			time.Sleep(5 * time.Millisecond)
			mu.Lock()
			inFlight--
			mu.Unlock()
			return routedAccess(600), nil
		},
	}

	limits := valhalla.DefaultLimits()
	limits.MaxConcurrency = concurrency
	chainer := isochrone.New(fc, &fakeGraph{nodes: nodes}, limits, isochrone.EgressCache{}, logger.Discard(), nil)
	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeTransit)); err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if peak > concurrency {
		t.Errorf("peak concurrent /route calls = %d, want ≤ %d", peak, concurrency)
	}
	if peak < 2 {
		t.Errorf("peak concurrent /route calls = %d; pairwise access is not running concurrently", peak)
	}
}

func TestChainer_transitAccessRoutesOnlyTheNearestStationsUpToTheCap(t *testing.T) {
	const cap = 5
	const nodeCount = 12

	// Listed farthest first, so keeping the first cap would keep the wrong ones.
	nodes := make([]transit.Node, nodeCount)
	for i := range nodeCount {
		d := nodeCount - i
		nodes[i] = node(fmt.Sprintf("station-%02d", d), -122.40+0.001*float64(d), 37.70)
	}

	limits := valhalla.DefaultLimits()
	limits.MaxAccessRoutes = cap
	fc := transitAccessClient(600)
	var buf bytes.Buffer
	chainer := isochrone.New(fc, &fakeGraph{nodes: nodes}, limits, isochrone.EgressCache{}, logger.New(&buf, false), nil)
	if _, err := chainer.Chain(context.Background(), req(90, transit.TravelModeTransit)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	routedTo := map[float64]bool{}
	for _, r := range fc.RouteCalls() {
		routedTo[r.Destination.Lng] = true
	}
	if len(routedTo) != cap {
		t.Errorf("access /route destinations = %d, want the cap %d", len(routedTo), cap)
	}
	for d := 1; d <= cap; d++ {
		if lng := -122.40 + 0.001*float64(d); !routedTo[lng] {
			t.Errorf("station-%02d is among the %d nearest but was not routed to", d, cap)
		}
	}

	logged := buf.String()
	if !strings.Contains(logged, "access route cap") || !strings.Contains(logged, `"cap":5`) {
		t.Errorf("truncation was not logged with its cap:\n%s", logged)
	}
}

func TestChainer_transitAccessIsBoundedByDefault(t *testing.T) {
	const nodeCount = 300

	nodes := make([]transit.Node, nodeCount)
	for i := range nodeCount {
		nodes[i] = node(
			fmt.Sprintf("station-%d", i),
			-122.40+0.001*float64(i%30),
			37.70+0.001*float64(i/30),
		)
	}

	fc := transitAccessClient(600)
	if _, err := newChainer(fc, &fakeGraph{nodes: nodes}).Chain(context.Background(), req(90, transit.TravelModeTransit)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	// Distinct destinations: the starter walk repeats one access leg's.
	routedTo := map[valhalla.LatLng]bool{}
	for _, r := range fc.RouteCalls() {
		routedTo[r.Destination] = true
	}
	if got := len(routedTo); got != valhalla.DefaultMaxAccessRoutes {
		t.Errorf("access /route destinations = %d for %d nearby stations, want the default cap %d", got, nodeCount, valhalla.DefaultMaxAccessRoutes)
	}
}

func TestChainer_matrixAccessIgnoresTheAccessRouteCap(t *testing.T) {
	const nodeCount = 20

	nodes := make([]transit.Node, nodeCount)
	cells := make([]valhalla.MatrixCell, nodeCount)
	for i := range nodeCount {
		nodes[i] = node(fmt.Sprintf("station-%d", i), -122.40+0.001*float64(i), 37.70)
		cells[i] = valhalla.MatrixCell{}
	}

	limits := valhalla.DefaultLimits()
	limits.MaxAccessRoutes = 3
	for _, mode := range []transit.TravelMode{transit.TravelModeWalk, transit.TravelModeBike, transit.TravelModeDrive} {
		t.Run(string(mode), func(t *testing.T) {
			fc := &valhalla.StubClient{
				IsochroneResp: cannedIso(),
				MatrixResp:    &valhalla.MatrixResponse{SourcesToTargets: [][]valhalla.MatrixCell{cells}},
			}
			chainer := isochrone.New(fc, &fakeGraph{nodes: nodes}, limits, isochrone.EgressCache{}, logger.Discard(), nil)
			if _, err := chainer.Chain(context.Background(), req(90, mode)); err != nil {
				t.Fatalf("Chain: %v", err)
			}
			_, matrix := fc.Calls()
			if len(matrix) != 1 {
				t.Fatalf("Matrix calls = %d, want 1", len(matrix))
			}
			if got := len(matrix[0].Destinations); got != nodeCount {
				t.Errorf("Matrix destinations = %d, want all %d — the access route cap is transit's alone", got, nodeCount)
			}
		})
	}
}

func TestChainer_countsTransitAccessLegsByWhetherTheyRode(t *testing.T) {
	stationA := node("station-a", -122.39, 37.71)
	stationB := node("station-b", -122.41, 37.69)
	matrixWithStarterRoute := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{valhalla.Cell(600, tokenRoadKm), valhalla.Cell(600, tokenRoadKm)},
			},
		},
		RouteFn: func(valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
			route := routedAccess(600)
			route.RodeTransit = true
			return route, nil
		},
	}

	tests := []struct {
		name      string
		mode      transit.TravelMode
		stub      *valhalla.StubClient
		wantErr   bool
		wantTrue  uint64
		wantFalse uint64
	}{
		{
			name: "rode and walked",
			mode: transit.TravelModeTransit,
			stub: &valhalla.StubClient{
				IsochroneResp: cannedIso(),
				RouteFn: func(r valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
					route := routedAccess(600)
					if r.Destination.Lat == stationA.Lat && r.Destination.Lng == stationA.Lng {
						route.RodeTransit = true
					}
					return route, nil
				},
			},
			wantTrue:  1,
			wantFalse: 1,
		},
		{
			name:      "walked the whole way",
			mode:      transit.TravelModeTransit,
			stub:      transitAccessClient(600),
			wantFalse: 2,
		},
		{
			name: "rode every access leg",
			mode: transit.TravelModeTransit,
			stub: &valhalla.StubClient{
				IsochroneResp: cannedIso(),
				RouteFn: func(valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
					route := routedAccess(600)
					route.RodeTransit = true
					return route, nil
				},
			},
			wantTrue: 2,
		},
		{
			name: "walk uses matrix",
			mode: transit.TravelModeWalk,
			stub: matrixWithStarterRoute,
		},
		{
			name: "bike uses matrix",
			mode: transit.TravelModeBike,
			stub: matrixWithStarterRoute,
		},
		{
			name: "drive uses matrix",
			mode: transit.TravelModeDrive,
			stub: matrixWithStarterRoute,
		},
		{
			name: "nil route is not counted",
			mode: transit.TravelModeTransit,
			stub: &valhalla.StubClient{
				IsochroneResp: cannedIso(),
				RouteFn: func(valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
					return nil, nil
				},
			},
		},
		{
			name: "bad request drop is not counted",
			mode: transit.TravelModeTransit,
			stub: &valhalla.StubClient{
				IsochroneResp: cannedIso(),
				RouteFn: func(r valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
					if r.Destination.Lat == stationB.Lat && r.Destination.Lng == stationB.Lng {
						return nil, fmt.Errorf("%w: no matching transit service near this location (error_code 171)", valhalla.ErrBadRequest)
					}
					route := routedAccess(600)
					route.RodeTransit = true
					return route, nil
				},
			},
			wantTrue: 1,
		},
		{
			name: "nil sibling is not counted",
			mode: transit.TravelModeTransit,
			stub: &valhalla.StubClient{
				IsochroneResp: cannedIso(),
				RouteFn: func(r valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
					if r.Destination.Lat == stationB.Lat && r.Destination.Lng == stationB.Lng {
						return nil, nil
					}
					route := routedAccess(600)
					route.RodeTransit = true
					return route, nil
				},
			},
			wantTrue: 1,
		},
		{
			name: "sibling upstream is not counted",
			mode: transit.TravelModeTransit,
			stub: &valhalla.StubClient{
				IsochroneResp: cannedIso(),
				RouteFn: func(r valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
					if r.Destination.Lat == stationB.Lat && r.Destination.Lng == stationB.Lng {
						return nil, valhalla.ErrUpstream
					}
					route := routedAccess(600)
					route.RodeTransit = true
					return route, nil
				},
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &metrics.Set{}
			chainer := isochrone.New(tc.stub, newTestGraph(), valhalla.DefaultLimits(), isochrone.EgressCache{}, logger.Discard(), m)
			_, err := chainer.Chain(context.Background(), req(90, tc.mode))
			if tc.wantErr {
				if err == nil {
					t.Fatal("Chain succeeded, want error")
				}
			} else if err != nil {
				t.Fatalf("Chain: %v", err)
			}
			if got := m.TransitAccessCount(true); got != tc.wantTrue {
				t.Errorf("TransitAccessCount(true) = %d, want %d", got, tc.wantTrue)
			}
			if got := m.TransitAccessCount(false); got != tc.wantFalse {
				t.Errorf("TransitAccessCount(false) = %d, want %d", got, tc.wantFalse)
			}
		})
	}
}
