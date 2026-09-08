package transit

import (
	"container/heap"
	"encoding/json"
	"slices"
	"sort"
)

type TravelMode string

const (
	TravelModeWalk    TravelMode = "walk"
	TravelModeBike    TravelMode = "bike"
	TravelModeDrive   TravelMode = "drive"
	TravelModeTransit TravelMode = "transit"
)

var travelModes = []TravelMode{
	TravelModeWalk,
	TravelModeBike,
	TravelModeDrive,
	TravelModeTransit,
}

func (m TravelMode) Valid() bool {
	return slices.Contains(travelModes, m)
}

func TravelModes() []TravelMode {
	return slices.Clone(travelModes)
}

type Edge struct {
	FromSlug      string  `json:"from_slug"`
	ToSlug        string  `json:"to_slug"`
	Seconds       int     `json:"seconds"`
	DwellS        int     `json:"dwell_s,omitempty"`
	RouteID       string  `json:"route_id,omitempty"`
	FromChainageM float64 `json:"from_chainage_m,omitempty"`
	ToChainageM   float64 `json:"to_chainage_m,omitempty"`
}

type ServiceGraph struct {
	ServiceID string `json:"service_id"`
	Edges     []Edge `json:"edges"`
	WaitSecs  int    `json:"wait_secs"`
}

type GraphNode struct {
	Slug       string   `json:"slug"`
	Lat        float64  `json:"lat"`
	Lng        float64  `json:"lng"`
	RoutingLat *float64 `json:"routing_lat,omitempty"`
	RoutingLng *float64 `json:"routing_lng,omitempty"`
	Names      []string `json:"names"`
}

type TransitGraph struct {
	Services []ServiceGraph  `json:"services"`
	Merge    json.RawMessage `json:"merge,omitempty"`
	Nodes    []GraphNode     `json:"nodes,omitempty"`
}

type Node struct {
	Slug string
	Lat  float64
	Lng  float64
}

type Reached struct {
	TotalSecs        int
	SourceSlug       string
	PredecessorSlug  string
	HopSecs          int
	HopDwellS        int
	HopService       string
	HopRouteID       string
	HopFromChainageM float64
	HopToChainageM   float64
	BoardSlug        string
	BoardService     string
	BoardWaitSecs    int
}

type GraphData interface {
	Nodes() []Node
	Search(accessSecs map[string]int) map[string]Reached
	HasWait() bool
}

type CompiledGraph struct {
	Graph *TransitGraph
}

var _ GraphData = CompiledGraph{}

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

func (d CompiledGraph) Search(accessSecs map[string]int) map[string]Reached {
	if d.Graph == nil || len(accessSecs) == 0 {
		return nil
	}
	return graphSearch(d.Graph, accessSecs)
}

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

func graphSearch(g *TransitGraph, accessSecs map[string]int) map[string]Reached {
	adj := poolEdges(g)

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
	Edge
	serviceID string
	waitSecs  int
}

func poolEdges(g *TransitGraph) map[string][]neighbor {
	adj := make(map[string][]neighbor)
	for _, sg := range g.Services {
		for _, e := range sg.Edges {
			adj[e.FromSlug] = append(adj[e.FromSlug], neighbor{
				Edge:      e,
				serviceID: sg.ServiceID,
				waitSecs:  sg.WaitSecs,
			})
		}
	}
	return adj
}

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
				TotalSecs:        state.TotalSecs + nb.Seconds,
				SourceSlug:       src,
				PredecessorSlug:  cur.slug,
				HopSecs:          nb.Seconds,
				HopDwellS:        nb.DwellS,
				HopService:       nb.serviceID,
				HopRouteID:       nb.RouteID,
				HopFromChainageM: nb.FromChainageM,
				HopToChainageM:   nb.ToChainageM,
				BoardSlug:        state.BoardSlug,
				BoardService:     state.BoardService,
				BoardWaitSecs:    state.BoardWaitSecs,
			}
			if cur.slug == src {
				next.TotalSecs += nb.waitSecs
				next.BoardSlug = src
				next.BoardService = nb.serviceID
				next.BoardWaitSecs = nb.waitSecs
			}
			if prev, seen := best[nb.ToSlug]; !seen || next.TotalSecs < prev.TotalSecs {
				best[nb.ToSlug] = next
				heap.Push(h, dijkNode{nb.ToSlug, next.TotalSecs})
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
