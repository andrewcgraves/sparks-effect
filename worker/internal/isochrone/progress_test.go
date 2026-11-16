package isochrone_test

import (
	"context"
	"math"
	"testing"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/isochrone"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/transit"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

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

func TestChainer_noProgressBeyondTheFrontier(t *testing.T) {
	resp := chainOverProgressGraph(t, 35)
	if p, drawn := progressBySlug(resp)["epsilon"]; drawn {
		t.Errorf("epsilon got progress %+v; its predecessor gamma is unreached, so it is past the frontier", p)
	}
}

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

func TestChainer_progressChargesTheBoardingWaitOnlyOnAFirstRide(t *testing.T) {
	resp := chainOverProgressGraph(t, 20)
	beta := progressBySlug(resp)["beta"]
	if beta.RemainingSecs != 300 {
		t.Errorf("beta RemainingSecs = %d, want 300 (the boarding wait buys no motion)", beta.RemainingSecs)
	}
	assertFraction(t, "beta", beta.Fraction, 300.0/840.0)

	if len(resp.Metadata.TripProgress) != 1 {
		t.Errorf("trip progress = %+v, want only the unfinished first ride", resp.Metadata.TripProgress)
	}
}

func TestChainer_noProgressWhenTheFirstRideCannotBeBoarded(t *testing.T) {
	resp := chainOverProgressGraph(t, 15)
	if len(resp.Metadata.TripProgress) != 0 {
		t.Errorf("trip progress = %+v, want none: the budget is gone before the doors close",
			resp.Metadata.TripProgress)
	}
}

func TestChainer_aVeryShortStubIsStillDrawn(t *testing.T) {
	resp := chainOverProgressGraph(t, 16)
	beta := progressBySlug(resp)["beta"]
	assertFraction(t, "beta", beta.Fraction, 60.0/840.0)
	if beta.Fraction <= 0 {
		t.Error("a barely-moved stub was suppressed; it should be drawn, however short")
	}
}

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
			{},
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
