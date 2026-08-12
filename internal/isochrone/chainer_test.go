package isochrone_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

// Search settles what the travel-time table states directly: every access
// station, and every node one stated ride away from one. The table is pairs
// rather than edges, so there is nothing here to chain through — a case about a
// multi-hop journey builds a real transit.CompiledGraph instead.
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

// tokenRoadKm is the road distance a stubbed matrix cell reports, for the many
// cases that are about a travel time and have no opinion about a distance.
//
// It cannot be a token zero. The chainer drops a cell whose distance undercuts
// the straight line to the station it names, because no road does that and a
// cell claiming one is a pair the server declined rather than routed (SPA-243).
// Every station in these fixtures sits within a few kilometres of the origin,
// so one comfortably longer value stands in for a real road to any of them.
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

// --- The journey the response describes ----------------------------------
//
// These cases drive the chainer over a real compiled graph rather than the
// travel-time table above, because what they are about is the shape of a
// journey across several hops and two services — which a table of pairs cannot
// state.

// interchangeGraph is a trunk with a spur off its middle station, the shape the
// seeded California scenario has at Palmdale: a rider bound for the spur's end
// boards the trunk, rides to the junction, and changes there.
//
//	alpha --trunk--> beta --trunk--> gamma
//	                  \--spur--> delta
//
// The stations sit a kilometre or so apart so that all four are within walking
// reach of the origin; which of them the rider can actually walk to is decided
// by the matrix, not by the geography.
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

// walkableFrom builds a matrix answer in which only the named station is within
// walking reach; every other node of interchangeGraph is unreachable on foot.
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

// Every station names the one it was ridden from, and following those names
// arrives at a station the rider walked to. That is what makes the reported set
// a tree rather than a list, and it is the whole reason the search keeps
// predecessors.
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

// A journey over an interchange is described hop by hop, each naming the
// service actually ridden — which is what the single boarded-service field
// cannot do, since it reports only the first.
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
	// A station the rider walked to rode nothing, so it has no legs at all.
	if alpha := stations["alpha"]; len(alpha.Legs) != 0 {
		t.Errorf("alpha legs = %+v, want none — the rider walked there", alpha.Legs)
	}
}

// Boarding wait is paid once, at the station the rider gets on. Changing
// service at the junction costs nothing, so the spur's longer wait is never
// charged — which is the model this change was required to preserve exactly.
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
	// 300 s walk + 600 s wait + 900 s to beta + 600 s on to delta, with the
	// spur's 1200 s wait never charged.
	if got := stations["delta"].RemainingSecs; got != 120*60-2400 {
		t.Errorf("delta remaining = %d s, want %d — a transfer costs nothing", got, 120*60-2400)
	}
}

// Seconds are reported alongside the whole minutes that were always there, so a
// caller adding hop to hop down a journey does not lose a minute at every stop.
// The minute fields keep meaning exactly what they meant.
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

