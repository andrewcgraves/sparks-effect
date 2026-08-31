// Package transit holds the compiled-graph model this worker reads off the
// queue, and the shortest-path search over it.
//
// It is deliberately a fraction of the type of the same name in
// sparks-effect-api. That repository compiles a graph out of routes, stations,
// services, vehicle types and track physics; this one only ever decodes one
// that has already been compiled. Everything needed to *build* a graph is
// therefore absent, and the fields below are exactly those the wire format
// carries — no more, so that a reader of this package is not misled into
// thinking the worker knows anything about how a graph came to be.
//
// The two repositories share no Go code, only the message body (see
// internal/routing) and the Postgres schema. That is a deliberate trade: these
// structs are duplicated, and the golden fixture in internal/routing/testdata
// is what stops the two copies drifting apart unnoticed.
package transit

import (
	"container/heap"
	"encoding/json"
	"sort"
)

// TravelMode is how a person covers the access and egress legs of an
// isochrone. It is the domain's own vocabulary and what the message and the
// routing_jobs row both carry.
//
// Valhalla calls the same concept "costing" and spells it pedestrian /
// bicycle / auto. That translation lives at the routing client's boundary, in
// internal/valhalla, and nowhere else.
type TravelMode string

const (
	TravelModeWalk  TravelMode = "walk"
	TravelModeBike  TravelMode = "bike"
	TravelModeDrive TravelMode = "drive"
)

// Valid reports whether m is one of the three modes. A message naming anything
// else is a permanent failure: no amount of retrying turns an unknown mode into
// a known one.
func (m TravelMode) Valid() bool {
	switch m {
	case TravelModeWalk, TravelModeBike, TravelModeDrive:
		return true
	default:
		return false
	}
}

// Edge is one directed hop along a service, in seconds of vehicle time.
//
// Seconds is the whole cost of the hop: the vehicle in motion plus the dwell it
// serves on arrival at ToSlug. DwellS reports that dwell on its own, so a
// consumer can say how long the vehicle stands there rather than watching it
// vanish into the total. It is additive and absent on graphs compiled before
// the API emitted it, which decode with a zero indistinguishable from a hop
// that really has no dwell — see the chainer's legs.
//
// A pair of stations served both ways is two of these, and their Seconds need
// not agree. Most hops are symmetric and the API compiles them that way, but a
// segment carrying reverse_run_seconds — CA HSR's Pacheco Pass and Tehachapi
// crossings, where northbound is the faster run (SPA-245) — compiles into two
// edges of different duration. The search reads only Edge.FromSlug, so this
// costs nothing here; it is recorded because folding a pair into one undirected
// hop would look like a harmless simplification and would move every isochrone
// crossing those spans. See
// TestSearch_honoursDifferentDurationsEachWayAcrossOnePair.
type Edge struct {
	FromSlug string `json:"from_slug"`
	ToSlug   string `json:"to_slug"`
	Seconds  int    `json:"seconds"`
	DwellS   int    `json:"dwell_s,omitempty"`
}

// ServiceGraph is one service's hops plus the boarding wait a rider pays to
// join it. WaitSecs is charged once, at the point of boarding, not per hop.
//
// The wire carries one more key here, wait_policy, and its absence below is
// deliberate. It names which of the API's boarding-wait policies produced
// WaitSecs — service override, scenario override, global default — which is a
// fact about how the graph was compiled, and this package models none of that
// (see the package comment). The number is what the search charges; where it
// came from is the API's business. Modelling it would only add a field to keep
// in sync for a value nothing here would read.
type ServiceGraph struct {
	ServiceID string `json:"service_id"`
	Edges     []Edge `json:"edges"`
	WaitSecs  int    `json:"wait_secs"`
}

// GraphNode is one addressable point in a compiled graph: the key every
// Edge.FromSlug/ToSlug refers to, plus the position an isochrone is plotted
// from and the display names that produced it.
//
// The position is why the worker needs no database of stations. A compiled
// user scenario's node keys are compile-time cluster keys persisted nowhere
// else, so the coordinate has to ride on the graph or it is unrecoverable.
//
// RoutingLat/RoutingLng are nil for almost every node: the API sets them only
// for a station whose real Lat/Lng the Valhalla graph cannot reach yet — a
// site still under construction has no walkable network OSM can see, so a
// pedestrian isochrone centred on it is rejected outright ("Locations are in
// unconnected regions"). See sparks-effect-api's Station.RoutingLocation
// (SPA-234). CompiledGraph.Nodes resolves them away immediately: nothing past
// that point has a reason to know the difference.
type GraphNode struct {
	Slug       string   `json:"slug"`
	Lat        float64  `json:"lat"`
	Lng        float64  `json:"lng"`
	RoutingLat *float64 `json:"routing_lat,omitempty"`
	RoutingLng *float64 `json:"routing_lng,omitempty"`
	Names      []string `json:"names"`
}

