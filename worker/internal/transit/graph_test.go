package transit_test

import (
	"container/heap"
	"encoding/json"
	"fmt"
	"math/rand"
	"testing"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
)

func graph(services ...transit.ServiceGraph) transit.CompiledGraph {
	return transit.CompiledGraph{Graph: &transit.TransitGraph{Services: services}}
}

func svc(id string, waitSecs int, edges ...transit.Edge) transit.ServiceGraph {
	return transit.ServiceGraph{ServiceID: id, WaitSecs: waitSecs, Edges: edges}
}

func edge(from, to string, secs int) transit.Edge {
	return transit.Edge{FromSlug: from, ToSlug: to, Seconds: secs}
}

func from(slug string, accessSecs int) map[string]int {
	return map[string]int{slug: accessSecs}
}

func TestSearch_chargesWaitOnceAcrossManyHops(t *testing.T) {
	g := graph(svc("express", 300,
		edge("a", "b", 600),
		edge("b", "c", 600),
		edge("c", "d", 600),
	))

	got, ok := g.Search(from("a", 0))["d"]
	if !ok {
		t.Fatal("Search did not reach d")
	}
	if got.TotalSecs != 2100 {
		t.Errorf("TotalSecs = %d, want 2100 (three 600 s hops plus one 300 s wait)", got.TotalSecs)
	}
	if got.BoardService != "express" {
		t.Errorf("BoardService = %q, want express", got.BoardService)
	}
	if got.BoardWaitSecs != 300 {
		t.Errorf("BoardWaitSecs = %d, want 300 charged once, not per hop", got.BoardWaitSecs)
	}
}

func TestSearch_interchangesAcrossServicesSharingANode(t *testing.T) {
	g := graph(
		svc("north-line", 120, edge("a", "junction", 600)),
		svc("south-line", 180, edge("junction", "z", 900)),
	)

	got, ok := g.Search(from("a", 0))["z"]
	if !ok {
		t.Fatal("Search did not reach z through the shared node")
	}

	if got.TotalSecs != 1620 {
		t.Errorf("TotalSecs = %d, want 1620 (1500 s riding, one 120 s wait)", got.TotalSecs)
	}
	if got.BoardService != "north-line" {
		t.Errorf("BoardService = %q, want the service boarded first", got.BoardService)
	}
	if got.HopService != "south-line" {
		t.Errorf("HopService = %q, want the service ridden into z", got.HopService)
	}
	if got.PredecessorSlug != "junction" {
		t.Errorf("PredecessorSlug = %q, want junction", got.PredecessorSlug)
	}
}

func TestSearch_minimisesRideTimePlusWait(t *testing.T) {
	g := graph(

		svc("infrequent-express", 1800, edge("a", "z", 600)),

		svc("frequent-local", 120, edge("a", "z", 1200)),
	)

	got := g.Search(from("a", 0))["z"]
	if got.BoardService != "frequent-local" {
		t.Errorf("BoardService = %q, want frequent-local — it arrives sooner once waiting is counted", got.BoardService)
	}
	if got.TotalSecs != 1320 {
		t.Errorf("TotalSecs = %d, want 1320", got.TotalSecs)
	}
}

func TestSearch_reportsUnreachableNodesByOmission(t *testing.T) {
	g := graph(svc("line", 0, edge("a", "b", 600)))

	reached := g.Search(from("a", 0))
	if _, ok := reached["elsewhere"]; ok {
		t.Error("Search reached a node with no edges into it")
	}

	if _, ok := g.Search(from("b", 0))["a"]; ok {
		t.Error("Search rode b -> a, but edges are directed")
	}
}

func TestSearch_honoursDifferentDurationsEachWayAcrossOnePair(t *testing.T) {
	g := graph(svc("ca-hsr-local", 0,
		edge("gilroy", "merced", 3140),
		edge("merced", "gilroy", 3030),
	))

	southbound, ok := g.Search(from("gilroy", 0))["merced"]
	if !ok {
		t.Fatal("Search did not reach merced from gilroy")
	}
	northbound, ok := g.Search(from("merced", 0))["gilroy"]
	if !ok {
		t.Fatal("Search did not reach gilroy from merced")
	}

	if southbound.TotalSecs != 3140 {
		t.Errorf("gilroy -> merced = %d s, want 3140", southbound.TotalSecs)
	}
	if northbound.TotalSecs != 3030 {
		t.Errorf("merced -> gilroy = %d s, want 3030 — the reverse edge carries its own duration", northbound.TotalSecs)
	}
	if southbound.TotalSecs == northbound.TotalSecs {
		t.Errorf("both directions settled at %d s; the pair's two edges were collapsed into one", southbound.TotalSecs)
	}
}

