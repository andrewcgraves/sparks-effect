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
type Edge struct {
	FromSlug string `json:"from_slug"`
	ToSlug   string `json:"to_slug"`
	Seconds  int    `json:"seconds"`
}

// ServiceGraph is one service's hops plus the boarding wait a rider pays to
// join it. WaitSecs is charged once, at the point of boarding, not per hop.
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
type GraphNode struct {
	Slug  string   `json:"slug"`
	Lat   float64  `json:"lat"`
	Lng   float64  `json:"lng"`
	Names []string `json:"names"`
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

// GraphData is the read-only view of one compiled graph that the chainer
// computes over.
//
// It is an interface rather than a bare *TransitGraph so the chainer's tests
// can supply a graph without building one, which is how the migrated suite
// from sparks-effect-api expresses most of its cases.
type GraphData interface {
	// Nodes lists every point an isochrone could be plotted from.
	Nodes() []Node
	// TravelTimeBetween reports the quickest ride from one node to another,
	// split into time aboard and time waiting to board, plus the service the
	// rider boards. ok is false when no path exists.
	TravelTimeBetween(fromSlug, toSlug string) (seconds, waitSecs int, serviceID string, ok bool)
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
func (d CompiledGraph) Nodes() []Node {
	if d.Graph == nil {
		return nil
	}
	nodes := make([]Node, len(d.Graph.Nodes))
	for i, n := range d.Graph.Nodes {
		nodes[i] = Node{Slug: n.Slug, Lat: n.Lat, Lng: n.Lng}
	}
	return nodes
}

// TravelTimeBetween runs Dijkstra over the graph's pooled service edges.
func (d CompiledGraph) TravelTimeBetween(fromSlug, toSlug string) (seconds, waitSecs int, serviceID string, ok bool) {
	if d.Graph == nil {
		return 0, 0, "", false
	}
	if fromSlug == toSlug {
		return 0, 0, "", true
	}
	return graphDijkstra(d.Graph, fromSlug, toSlug)
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

// graphDijkstra finds the quickest from→to path over every service's edges
// pooled into one adjacency map.
//
// Pooling by slug is the whole of interchange in this system: there is no
// transfer edge type and no transfer table, so two services connect if and only
// if they emit an edge naming the same key. The API's merge step is what
// arranges for them to.
//
// Wait is charged once, on the first hop, and then carried unchanged: a rider
// waits for the service they board and rides through everything after. That
// makes the cost being minimised vehicle time plus one boarding wait, which is
// why the two are tracked separately rather than summed into the queue key.
func graphDijkstra(g *TransitGraph, from, to string) (int, int, string, bool) {
	type neighbor struct {
		slug      string
		secs      int
		serviceID string
		waitSecs  int
	}
	adj := make(map[string][]neighbor)
	for _, sg := range g.Services {
		for _, e := range sg.Edges {
			adj[e.FromSlug] = append(adj[e.FromSlug], neighbor{
				slug:      e.ToSlug,
				secs:      e.Seconds,
				serviceID: sg.ServiceID,
				waitSecs:  sg.WaitSecs,
			})
		}
	}

	type pathState struct {
		vehicleSecs int
		waitSecs    int
		serviceID   string
	}
	best := map[string]pathState{from: {}}
	h := &dijkHeap{{from, 0}}
	heap.Init(h)

	for h.Len() > 0 {
		cur := heap.Pop(h).(dijkNode)
		curState := best[cur.slug]
		if cur.secs > curState.vehicleSecs+curState.waitSecs {
			continue
		}
		if cur.slug == to {
			return curState.vehicleSecs, curState.waitSecs, curState.serviceID, true
		}
		for _, nb := range adj[cur.slug] {
			nextVehicle := curState.vehicleSecs + nb.secs
			nextWait := curState.waitSecs
			nextService := curState.serviceID
			if cur.slug == from {
				nextWait = nb.waitSecs
				nextService = nb.serviceID
			}
			nextTotal := nextVehicle + nextWait
			if prev, seen := best[nb.slug]; !seen || nextTotal < prev.vehicleSecs+prev.waitSecs {
				best[nb.slug] = pathState{nextVehicle, nextWait, nextService}
				heap.Push(h, dijkNode{nb.slug, nextTotal})
			}
		}
	}
	return 0, 0, "", false
}

type dijkNode struct {
	slug string
	secs int
}

type dijkHeap []dijkNode

func (h dijkHeap) Len() int           { return len(h) }
func (h dijkHeap) Less(i, j int) bool { return h[i].secs < h[j].secs }
func (h dijkHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *dijkHeap) Push(x any)        { *h = append(*h, x.(dijkNode)) }
func (h *dijkHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}