// TransitGraph is a compiled, Dijkstra-ready scenario: the services that run,
// the nodes they call at, and a report of how interchange was resolved.
//
// Merge is carried but never read. It records which stops the compiler folded
// onto one node and which it nearly did — a diagnostic aimed at whoever
// authored the scenario, not at anything computing over it, since the merge has
// already been applied to the slugs by the time the graph is emitted. It is
// held as raw JSON rather than modelled: giving it Go structure here would
// duplicate four more types from the API for a value this worker only ever
// skips past, and every one of them would be another thing to keep in sync.
type TransitGraph struct {
	Services []ServiceGraph  `json:"services"`
	Merge    json.RawMessage `json:"merge,omitempty"`
	Nodes    []GraphNode     `json:"nodes,omitempty"`
}

// Node is a graph node reduced to what routing actually needs: a key and a
// place. Display names are dropped — nothing in the chain renders a station.
type Node struct {
	Slug string
	Lat  float64
	Lng  float64
}

// Reached is the best journey a search found to one node: what it cost, and
// the hop that arrived there.
//
// The predecessor pointer is why the search returns this rather than a bare
// duration. Following it from any node walks back to the access station the
// journey began at, so the whole reachability tree is recoverable — which is
// what lets a caller describe the trip rather than only its total.
type Reached struct {
	// TotalSecs is everything elapsed on arrival: the access leg that began
	// this journey, the vehicle time along it — dwell included — and the one
	// boarding wait.
	TotalSecs int
	// SourceSlug is the access station the journey began at. It is the node
	// itself when the rider walked here and rode nothing.
	SourceSlug string
	// PredecessorSlug is the node ridden from on the hop into this one, and is
	// empty exactly when this node is where the rider walked to.
	PredecessorSlug string
	// HopService, HopSecs and HopDwellS describe that hop: the service ridden,
	// what it cost, and the dwell served on arrival, which HopSecs includes.
	HopSecs    int
	HopDwellS  int
	HopService string
	// BoardSlug, BoardService and BoardWaitSecs record the one boarding on this
	// journey: where the rider got on, what they got on, and what they waited
	// to do it. A journey that rode nothing has all three empty.
	BoardSlug     string
	BoardService  string
	BoardWaitSecs int
}

// GraphData is the read-only view of one compiled graph that the chainer
// computes over.
//
// It is an interface rather than a bare *TransitGraph so the chainer's tests
// can supply a graph without building one, which is how the migrated suite
// from sparks-effect-api expresses most of its cases.
type GraphData interface {
	// Nodes lists every point an isochrone could be plotted from.
	Nodes() []Node
	// Search reports, for every node the rider can get to, the cheapest
	// journey there from any of the access stations it is seeded with —
	// accessSecs maps an access station's slug to the seconds spent getting to
	// it. Nodes no journey reaches are absent from the result.
	Search(accessSecs map[string]int) map[string]Reached
	// HasWait reports whether any service charges a boarding wait. It is on
	// this interface, rather than read off the graph at the call site, because
	// the chainer reports it as metadata and has no other reason to look
	// inside a graph it is otherwise happy to treat as opaque.
	HasWait() bool
}

// CompiledGraph adapts a decoded TransitGraph onto GraphData. It is the only
// implementation used in production: the graph arrives inline on the message,
// already scoped to one scenario, so there is nothing left to resolve by slug.
type CompiledGraph struct {
	Graph *TransitGraph
}

var _ GraphData = CompiledGraph{}

// Nodes reports the graph's own node set, carrying the slug and the position
// the API attached at compile time.
//
// A node with a routing anchor (GraphNode.RoutingLat/RoutingLng) reports that
// point instead of its real Lat/Lng: every routing call this worker makes —
// egress isochrone, matrix, route — should ask Valhalla about the point that
// is actually reachable, and Node carries nothing else that would need the
// real position instead.
func (d CompiledGraph) Nodes() []Node {
	if d.Graph == nil {
		return nil
	}
	nodes := make([]Node, len(d.Graph.Nodes))
	for i, n := range d.Graph.Nodes {
		lat, lng := n.Lat, n.Lng
		if n.RoutingLat != nil && n.RoutingLng != nil {
			lat, lng = *n.RoutingLat, *n.RoutingLng
		}
		nodes[i] = Node{Slug: n.Slug, Lat: lat, Lng: lng}
	}
	return nodes
}

// Search runs one multi-source Dijkstra over the graph's pooled service edges.
func (d CompiledGraph) Search(accessSecs map[string]int) map[string]Reached {
	if d.Graph == nil || len(accessSecs) == 0 {
		return nil
	}
	return graphSearch(d.Graph, accessSecs)
}

// HasWait reports whether any service in the graph charges a boarding wait.
//
// It is what the chainer reports as its wait model, in place of the scenario
// slug the API used to special-case on. The queue message carries no scenario
// identity — only a compile job id and the graph itself — so "is this the
// wait-free scenario?" can no longer be asked by name, and is instead read off
// the graph, which is the thing that actually determines the answer.
func (d CompiledGraph) HasWait() bool {
	if d.Graph == nil {
		return false
	}
	for _, sg := range d.Graph.Services {
		if sg.WaitSecs > 0 {
			return true
		}
	}
	return false
}