func TestSearch_corridorTotalsDifferByDirection(t *testing.T) {
	g := graph(svc("ca-hsr-local", 0,

		edge("sf", "gilroy", 1200), edge("gilroy", "sf", 1200),
		edge("gilroy", "merced", 3140), edge("merced", "gilroy", 3030),
		edge("merced", "fresno", 900), edge("fresno", "merced", 900),
	))

	down := g.Search(from("sf", 0))["fresno"]
	up := g.Search(from("fresno", 0))["sf"]

	if down.TotalSecs != 5240 {
		t.Errorf("sf -> fresno = %d s, want 5240 (1200 + 3140 + 900)", down.TotalSecs)
	}
	if up.TotalSecs != 5130 {
		t.Errorf("fresno -> sf = %d s, want 5130 (900 + 3030 + 1200)", up.TotalSecs)
	}
}

func TestSearch_carriesDwellPerDirectionOfAPair(t *testing.T) {
	g := graph(svc("ca-hsr-local", 0,
		transit.Edge{FromSlug: "gilroy", ToSlug: "merced", Seconds: 3140, DwellS: 90},
		transit.Edge{FromSlug: "merced", ToSlug: "gilroy", Seconds: 3075, DwellS: 45},
	))

	southbound := g.Search(from("gilroy", 0))["merced"]
	if southbound.HopSecs != 3140 || southbound.HopDwellS != 90 {
		t.Errorf("hop into merced = %d s with %d s dwell, want 3140 and 90",
			southbound.HopSecs, southbound.HopDwellS)
	}
	northbound := g.Search(from("merced", 0))["gilroy"]
	if northbound.HopSecs != 3075 || northbound.HopDwellS != 45 {
		t.Errorf("hop into gilroy = %d s with %d s dwell, want 3075 and 45",
			northbound.HopSecs, northbound.HopDwellS)
	}
}

func TestSearch_accessStationCostsOnlyItsAccessLeg(t *testing.T) {
	g := graph(svc("line", 300, edge("a", "b", 600)))

	got, ok := g.Search(from("a", 420))["a"]
	if !ok {
		t.Fatal("Search did not report the access station itself")
	}
	if got.TotalSecs != 420 {
		t.Errorf("TotalSecs = %d, want the 420 s access leg alone", got.TotalSecs)
	}
	if got.PredecessorSlug != "" {
		t.Errorf("PredecessorSlug = %q, want empty — the rider rode nothing to get here", got.PredecessorSlug)
	}
	if got.SourceSlug != "a" {
		t.Errorf("SourceSlug = %q, want a", got.SourceSlug)
	}
}

func TestSearch_picksTheCheapestAccessStationPerNode(t *testing.T) {
	g := graph(
		svc("slow-from-near", 0, edge("near", "z", 3000)),
		svc("fast-from-far", 0, edge("far", "z", 600)),
	)

	got := g.Search(map[string]int{"near": 60, "far": 900})["z"]
	if got.TotalSecs != 1500 {
		t.Errorf("TotalSecs = %d, want 1500 — the longer walk to the faster service", got.TotalSecs)
	}
	if got.SourceSlug != "far" || got.PredecessorSlug != "far" {
		t.Errorf("journey came via %q from %q, want both to be far", got.SourceSlug, got.PredecessorSlug)
	}
}

func TestSearch_ridesThroughAnAccessStationRatherThanPayingItsWait(t *testing.T) {
	g := graph(svc("line", 1200,
		edge("a", "b", 300),
		edge("b", "z", 600),
	))

	got := g.Search(map[string]int{"a": 0, "b": 60})["z"]

	if got.TotalSecs != 1860 {
		t.Errorf("TotalSecs = %d, want 1860 (board at b)", got.TotalSecs)
	}
	if got.BoardSlug != "b" {
		t.Errorf("BoardSlug = %q, want b", got.BoardSlug)
	}
	if atB := g.Search(map[string]int{"a": 0, "b": 60})["b"]; atB.TotalSecs != 60 {
		t.Errorf("b reported at %d s, want the 60 s walk", atB.TotalSecs)
	}
}

