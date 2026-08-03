package transit_test

import (
	"encoding/json"
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

// Boarding wait is charged once, at the point of boarding, and not again per
// hop: a rider waits for the service they board and then rides through
// everything after. Charging it per hop would understate reachability badly on
// any service with more than a couple of stops.
func TestTravelTimeBetween_chargesWaitOnceAcrossManyHops(t *testing.T) {
	g := graph(svc("express", 300,
		edge("a", "b", 600),
		edge("b", "c", 600),
		edge("c", "d", 600),
	))

	secs, wait, service, ok := g.TravelTimeBetween("a", "d")
	if !ok {
		t.Fatal("TravelTimeBetween(a, d) not ok, want a path")
	}
	if secs != 1800 {
		t.Errorf("vehicle seconds = %d, want 1800 (three 600 s hops)", secs)
	}
	if wait != 300 {
		t.Errorf("wait seconds = %d, want 300 charged once, not per hop", wait)
	}
	if service != "express" {
		t.Errorf("service = %q, want express", service)
	}
}

// Two services sharing a node key are connected, because every service's edges
// are pooled into one adjacency map. That pooling is the whole of interchange
// in this system — there is no transfer edge type and no transfer table.
func TestTravelTimeBetween_interchangesAcrossServicesSharingANode(t *testing.T) {
	g := graph(
		svc("north-line", 120, edge("a", "junction", 600)),
		svc("south-line", 180, edge("junction", "z", 900)),
	)

	secs, wait, service, ok := g.TravelTimeBetween("a", "z")
	if !ok {
		t.Fatal("TravelTimeBetween(a, z) not ok, want a path through the shared node")
	}
	if secs != 1500 {
		t.Errorf("vehicle seconds = %d, want 1500", secs)
	}
	// The wait reported is the one paid to board first. The second boarding is
	// not charged, matching the single-wait model above.
	if wait != 120 {
		t.Errorf("wait seconds = %d, want 120 (the first boarding)", wait)
	}
	if service != "north-line" {
		t.Errorf("service = %q, want the service boarded first", service)
	}
}

// The quantity minimised is vehicle time plus one boarding wait — not vehicle
// time alone. A nominally faster service that leaves less often can be the
// slower way to arrive, and the search has to see that.
func TestTravelTimeBetween_minimisesRideTimePlusWait(t *testing.T) {
	g := graph(
		// Quick ride, long wait: 600 + 1800 = 2400.
		svc("infrequent-express", 1800, edge("a", "z", 600)),
		// Slower ride, short wait: 1200 + 120 = 1320.
		svc("frequent-local", 120, edge("a", "z", 1200)),
	)

	secs, wait, service, ok := g.TravelTimeBetween("a", "z")
	if !ok {
		t.Fatal("TravelTimeBetween(a, z) not ok")
	}
	if service != "frequent-local" {
		t.Errorf("service = %q, want frequent-local — it arrives sooner once waiting is counted", service)
	}
	if secs+wait != 1320 {
		t.Errorf("total = %d, want 1320", secs+wait)
	}
}

func TestTravelTimeBetween_reportsNoPath(t *testing.T) {
	g := graph(svc("line", 0, edge("a", "b", 600)))

	if _, _, _, ok := g.TravelTimeBetween("a", "elsewhere"); ok {
		t.Error("TravelTimeBetween(a, elsewhere) ok, want no path")
	}
	// Edges are directed, so the reverse of a one-way edge is not a path.
	if _, _, _, ok := g.TravelTimeBetween("b", "a"); ok {
		t.Error("TravelTimeBetween(b, a) ok, want no path — edges are directed")
	}
}

// A node reaches itself at no cost. The chainer relies on this for the station
// a rider walks directly to and does not ride from.
func TestTravelTimeBetween_selfIsFree(t *testing.T) {
	g := graph(svc("line", 300, edge("a", "b", 600)))

	secs, wait, _, ok := g.TravelTimeBetween("a", "a")
	if !ok {
		t.Fatal("TravelTimeBetween(a, a) not ok, want a free path")
	}
	if secs != 0 || wait != 0 {
		t.Errorf("self path = %d s + %d s wait, want free", secs, wait)
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
	if _, _, _, ok := g.TravelTimeBetween("a", "b"); ok {
		t.Error("TravelTimeBetween on a nil graph reported a path")
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