// A graph compiled before the API reported dwell carries none, and the response
// describes the journey anyway — minus the dwell, which is incomplete but never
// wrong. No backfill is required to deploy this.
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
					valhalla.Cell(600, tokenRoadKm),  // station-a: 10 mins
					valhalla.Cell(3000, tokenRoadKm), // station-b: 50 mins
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

	// 1 origin feature + 1 egress feature: station-a's best path is a direct
	// walk (no polygon, since SPA-188), station-b's is via transit through
	// station-a (gets one).
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
	// budget=30 min=1800s, station-a access=600s, transit a→b=1800s: 600+1800=2400 > 1800 budget
	// station-b direct: unreachable (-1)
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{
					valhalla.Cell(600, tokenRoadKm), // station-a: 10 mins
					{},                              // station-b: unreachable
				},
			},
		},
	}
	chainer := newChainer(fc, newTestGraph())

	resp, err := chainer.Chain(context.Background(), req(30, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	// station-a listed, reached by direct walk (no egress polygon, per
	// SPA-188); station-b excluded entirely (1800-600-1800 = -600s ≤ 0).
	if len(resp.Metadata.ReachableStations) != 1 {
		t.Fatalf("ReachableStations: want 1, got %d", len(resp.Metadata.ReachableStations))
	}
	if resp.Metadata.ReachableStations[0].StationSlug != "station-a" {
		t.Errorf("station slug: want station-a, got %q", resp.Metadata.ReachableStations[0].StationSlug)
	}
}

// SPA-188: a station reached purely by walking — no transit boarded — is just
// "how far you can walk from a station you already walked to," which the
// origin polygon already covers. It gets no isochrone feature of its own, but
// it still stays in the reachable-stations list: a later ticket (SPA-191)
// draws a walking path to it off that entry.
func TestChainer_directAccess_AequalsB(t *testing.T) {
	// Single station, directly reachable; no transit leg.
	graph := &fakeGraph{nodes: []transit.Node{node("station-a", -122.39, 37.71)}}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{valhalla.Cell(600, tokenRoadKm)}, // 10 mins
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
	if rs.ViaService != "" {
		t.Errorf("ViaService: want empty (no service boarded), got %q", rs.ViaService)
	}
	// Origin feature only — no egress polygon for a walk-only station.
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
	// One hub reachable on foot, five stations reachable only by riding a
	// service from it, to trigger concurrent egress fan-out. A directly
	// walkable station gets no egress isochrone at all since SPA-188, so the
	// fan-out this test means to exercise needs stations reached by transit.
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

// The egress fan-out is as wide as the graph has stations reached by transit,
// and the routing server is a single small container. Without a bound, one
// large scenario would open hundreds of concurrent requests against a server
// sized for a handful — so the bound is measured, not just configured.
func TestChainer_egressFanOutIsBoundedByConfiguration(t *testing.T) {
	const nodeCount = 40
	const concurrency = 3

	// One hub reachable on foot; every other node reached only by riding a
	// service from it, since a directly walkable station gets no egress
	// isochrone since SPA-188 and so would never enter this fan-out.
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
	chainer := isochrone.New(fc, &fakeGraph{nodes: nodes, travelTimes: tt}, limits, isochrone.EgressCache{}, logger.Discard())

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
		cells[i] = valhalla.MatrixCell{}
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
	// station-a is directly reachable but, since SPA-188, a walk-only station
	// gets no egress isochrone at all — so this needs a station reached by
	// transit for there to be an egress budget to check.
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
				{valhalla.Cell(600, tokenRoadKm), {}}, // hub: 10 min access
			},
		},
	}
	chainer := newChainer(fc, graph)

	// budget 90 min − 10 min access − 1 min transit = 79 min remaining at station-a.
	resp, err := chainer.Chain(context.Background(), req(90, transit.TravelModeDrive))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	sa := findStation(t, resp, "station-a")
	if sa.RemainingMins != 79 {
		t.Fatalf("RemainingMins: want 79, got %d", sa.RemainingMins)
	}
	// Match on origin coordinates rather than call order, which errgroup doesn't fix.
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