func TestSearch_carriesPerHopDwell(t *testing.T) {
	g := graph(svc("line", 0,
		transit.Edge{FromSlug: "a", ToSlug: "b", Seconds: 645, DwellS: 45},
	))

	got := g.Search(from("a", 0))["b"]
	if got.HopSecs != 645 || got.HopDwellS != 45 {
		t.Errorf("hop into b = %d s with %d s dwell, want 645 and 45", got.HopSecs, got.HopDwellS)
	}
}

func TestSearch_isDeterministicAcrossRuns(t *testing.T) {
	g := graph(
		svc("west", 0, edge("west-end", "z", 600)),
		svc("east", 0, edge("east-end", "z", 600)),
	)
	sources := map[string]int{"west-end": 300, "east-end": 300}

	first := g.Search(sources)["z"]
	for i := 0; i < 25; i++ {
		if got := g.Search(sources)["z"]; got != first {
			t.Fatalf("run %d settled on %+v, want %+v every time", i, got, first)
		}
	}
}

func TestNodes_carriesPositions(t *testing.T) {
	g := transit.CompiledGraph{Graph: &transit.TransitGraph{
		Nodes: []transit.GraphNode{
			{Slug: "a", Lat: 37.7, Lng: -122.4, Names: []string{"A", "Also A"}},
		},
	}}

	nodes := g.Nodes()
	if len(nodes) != 1 {
		t.Fatalf("Nodes() = %d nodes, want 1", len(nodes))
	}
	if nodes[0] != (transit.Node{Slug: "a", Lat: 37.7, Lng: -122.4}) {
		t.Errorf("node = %+v, want slug and position carried through", nodes[0])
	}
}

func TestNodes_reportsRoutingAnchorWhenSet(t *testing.T) {
	anchorLat, anchorLng := 36.0545, -115.1706
	g := transit.CompiledGraph{Graph: &transit.TransitGraph{
		Nodes: []transit.GraphNode{
			{Slug: "las-vegas", Lat: 36.0545, Lng: -115.1778, RoutingLat: &anchorLat, RoutingLng: &anchorLng},
			{Slug: "palmdale", Lat: 34.58, Lng: -118.12},
		},
	}}

	nodes := g.Nodes()
	byslug := make(map[string]transit.Node, len(nodes))
	for _, n := range nodes {
		byslug[n.Slug] = n
	}

	lv := byslug["las-vegas"]
	if lv.Lat != anchorLat || lv.Lng != anchorLng {
		t.Errorf("las-vegas node = (%v, %v), want the routing anchor (%v, %v)", lv.Lat, lv.Lng, anchorLat, anchorLng)
	}

	pl := byslug["palmdale"]
	if pl.Lat != 34.58 || pl.Lng != -118.12 {
		t.Errorf("palmdale node = (%v, %v), want its own Lat/Lng unchanged (no routing anchor set)", pl.Lat, pl.Lng)
	}
}

func TestHasWait(t *testing.T) {
	if graph(svc("a", 0, edge("x", "y", 60)), svc("b", 0, edge("y", "z", 60))).HasWait() {
		t.Error("HasWait() = true for a graph whose services all declare zero wait")
	}
	if !graph(svc("a", 0, edge("x", "y", 60)), svc("b", 300, edge("y", "z", 60))).HasWait() {
		t.Error("HasWait() = false though one service declares a wait")
	}
}

func TestNilGraphIsHarmless(t *testing.T) {
	var g transit.CompiledGraph

	if nodes := g.Nodes(); len(nodes) != 0 {
		t.Errorf("Nodes() = %v, want none", nodes)
	}
	if reached := g.Search(from("a", 0)); len(reached) != 0 {
		t.Errorf("Search on a nil graph reached %v", reached)
	}
	if g.HasWait() {
		t.Error("HasWait() = true on a nil graph")
	}
}