// graphSearch settles every node reachable from any access station, over every
// service's edges pooled into one adjacency map.
//
// Pooling by slug is the whole of interchange in this system: there is no
// transfer edge type and no transfer table, so two services connect if and only
// if they emit an edge naming the same key. The API's merge step is what
// arranges for them to.
//
// Wait is charged once, on the first hop out of the access station the journey
// began at, and then carried unchanged: a rider waits for the service they
// board and rides through everything after, transfers included. The quantity
// minimised is therefore the access leg plus vehicle time plus that one wait.
//
// One settled search per access station, merged by keeping whichever left a
// node cheapest. That is a fraction of the work the pairwise search this
// replaced did — it ran a search per access-station/egress-station *pair* and
// threw the settled set away every time — and the parent pointers it keeps are
// what make the journey to a node recoverable rather than only its total.
//
// Not one pass seeded with every access station at once, which is the obvious
// simplification and is not equivalent. Charging the wait once makes the cost
// of a journey depend on where it started, and a single merged pass lets a
// journey ride back into its own access station already aboard and leave again
// without paying a wait it should have paid. That is a modelling artefact
// rather than a trip anyone would take, and it moves reachability, so the
// per-source shape is kept. See TestSearch_matchesThePerPairSearchItReplaced.
func graphSearch(g *TransitGraph, accessSecs map[string]int) map[string]Reached {
	adj := poolEdges(g)

	// Sources are walked in slug order, and ties on arrival cost go to the
	// first of them, so a run over the same graph settles the same way every
	// time. Predecessors decide the shape of the tree drawn from them, and a
	// tie left to Go's map iteration would redraw it between two runs of one
	// query.
	out := make(map[string]Reached)
	for _, src := range sortedKeys(accessSecs) {
		for slug, r := range settleFrom(adj, src, accessSecs[src]) {
			if prev, seen := out[slug]; !seen || r.TotalSecs < prev.TotalSecs {
				out[slug] = r
			}
		}
	}
	return out
}

type neighbor struct {
	slug      string
	secs      int
	dwellS    int
	serviceID string
	waitSecs  int
}

func poolEdges(g *TransitGraph) map[string][]neighbor {
	adj := make(map[string][]neighbor)
	for _, sg := range g.Services {
		for _, e := range sg.Edges {
			adj[e.FromSlug] = append(adj[e.FromSlug], neighbor{
				slug:      e.ToSlug,
				secs:      e.Seconds,
				dwellS:    e.DwellS,
				serviceID: sg.ServiceID,
				waitSecs:  sg.WaitSecs,
			})
		}
	}
	return adj
}

// settleFrom runs one Dijkstra out from a single access station, reached at the
// cost of walking to it, recording for every node it settles the hop that
// arrived there and the boarding that paid for the whole journey.
//
// The access station itself is settled at its access cost and never improved,
// which is what keeps a journey from riding back to where it started and
// leaving again for free.
func settleFrom(adj map[string][]neighbor, src string, accessSecs int) map[string]Reached {
	best := map[string]Reached{src: {TotalSecs: accessSecs, SourceSlug: src}}
	h := &dijkHeap{{src, accessSecs}}
	heap.Init(h)

	for h.Len() > 0 {
		cur := heap.Pop(h).(dijkNode)
		state := best[cur.slug]
		if cur.secs > state.TotalSecs {
			continue
		}
		for _, nb := range adj[cur.slug] {
			next := Reached{
				TotalSecs:       state.TotalSecs + nb.secs,
				SourceSlug:      src,
				PredecessorSlug: cur.slug,
				HopSecs:         nb.secs,
				HopDwellS:       nb.dwellS,
				HopService:      nb.serviceID,
				BoardSlug:       state.BoardSlug,
				BoardService:    state.BoardService,
				BoardWaitSecs:   state.BoardWaitSecs,
			}
			if cur.slug == src {
				next.TotalSecs += nb.waitSecs
				next.BoardSlug = src
				next.BoardService = nb.serviceID
				next.BoardWaitSecs = nb.waitSecs
			}
			if prev, seen := best[nb.slug]; !seen || next.TotalSecs < prev.TotalSecs {
				best[nb.slug] = next
				heap.Push(h, dijkNode{nb.slug, next.TotalSecs})
			}
		}
	}
	return best
}

func sortedKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type dijkNode struct {
	slug string
	secs int
}

type dijkHeap []dijkNode

func (h dijkHeap) Len() int { return len(h) }
func (h dijkHeap) Less(i, j int) bool {
	if h[i].secs != h[j].secs {
		return h[i].secs < h[j].secs
	}
	return h[i].slug < h[j].slug
}
func (h dijkHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *dijkHeap) Push(x any)   { *h = append(*h, x.(dijkNode)) }
func (h *dijkHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