// Valhalla's contour is minute-granular, so a station with under a minute of
// budget left would go out as {"time": 0} — which Valhalla rejects, failing the
// whole errgroup and with it the entire request. Such stations are dropped.
func TestChainer_subMinuteRemainingBudget_stationDropped(t *testing.T) {
	graph := &fakeGraph{nodes: []transit.Node{node("station-a", -122.39, 37.71)}}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{
			// 600 s budget − 570 s access = 30 s remaining → 0 whole minutes.
			SourcesToTargets: [][]valhalla.MatrixCell{{valhalla.Cell(570, tokenRoadKm)}},
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
	// station-b must be reached by transit, not walked to directly: a
	// walk-only station makes no egress request at all since SPA-188, so it
	// could never fail one.
	graph := &fakeGraph{
		nodes: []transit.Node{
			node("station-a", -122.39, 37.71),
			node("station-b", -122.41, 37.69),
		},
		travelTimes: map[[2]string]int{{"station-a", "station-b"}: 1800},
	}
	fc := &valhalla.StubClient{
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{
				{valhalla.Cell(600, tokenRoadKm), {}},
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
					valhalla.Cell(600, tokenRoadKm),  // hsr-a: 10 min
					valhalla.Cell(3000, tokenRoadKm), // hsr-b: 50 min direct
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
					valhalla.Cell(600, tokenRoadKm), // station-a: 10 min
					{},                              // station-b: not directly reachable
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
	// station-a must be reached by transit, not walked to directly: a
	// walk-only station carries no egress feature at all since SPA-188.
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

// cannedRoute is a routed shape that is unmistakably not a two-point segment:
// it bends, the way a walk along streets does, and its endpoints are Valhalla's
// snapped ones rather than either of the points that were asked about.
func cannedRoute() *valhalla.RouteResponse {
	return &valhalla.RouteResponse{Shape: []valhalla.LatLng{
		{Lat: 37.7001, Lng: -122.4002},
		{Lat: 37.7040, Lng: -122.4002},
		{Lat: 37.7040, Lng: -122.3930},
		{Lat: 37.7098, Lng: -122.3901},
	}}
}

// starterWalkGeometry decodes the LineString the chain reports, so a case can
// assert on coordinates rather than on bytes.
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

// The point of the whole ticket: what is drawn is the shape that was walked,
// not the chord across whatever lies between the two points.
func TestChainer_starterWalkCarriesTheRoutedShape(t *testing.T) {
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteResp:     cannedRoute(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{
				valhalla.Cell(600, tokenRoadKm),  // station-a: 10 mins on foot
				valhalla.Cell(3000, tokenRoadKm), // station-b: 50 mins on foot
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
	// Endpoints are Valhalla's, not the request origin (37.7, -122.4) or the
	// graph node (37.71, -122.39): the line starts and ends where the timing
	// actually started and ended.
	if coords[0][0] != -122.4002 || coords[0][1] != 37.7001 {
		t.Errorf("first point = %v, want Valhalla's snapped origin [-122.4002 37.7001]", coords[0])
	}
	if coords[3][0] != -122.3901 || coords[3][1] != 37.7098 {
		t.Errorf("last point = %v, want Valhalla's snapped station [-122.3901 37.7098]", coords[3])
	}
}

// Only the starter station gets a route. Every other station's access time
// comes from the matrix and none of them need geometry: they are reached by
// riding, and their egress isochrones already say what that buys.
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
	// Asked from the request's own origin and to the graph node's coordinates:
	// the two ends of the leg the matrix timed.
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

// A plot that reached no station on foot has no walking leg to draw, and asking
// Valhalla to route to nowhere would be a request with no destination.
//
// This is also the only shape that case takes. Every station in the accounting
// was reached from some station walked to from the origin, and that station's
// own remaining budget is the largest of any it reaches — so a plot with any
// reachable station at all has a walked-to one among them.
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

// The starter is the shortest walk, and whole-minute ties are broken by slug so
// the same plot names the same station every time. Access times are truncated
// to minutes before the comparison, which is what makes those ties common —
// station-b is 12 seconds nearer here and still loses, because both walks are
// reported as 10 minutes and the slug decides.
func TestChainer_starterIsTheShortestWalkThenTheSlug(t *testing.T) {
	graph := &fakeGraph{
		nodes: []transit.Node{
			node("station-a", -122.39, 37.71),
			node("station-b", -122.41, 37.69),
		},
	}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteResp:     cannedRoute(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{
				valhalla.Cell(612, tokenRoadKm), // station-a: 10 mins
				valhalla.Cell(600, tokenRoadKm), // station-b: 10 mins
			}},
		},
	}

	resp, err := newChainer(fc, graph).Chain(context.Background(), req(90, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if resp.Metadata.StarterWalk == nil || resp.Metadata.StarterWalk.StationSlug != "station-a" {
		t.Errorf("StarterWalk = %+v, want station-a on the slug tie-break", resp.Metadata.StarterWalk)
	}

	// And a station a clear minute nearer wins outright, tie-break or not.
	fc2 := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		RouteResp:     cannedRoute(),
		MatrixResp: &valhalla.MatrixResponse{
			SourcesToTargets: [][]valhalla.MatrixCell{{
				valhalla.Cell(660, tokenRoadKm), // station-a: 11 mins
				valhalla.Cell(600, tokenRoadKm), // station-b: 10 mins
			}},
		},
	}
	resp2, err := newChainer(fc2, graph).Chain(context.Background(), req(90, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if resp2.Metadata.StarterWalk == nil || resp2.Metadata.StarterWalk.StationSlug != "station-b" {
		t.Errorf("StarterWalk = %+v, want station-b as the shorter walk", resp2.Metadata.StarterWalk)
	}
}

// SPA-243: a pair Valhalla declined to route is not a station next door.
//
// It declines by answering nulls, which decode to a zero time beside a zero
// distance, and taking that at face value makes a station a hundred kilometres
// away the nearest thing in the plot: nearer than the one across the road, so
// the access leg is drawn to it and the search is seeded with the rider already
// standing on its platform. Both spellings of "no route" are covered, and so is
// the third shape the same nonsense takes — a road shorter than the straight
// line it is supposed to span, which no road is.
//
// Drive is where this bit. It is the only mode whose reach admits a station far
// enough away for the routing graph not to have it at all.
func TestChainer_matrixPairThatWasNeverRoutedIsNotAnAccessStation(t *testing.T) {
	// far-station is some 150 km off: within a four-hour drive, and nowhere
	// near the origin by any measure.
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
					valhalla.Cell(600, tokenRoadKm), // near-station: a 10-minute drive
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

	// And the rule bites only on the impossible: a genuinely long drive, stated
	// as a genuinely long road, is still an access leg.
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

// A station the routing graph cannot answer for is a gap in the map data, and
// the plot is the only thing that ever notices. Dropping it silently is how
// SPA-243 stayed invisible until it produced a visibly wrong plot, so each drop
// names its station at a level that is on in production.
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
				valhalla.MatrixCell{},  // no route found
				valhalla.Cell(60, 0.5), // a road shorter than the line it spans
			}},
		},
	}

	var buf bytes.Buffer
	chainer := isochrone.New(fc, graph, valhalla.DefaultLimits(), isochrone.EgressCache{}, logger.New(&buf, false))
	if _, err := chainer.Chain(context.Background(), req(240, transit.TravelModeDrive)); err != nil {
		t.Fatalf("Chain: %v", err)
	}

	logged := buf.String()
	for _, want := range []string{"unrouted-station", "implausible-station"} {
		if !strings.Contains(logged, want) {
			t.Errorf("%s was dropped without being named in the log:\n%s", want, logged)
		}
	}
	// The station that was answered for is not a drop, so it has nothing to
	// report — a log line per station would bury the two that matter.
	if strings.Contains(logged, "near-station") {
		t.Errorf("near-station was reached and should not appear in the log:\n%s", logged)
	}
}

// A station reached by riding is never the starter, however short the hop: the
// walking line is the leg before boarding anything.
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

// The routed leg is annotation on a surface that is already complete without
// it. An origin dropped somewhere Valhalla cannot route from — a lake, an
// island — must lose the line, not the plot.
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
	// The accounting the surface rests on is untouched.
	if len(resp.Metadata.ReachableStations) != 2 {
		t.Errorf("ReachableStations = %d, want 2", len(resp.Metadata.ReachableStations))
	}
	if len(resp.Features) != 2 {
		t.Errorf("Features = %d, want 2", len(resp.Features))
	}
}

// The extra call is issued alongside the egress fan-out rather than after it.
// The fan-out is the bulk of a chain's routing work, so a route that waited for
// it would add its own latency to every plot for nothing.
//
// It cannot be issued alongside the matrix, as SPA-196 supposed: which station
// is the starter is only known once the matrix has said how long each walk
// takes. Alongside the fan-out is the earliest it can go.
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
			// The origin polygon precedes the matrix, so it precedes any
			// knowledge of which station the starter is; only the fan-out is
			// the thing the route should overlap.
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