func TestMergeIsCarriedButNotModelled(t *testing.T) {
	const body = `{"services":[],"merge":{"clusters":[{"key":"a"}],"near_misses":[]},"nodes":[]}`

	var g transit.TransitGraph
	if err := json.Unmarshal([]byte(body), &g); err != nil {
		t.Fatalf("decoding a graph with a merge report: %v", err)
	}
	if len(g.Merge) == 0 {
		t.Error("Merge is empty; the report was dropped on decode")
	}
	var report map[string]any
	if err := json.Unmarshal(g.Merge, &report); err != nil {
		t.Fatalf("Merge did not survive as valid JSON: %v", err)
	}
	if _, ok := report["clusters"]; !ok {
		t.Errorf("Merge = %v, want the clusters key preserved", report)
	}
}

func TestEdgeWithoutDwellDecodesAsZero(t *testing.T) {
	var e transit.Edge
	if err := json.Unmarshal([]byte(`{"from_slug":"a","to_slug":"b","seconds":600}`), &e); err != nil {
		t.Fatalf("decoding a pre-dwell edge: %v", err)
	}
	if e.Seconds != 600 || e.DwellS != 0 {
		t.Errorf("edge = %+v, want 600 s and no dwell", e)
	}
}

func referenceTravelTime(g *transit.TransitGraph, src, dst string) (secs, wait int, ok bool) {
	if src == dst {
		return 0, 0, true
	}
	type neighbor struct {
		slug     string
		secs     int
		waitSecs int
	}
	adj := make(map[string][]neighbor)
	for _, sg := range g.Services {
		for _, e := range sg.Edges {
			adj[e.FromSlug] = append(adj[e.FromSlug], neighbor{e.ToSlug, e.Seconds, sg.WaitSecs})
		}
	}

	type pathState struct{ vehicleSecs, waitSecs int }
	best := map[string]pathState{src: {}}
	h := &refHeap{{src, 0}}
	heap.Init(h)

	for h.Len() > 0 {
		cur := heap.Pop(h).(refNode)
		state := best[cur.slug]
		if cur.secs > state.vehicleSecs+state.waitSecs {
			continue
		}
		if cur.slug == dst {
			return state.vehicleSecs, state.waitSecs, true
		}
		for _, nb := range adj[cur.slug] {
			next := pathState{state.vehicleSecs + nb.secs, state.waitSecs}
			if cur.slug == src {
				next.waitSecs = nb.waitSecs
			}
			total := next.vehicleSecs + next.waitSecs
			if prev, seen := best[nb.slug]; !seen || total < prev.vehicleSecs+prev.waitSecs {
				best[nb.slug] = next
				heap.Push(h, refNode{nb.slug, total})
			}
		}
	}
	return 0, 0, false
}

func referenceReach(g *transit.TransitGraph, nodes []string, accessSecs map[string]int) map[string]int {
	out := make(map[string]int)
	for _, n := range nodes {
		for accessSlug, aSecs := range accessSecs {
			total := aSecs
			if accessSlug != n {
				rideSecs, waitSecs, ok := referenceTravelTime(g, accessSlug, n)
				if !ok {
					continue
				}
				total += rideSecs + waitSecs
			}
			if prev, seen := out[n]; !seen || total < prev {
				out[n] = total
			}
		}
	}
	return out
}

type refNode struct {
	slug string
	secs int
}

type refHeap []refNode

