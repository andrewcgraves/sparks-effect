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

// from names one access station at the cost of walking to it, which is what a
// search is seeded with.
func from(slug string, accessSecs int) map[string]int {
	return map[string]int{slug: accessSecs}
}

// Boarding wait is charged once, at the point of boarding, and not again per
// hop: a rider waits for the service they board and then rides through
// everything after. Charging it per hop would understate reachability badly on
// any service with more than a couple of stops.
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

// Two services sharing a node key are connected, because every service's edges
// are pooled into one adjacency map. That pooling is the whole of interchange
// in this system — there is no transfer edge type and no transfer table.
func TestSearch_interchangesAcrossServicesSharingANode(t *testing.T) {
	g := graph(
		svc("north-line", 120, edge("a", "junction", 600)),
		svc("south-line", 180, edge("junction", "z", 900)),
	)

	got, ok := g.Search(from("a", 0))["z"]
	if !ok {
		t.Fatal("Search did not reach z through the shared node")
	}
	// 1500 s of riding plus the 120 s wait paid to board first. The second
	// boarding is not charged, matching the single-wait model above.
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

// The quantity minimised is the access leg plus vehicle time plus one boarding
// wait — not vehicle time alone. A nominally faster service that leaves less
// often can be the slower way to arrive, and the search has to see that.
func TestSearch_minimisesRideTimePlusWait(t *testing.T) {
	g := graph(
		// Quick ride, long wait: 600 + 1800 = 2400.
		svc("infrequent-express", 1800, edge("a", "z", 600)),
		// Slower ride, short wait: 1200 + 120 = 1320.
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
	// Edges are directed, so the reverse of a one-way edge is not a path.
	if _, ok := g.Search(from("b", 0))["a"]; ok {
		t.Error("Search rode b -> a, but edges are directed")
	}
}

// An access station is reached at the cost of walking to it and nothing more.
// The chainer relies on this for the station a rider walks straight to and does
// not ride from.
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

// One search, many access stations: each node is reported against whichever
// access station leaves it cheapest, and the predecessor chain leads back to
// that station rather than to whichever was seeded first.
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

// Riding through an access station beats walking to it and boarding there when
// the wait avoided is worth more than the extra riding — the case a per-pair
// search covered by starting again at every access station, and the reason a
// label here is a node together with whether the rider has boarded yet.
func TestSearch_ridesThroughAnAccessStationRatherThanPayingItsWait(t *testing.T) {
	g := graph(svc("line", 1200,
		edge("a", "b", 300),
		edge("b", "z", 600),
	))

	got := g.Search(map[string]int{"a": 0, "b": 60})["z"]
	// Boarding at b costs 60 + 1200 + 600 = 1860. Boarding at a and staying on
	// costs 0 + 1200 + 300 + 600 = 2100 — worse. But b itself is reported at
	// the 60 s walk, not at the 1500 s ride, so the two labels differ.
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

// Dwell rides along on the hop it belongs to, so a consumer can say how long
// the vehicle stands at the node it arrived at.
func TestSearch_carriesPerHopDwell(t *testing.T) {
	g := graph(svc("line", 0,
		transit.Edge{FromSlug: "a", ToSlug: "b", Seconds: 645, DwellS: 45},
	))

	got := g.Search(from("a", 0))["b"]
	if got.HopSecs != 645 || got.HopDwellS != 45 {
		t.Errorf("hop into b = %d s with %d s dwell, want 645 and 45", got.HopSecs, got.HopDwellS)
	}
}

// The same query settles the same way twice. Predecessors decide the shape of
// the tree drawn from them, so a tie resolved by map iteration order would
// redraw the graph between two runs of one query.
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

// A node with a routing anchor (SPA-234) reports the anchor as its position,
// not its real Lat/Lng: every routing call — egress isochrone, matrix,
// route — needs a point Valhalla's graph can actually reach, and Node carries
// nothing that would need the real one instead.
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

// HasWait is what the chainer reports as its wait model, in place of the
// scenario slug the API used to special-case on.
func TestHasWait(t *testing.T) {
	if graph(svc("a", 0, edge("x", "y", 60)), svc("b", 0, edge("y", "z", 60))).HasWait() {
		t.Error("HasWait() = true for a graph whose services all declare zero wait")
	}
	if !graph(svc("a", 0, edge("x", "y", 60)), svc("b", 300, edge("y", "z", 60))).HasWait() {
		t.Error("HasWait() = false though one service declares a wait")
	}
}

// A nil graph is what a malformed message decodes to, and it must answer
// harmlessly rather than panicking — the decoder rejects it, and this is the
// belt to that braces.
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

// Merge is carried but never read, so it has to survive decoding without the
// worker modelling any of the four types behind it.
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

// Dwell decodes as zero on a graph compiled before the API reported it, which
// is what every persisted graph looks like until its scenario is compiled
// again. Absent and zero are the same thing on the wire, deliberately.
func TestEdgeWithoutDwellDecodesAsZero(t *testing.T) {
	var e transit.Edge
	if err := json.Unmarshal([]byte(`{"from_slug":"a","to_slug":"b","seconds":600}`), &e); err != nil {
		t.Fatalf("decoding a pre-dwell edge: %v", err)
	}
	if e.Seconds != 600 || e.DwellS != 0 {
		t.Errorf("edge = %+v, want 600 s and no dwell", e)
	}
}

// --- Equivalence with the per-pair search this replaced -------------------
//
// The search behind every isochrone in the product changed shape. What must
// not change is which stations a rider reaches and how much budget they have
// left on arrival, and that is what these cases hold to: the multi-source pass
// is checked against an independent transcription of the per-pair algorithm it
// replaced, over graphs built to provoke the disagreements worth having.
//
// The reference below is the old implementation, kept here rather than in the
// package so it cannot drift into sharing code with the thing it is checking.

// referenceTravelTime is the pairwise Dijkstra CompiledGraph.TravelTimeBetween
// used to run, transcribed. Wait is charged on the first hop out of the source
// and carried unchanged after.
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

// referenceReach is how the chainer used to work the answer out: every
// reachable access station tried against every node, keeping whichever left the
// most budget.
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

// The hand-written cases above cover the disagreements that were thought of.
// This one covers the ones that were not: random graphs, from a fixed seed so a
// failure is reproducible, exercising several services, both directions, and
// access sets ranging from one station to most of the graph.
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
