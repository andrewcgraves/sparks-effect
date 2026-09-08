package isochrone_test

import (
	"context"
	"math"
	"testing"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/isochrone"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

// --- Trip progress -------------------------------------------------------
//
// A station the budget does not quite reach used to leave no trace at all: no
// dot, no polygon, no mention, and a rider who got 91% of the way there saw
// exactly what a rider who never left saw. SPA-264 reports how far along each
// unfinished leg the budget would have carried them, so the map can draw a stub
// along the alignment rather than stopping dead.
//
// Everything here goes through the chainer's one public call against a fake
// routing client, like the forty-odd cases already in this package. Progress is
// metadata on the same response, so there is no new seam to assert from.

// progressGraph is interchangeGraph's shape — a trunk with a spur off its
// middle station — with two things the frontier cases need that the original
// does not carry: every hop names the corridor it runs over and where its
// endpoints sit along it, and the trunk continues one station past the fork so
// that "nothing beyond the frontier" has somewhere to not be drawn.
//
//	alpha --trunk--> beta --trunk--> gamma --trunk--> epsilon   (rt-trunk)
//	                  \---spur--> delta                          (rt-spur)
//
// The spur's chainages descend — delta sits nearer the start of rt-spur than
// beta does — because an alignment drawn in the opposite direction to a journey
// is an authoring detail nobody sees, and it must not be a special case.
//
// Timings, from an access leg of 300 s to alpha, with the trunk charging a
// 600 s boarding wait and the spur none (a transfer is free):
//
//	alpha    300
//	beta    1800  = 300 + 900 + 600 wait
//	gamma   2700  = 1800 + 900
//	delta   2400  = 1800 + 600
//	epsilon 3600  = 2700 + 900
func progressGraph() transit.CompiledGraph {
	return transit.CompiledGraph{Graph: &transit.TransitGraph{
		Nodes: []transit.GraphNode{
			{Slug: "alpha", Lat: 37.70, Lng: -122.40},
			{Slug: "beta", Lat: 37.71, Lng: -122.40},
			{Slug: "gamma", Lat: 37.72, Lng: -122.40},
			{Slug: "delta", Lat: 37.71, Lng: -122.41},
			{Slug: "epsilon", Lat: 37.73, Lng: -122.40},
		},
		Services: []transit.ServiceGraph{
			{ServiceID: "trunk", WaitSecs: 600, Edges: []transit.Edge{
				{FromSlug: "alpha", ToSlug: "beta", Seconds: 900, DwellS: 60,
					RouteID: "rt-trunk", FromChainageM: 0, ToChainageM: 10000},
				{FromSlug: "beta", ToSlug: "gamma", Seconds: 900, DwellS: 45,
					RouteID: "rt-trunk", FromChainageM: 10000, ToChainageM: 25000},
				{FromSlug: "gamma", ToSlug: "epsilon", Seconds: 900, DwellS: 45,
					RouteID: "rt-trunk", FromChainageM: 25000, ToChainageM: 40000},
			}},
			{ServiceID: "spur", WaitSecs: 0, Edges: []transit.Edge{
				{FromSlug: "beta", ToSlug: "delta", Seconds: 600, DwellS: 30,
					RouteID: "rt-spur", FromChainageM: 4000, ToChainageM: 1000},
			}},
		},
	}}
}

// walkableFromNode is walkableFrom for a graph of any size: only the station at
// index is within walking reach of the origin.
func walkableFromNode(accessSecs int, index, nodeCount int) *valhalla.MatrixResponse {
	row := make([]valhalla.MatrixCell, nodeCount)
	row[index] = valhalla.Cell(accessSecs, tokenRoadKm)
	return &valhalla.MatrixResponse{SourcesToTargets: [][]valhalla.MatrixCell{row}}
}

func progressBySlug(resp *isochrone.ChainResponse) map[string]isochrone.TripProgress {
	out := make(map[string]isochrone.TripProgress, len(resp.Metadata.TripProgress))
	for _, p := range resp.Metadata.TripProgress {
		out[p.ToSlug] = p
	}
	return out
}

// chainOverProgressGraph runs one chain over progressGraph at the given whole-
// minute budget, walking to alpha in 300 s.
func chainOverProgressGraph(t *testing.T, budgetMins int) *isochrone.ChainResponse {
	t.Helper()
	fc := &valhalla.StubClient{IsochroneResp: cannedIso(), MatrixResp: walkableFromNode(300, 0, 5)}
	resp, err := newChainer(fc, progressGraph()).Chain(context.Background(), req(budgetMins, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	return resp
}

func assertFraction(t *testing.T, label string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Errorf("%s fraction = %v, want %v", label, got, want)
	}
}

// At a 35-minute budget the rider reaches alpha and beta and runs out partway
// along both hops out of beta. Each fork gets exactly one entry, and neither
// names the station it fell short of as reached.
//
// This is the whole of "which legs get an entry": the tree frontier, one entry
// per unreached station whose predecessor was reached.
func TestChainer_progressIsEmittedOncePerBranchAtTheFrontier(t *testing.T) {
	resp := chainOverProgressGraph(t, 35)

	stations := stationsBySlug(t, resp)
	if len(stations) != 2 {
		t.Fatalf("reachable stations = %v, want just alpha and beta", stations)
	}
	for _, unreached := range []string{"gamma", "delta", "epsilon"} {
		if _, listed := stations[unreached]; listed {
			t.Errorf("%s is listed as reachable; falling short of it must not light its dot", unreached)
		}
	}

	progress := progressBySlug(resp)
	if len(progress) != 2 {
		t.Fatalf("trip progress = %+v, want one entry per fork out of beta", resp.Metadata.TripProgress)
	}

	// gamma: ride is the hop less the dwell it would have served on arrival,
	// 900 − 45 = 855 s, against 2100 − 1800 = 300 s of budget left at beta.
	gamma := progress["gamma"]
	if gamma.FromSlug != "beta" || gamma.ServiceID != "trunk" || gamma.RouteID != "rt-trunk" {
		t.Errorf("gamma progress = %+v, want a trunk hop out of beta on rt-trunk", gamma)
	}
	if gamma.RideSecs != 855 {
		t.Errorf("gamma RideSecs = %d, want 855 (900 less the 45 s dwell)", gamma.RideSecs)
	}
	if gamma.RemainingSecs != 300 {
		t.Errorf("gamma RemainingSecs = %d, want 300", gamma.RemainingSecs)
	}
	assertFraction(t, "gamma", gamma.Fraction, 300.0/855.0)
	if gamma.FromChainageM != 10000 || gamma.ToChainageM != 25000 {
		t.Errorf("gamma chainages = (%v, %v), want (10000, 25000) copied from the edge",
			gamma.FromChainageM, gamma.ToChainageM)
	}

	// delta runs over the other corridor, and its chainages descend. Both facts
	// have to survive: a stub drawn down rt-trunk, or drawn from 1000 to 4000
	// instead of 4000 to 1000, would point at the wrong place entirely.
	delta := progress["delta"]
	if delta.RouteID != "rt-spur" {
		t.Errorf("delta RouteID = %q, want rt-spur — each leg follows its own corridor", delta.RouteID)
	}
	if delta.FromChainageM != 4000 || delta.ToChainageM != 1000 {
		t.Errorf("delta chainages = (%v, %v), want (4000, 1000) descending",
			delta.FromChainageM, delta.ToChainageM)
	}
	assertFraction(t, "delta", delta.Fraction, 300.0/570.0)
}

// epsilon is two hops past the budget: its own predecessor, gamma, is not
// reached either. Nothing is drawn for it, because the map must not speculate
// about stations beyond the first one a branch fails to reach.
func TestChainer_noProgressBeyondTheFrontier(t *testing.T) {
	resp := chainOverProgressGraph(t, 35)
	if p, drawn := progressBySlug(resp)["epsilon"]; drawn {
		t.Errorf("epsilon got progress %+v; its predecessor gamma is unreached, so it is past the frontier", p)
	}
}

// A rider whose budget covers the ride but not the dwell at the end of it
// physically arrives; only the modelled stop is unfinished. Drawing the stub
// all the way to the station is the truthful rendering, so the fraction clamps
// at one rather than running past it.
//
// It is not a promotion into the reachable set: the station stays unlit and
// gets no egress polygon, because the remaining seconds cannot support one.
func TestChainer_progressClampsWhenBudgetCoversTheRideButNotTheDwell(t *testing.T) {
	graph := transit.CompiledGraph{Graph: &transit.TransitGraph{
		Nodes: []transit.GraphNode{
			{Slug: "alpha", Lat: 37.70, Lng: -122.40},
			{Slug: "beta", Lat: 37.71, Lng: -122.40},
		},
		Services: []transit.ServiceGraph{{ServiceID: "svc", Edges: []transit.Edge{
			{FromSlug: "alpha", ToSlug: "beta", Seconds: 900, DwellS: 300,
				RouteID: "rt-1", FromChainageM: 0, ToChainageM: 12000},
		}}},
	}}
	fc := &valhalla.StubClient{IsochroneResp: cannedIso(), MatrixResp: walkableFromNode(300, 0, 2)}

	// 960 s of budget, 300 spent walking to alpha: 660 s left against a 600 s
	// ride. beta's own total is 1200, so it is still unreached.
	resp, err := newChainer(fc, graph).Chain(context.Background(), req(16, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}

	beta := progressBySlug(resp)["beta"]
	assertFraction(t, "beta", beta.Fraction, 1)
	if beta.RideSecs != 600 {
		t.Errorf("beta RideSecs = %d, want 600 — dwell is time standing still, not distance covered", beta.RideSecs)
	}
	if _, listed := stationsBySlug(t, resp)["beta"]; listed {
		t.Error("beta is listed as reachable; a clamped stub must not promote a station the rider did not complete")
	}
}

// The boarding wait is charged against the stub when the hop would be the
// rider's first ride, because waiting on a platform is time that buys no
// motion. A transfer costs nothing, which is what the wait model already says.
func TestChainer_progressChargesTheBoardingWaitOnlyOnAFirstRide(t *testing.T) {
	// 1200 s budget, 300 walking to alpha, 600 waiting for the trunk: 300 s
	// left against a 840 s ride. Without the wait deducted the rider would
	// appear to cover 900 of 840 seconds and the stub would clamp to the whole
	// hop, which is the failure this pins.
	resp := chainOverProgressGraph(t, 20)
	beta := progressBySlug(resp)["beta"]
	if beta.RemainingSecs != 300 {
		t.Errorf("beta RemainingSecs = %d, want 300 (the boarding wait buys no motion)", beta.RemainingSecs)
	}
	assertFraction(t, "beta", beta.Fraction, 300.0/840.0)

	// The forks out of beta are past the frontier here: beta is not reached.
	if len(resp.Metadata.TripProgress) != 1 {
		t.Errorf("trip progress = %+v, want only the unfinished first ride", resp.Metadata.TripProgress)
	}
}

// A rider who cannot even board before the budget expires has not travelled,
// and a stub would present waiting on a platform as motion.
func TestChainer_noProgressWhenTheFirstRideCannotBeBoarded(t *testing.T) {
	// 900 s budget, 300 walking to alpha, 600 waiting: nothing left to ride on.
	resp := chainOverProgressGraph(t, 15)
	if len(resp.Metadata.TripProgress) != 0 {
		t.Errorf("trip progress = %+v, want none: the budget is gone before the doors close",
			resp.Metadata.TripProgress)
	}
}

// "I boarded and barely moved" is still shown. There is no fraction floor: any
// floor would recreate this ticket's own fault at a smaller scale, and a floor
// expressed as a fraction means wildly different things on a 400 km hop and an
// 800 m one.
func TestChainer_aVeryShortStubIsStillDrawn(t *testing.T) {
	// 901 s is not a whole minute, so the smallest budget above the boarding
	// point is 16 minutes: 960 − 300 − 600 = 60 s against an 840 s ride.
	resp := chainOverProgressGraph(t, 16)
	beta := progressBySlug(resp)["beta"]
	assertFraction(t, "beta", beta.Fraction, 60.0/840.0)
	if beta.Fraction <= 0 {
		t.Error("a barely-moved stub was suppressed; it should be drawn, however short")
	}
}

// A hop whose edge names no corridor cannot be drawn, and progress is
// decoration: the isochrone is computed and returned entirely as normal. This
// is the shape every graph compiled before SPA-264 has, so it is also what a
// deploy that has not yet recompiled looks like.
func TestChainer_edgeWithoutARouteYieldsNoProgressAndAnUnaffectedIsochrone(t *testing.T) {
	graph := transit.CompiledGraph{Graph: &transit.TransitGraph{
		Nodes: []transit.GraphNode{
			{Slug: "alpha", Lat: 37.70, Lng: -122.40},
			{Slug: "beta", Lat: 37.71, Lng: -122.40},
		},
		Services: []transit.ServiceGraph{{ServiceID: "svc", Edges: []transit.Edge{
			{FromSlug: "alpha", ToSlug: "beta", Seconds: 900, DwellS: 60},
		}}},
	}}
	fc := &valhalla.StubClient{IsochroneResp: cannedIso(), MatrixResp: walkableFromNode(300, 0, 2)}

	resp, err := newChainer(fc, graph).Chain(context.Background(), req(16, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}
	if len(resp.Metadata.TripProgress) != 0 {
		t.Errorf("trip progress = %+v, want none from an edge with no route", resp.Metadata.TripProgress)
	}
	if _, reached := stationsBySlug(t, resp)["alpha"]; !reached {
		t.Error("alpha is not reachable; a missing route must not disturb the isochrone")
	}
	if len(resp.Features) == 0 {
		t.Error("no features; a cosmetic overlay must never be able to fail a computation")
	}
}

// The search settles each access station separately and keeps, per node, the
// cheapest journey found across all of them (graphSearch). So the best way to
// reach A and the journey that reaches B *through* A need not come from the
// same source — and when they differ, the budget left at A on B's journey is
// not the budget left at A on A's own best journey.
//
// Reading the two out of the merged map independently overstated the fraction:
// it measured the remainder from A's cheapest arrival while deducting a
// boarding wait paid at some other station on some other service.
//
//	s --x(300s, 50s wait)--> a --y(1000s, 600s wait)--> b
//
// Walking: s at 100 s, a at 300 s. The cheapest way to a is the walk (300),
// but the cheapest way to b rides through a from s: 100 + 300 + 50 = 450 at a,
// then 1000 more = 1450, the transfer free. At a 1200 s budget the rider is at
// a at 450 s already aboard, so 750 s of the 1000 s ride is bought.
func TestChainer_progressMeasuresTheJourneyThatActuallyReachesTheStation(t *testing.T) {
	graph := transit.CompiledGraph{Graph: &transit.TransitGraph{
		Nodes: []transit.GraphNode{
			{Slug: "s", Lat: 37.700, Lng: -122.400},
			{Slug: "a", Lat: 37.701, Lng: -122.400},
			{Slug: "b", Lat: 37.702, Lng: -122.400},
		},
		Services: []transit.ServiceGraph{
			{ServiceID: "x", WaitSecs: 50, Edges: []transit.Edge{
				{FromSlug: "s", ToSlug: "a", Seconds: 300,
					RouteID: "rt-1", FromChainageM: 0, ToChainageM: 3000},
			}},
			{ServiceID: "y", WaitSecs: 600, Edges: []transit.Edge{
				{FromSlug: "a", ToSlug: "b", Seconds: 1000,
					RouteID: "rt-1", FromChainageM: 3000, ToChainageM: 13000},
			}},
		},
	}}
	fc := &valhalla.StubClient{
		IsochroneResp: cannedIso(),
		MatrixResp: &valhalla.MatrixResponse{SourcesToTargets: [][]valhalla.MatrixCell{{
			valhalla.Cell(100, tokenRoadKm),
			valhalla.Cell(300, tokenRoadKm),
			{}, // b is not walkable; it is only ever ridden to
		}}},
	}

	resp, err := newChainer(fc, graph).Chain(context.Background(), req(20, transit.TravelModeWalk))
	if err != nil {
		t.Fatalf("Chain: %v", err)
	}

	progress := progressBySlug(resp)
	b, drawn := progress["b"]
	if !drawn {
		t.Fatalf("no progress toward b; got %+v", resp.Metadata.TripProgress)
	}
	if b.RemainingSecs != 750 {
		t.Errorf("RemainingSecs = %d, want 750 — measured from the arrival at a on b's own journey, "+
			"not from a's cheapest arrival less a wait paid elsewhere", b.RemainingSecs)
	}
	assertFraction(t, "b", b.Fraction, 750.0/1000.0)
}