func (h refHeap) Len() int           { return len(h) }
func (h refHeap) Less(i, j int) bool { return h[i].secs < h[j].secs }
func (h refHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *refHeap) Push(x any)        { *h = append(*h, x.(refNode)) }
func (h *refHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

func assertSearchMatchesReference(t *testing.T, name string, g *transit.TransitGraph, nodes []string, accessSecs map[string]int) {
	t.Helper()

	want := referenceReach(g, nodes, accessSecs)
	gotAll := transit.CompiledGraph{Graph: g}.Search(accessSecs)

	got := make(map[string]int, len(want))
	for _, n := range nodes {
		if r, ok := gotAll[n]; ok {
			got[n] = r.TotalSecs
		}
	}
	for _, n := range nodes {
		w, wOK := want[n]
		gv, gOK := got[n]
		switch {
		case wOK != gOK:
			t.Errorf("%s: node %q reachable = %v, want %v", name, n, gOK, wOK)
		case wOK && w != gv:
			t.Errorf("%s: node %q settled at %d s, want %d s", name, n, gv, w)
		}
	}
}

func TestSearch_matchesThePerPairSearchItReplaced(t *testing.T) {
	cases := []struct {
		name     string
		services []transit.ServiceGraph
		nodes    []string
		access   map[string]int
	}{
		{
			name:     "one line, one access station",
			services: []transit.ServiceGraph{svc("line", 300, edge("a", "b", 600), edge("b", "c", 600))},
			nodes:    []string{"a", "b", "c"},
			access:   map[string]int{"a": 120},
		},
		{
			name: "two access stations on one line",
			services: []transit.ServiceGraph{svc("line", 600,
				edge("a", "b", 300), edge("b", "c", 300), edge("c", "d", 300))},
			nodes:  []string{"a", "b", "c", "d"},
			access: map[string]int{"a": 60, "c": 900},
		},
		{
			name: "an access station worth riding through rather than boarding at",
			services: []transit.ServiceGraph{svc("line", 1200,
				edge("a", "b", 300), edge("b", "z", 600))},
			nodes:  []string{"a", "b", "z"},
			access: map[string]int{"a": 0, "b": 60},
		},
		{
			name: "interchange between two services at a junction",
			services: []transit.ServiceGraph{
				svc("trunk", 900, edge("a", "junction", 1800), edge("junction", "a", 1800)),
				svc("spur", 1800, edge("junction", "z", 1200), edge("z", "junction", 1200)),
			},
			nodes:  []string{"a", "junction", "z"},
			access: map[string]int{"a": 300, "junction": 4000},
		},
		{
			name: "competing services between the same pair",
			services: []transit.ServiceGraph{
				svc("express", 1800, edge("a", "z", 600)),
				svc("local", 120, edge("a", "m", 400), edge("m", "z", 500)),
			},
			nodes:  []string{"a", "m", "z"},
			access: map[string]int{"a": 200},
		},
		{
			name:     "a node no service calls at",
			services: []transit.ServiceGraph{svc("line", 0, edge("a", "b", 600))},
			nodes:    []string{"a", "b", "orphan"},
			access:   map[string]int{"a": 100, "orphan": 700},
		},
		{
			name:     "no access stations at all",
			services: []transit.ServiceGraph{svc("line", 300, edge("a", "b", 600))},
			nodes:    []string{"a", "b"},
			access:   map[string]int{},
		},
	}

	for _, tc := range cases {
		g := &transit.TransitGraph{Services: tc.services}
		assertSearchMatchesReference(t, tc.name, g, tc.nodes, tc.access)
	}
}

func TestSearch_matchesThePerPairSearchOnRandomGraphs(t *testing.T) {
	rng := rand.New(rand.NewSource(223))

	for run := 0; run < 200; run++ {
		nodeCount := 3 + rng.Intn(8)
		nodes := make([]string, nodeCount)
		for i := range nodes {
			nodes[i] = fmt.Sprintf("n%02d", i)
		}

		serviceCount := 1 + rng.Intn(3)
		services := make([]transit.ServiceGraph, serviceCount)
		for s := range services {
			sg := transit.ServiceGraph{
				ServiceID: fmt.Sprintf("svc-%d", s),
				WaitSecs:  rng.Intn(5) * 300,
			}
			for i := range nodes {
				for j := range nodes {
					if i == j || rng.Intn(4) != 0 {
						continue
					}
					sg.Edges = append(sg.Edges, edge(nodes[i], nodes[j], 60+rng.Intn(3000)))
				}
			}
			services[s] = sg
		}

		access := make(map[string]int)
		for _, n := range nodes {
			if rng.Intn(3) == 0 {
				access[n] = rng.Intn(2400)
			}
		}
		if len(access) == 0 {
			access[nodes[0]] = rng.Intn(2400)
		}

		g := &transit.TransitGraph{Services: services}
		assertSearchMatchesReference(t, fmt.Sprintf("random run %d", run), g, nodes, access)
	}
}

func TestTravelModeValid(t *testing.T) {
	for _, m := range transit.TravelModes() {
		if !m.Valid() {
			t.Errorf("%q is in TravelModes but Valid is false", m)
		}
	}
	if !transit.TravelModeTransit.Valid() {
		t.Error("transit must be a valid mode — the API already enqueues it (SPA-248)")
	}
	for _, m := range []transit.TravelMode{"", "teleport", "multimodal", "pedestrian", "Transit", "transit "} {
		if m.Valid() {
			t.Errorf("%q Valid = true, want false", m)
		}
	}
}
