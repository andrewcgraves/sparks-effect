package valhalla_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/geo"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

func TestSyntheticIsochroneIsADrawablePolygon(t *testing.T) {
	client := valhalla.NewSyntheticClient()
	origin := valhalla.LatLng{Lat: 37.7, Lng: -122.4}

	resp, err := client.Isochrone(context.Background(), valhalla.IsochroneRequest{
		Origin: origin, Costing: valhalla.CostingPedestrian, BudgetSecs: 3600,
	})
	if err != nil {
		t.Fatalf("Isochrone: %v", err)
	}
	if len(resp.Features) != 1 {
		t.Fatalf("Features: want 1, got %d", len(resp.Features))
	}

	var feature struct {
		Type       string         `json:"type"`
		Properties map[string]any `json:"properties"`
		Geometry   struct {
			Type        string        `json:"type"`
			Coordinates [][][]float64 `json:"coordinates"`
		} `json:"geometry"`
	}
	if err := json.Unmarshal(resp.Features[0], &feature); err != nil {
		t.Fatalf("feature is not valid JSON: %v", err)
	}

	if feature.Type != "Feature" || feature.Geometry.Type != "Polygon" {
		t.Errorf("got %s/%s, want Feature/Polygon", feature.Type, feature.Geometry.Type)
	}

	if feature.Properties["synthetic"] != true {
		t.Errorf("properties = %v, want synthetic: true", feature.Properties)
	}
	if feature.Properties["contour"] != float64(60) {
		t.Errorf("contour = %v, want 60 minutes", feature.Properties["contour"])
	}

	if len(feature.Geometry.Coordinates) != 1 {
		t.Fatalf("want exactly one ring, got %d", len(feature.Geometry.Coordinates))
	}
	ring := feature.Geometry.Coordinates[0]
	if len(ring) < 4 {
		t.Fatalf("ring has %d points, too few for a polygon", len(ring))
	}

	first, last := ring[0], ring[len(ring)-1]
	if first[0] != last[0] || first[1] != last[1] {
		t.Errorf("ring does not close: first %v, last %v", first, last)
	}

	wantKm := 5.0 / geo.DetourFactor
	for i, pt := range ring {
		gotKm := geo.HaversineKm(origin.Lat, origin.Lng, pt[1], pt[0])
		if math.Abs(gotKm-wantKm) > 0.5 {
			t.Errorf("vertex %d is %.2f km from origin, want ~%.2f km", i, gotKm, wantKm)
		}
	}
}

func TestSyntheticIsochroneGrowsWithBudget(t *testing.T) {
	client := valhalla.NewSyntheticClient()
	origin := valhalla.LatLng{Lat: 37.7, Lng: -122.4}

	radius := func(budgetSecs int) float64 {
		t.Helper()
		resp, err := client.Isochrone(context.Background(), valhalla.IsochroneRequest{
			Origin: origin, Costing: valhalla.CostingAuto, BudgetSecs: budgetSecs,
		})
		if err != nil {
			t.Fatalf("Isochrone: %v", err)
		}
		var feature struct {
			Geometry struct {
				Coordinates [][][]float64 `json:"coordinates"`
			} `json:"geometry"`
		}
		if err := json.Unmarshal(resp.Features[0], &feature); err != nil {
			t.Fatalf("feature is not valid JSON: %v", err)
		}
		pt := feature.Geometry.Coordinates[0][0]
		return geo.HaversineKm(origin.Lat, origin.Lng, pt[1], pt[0])
	}

	small, large := radius(600), radius(3600)
	if large <= small {
		t.Errorf("60-min radius %.2f km is not larger than 10-min radius %.2f km", large, small)
	}
}

func TestSyntheticMatrixTimesGrowWithDistance(t *testing.T) {
	client := valhalla.NewSyntheticClient()

	resp, err := client.Matrix(context.Background(), valhalla.MatrixRequest{
		Origins: []valhalla.LatLng{{Lat: 37.7, Lng: -122.4}},
		Destinations: []valhalla.LatLng{
			{Lat: 37.71, Lng: -122.39},
			{Lat: 38.20, Lng: -122.39},
		},
		Costing: valhalla.CostingAuto,
	})
	if err != nil {
		t.Fatalf("Matrix: %v", err)
	}

	if len(resp.SourcesToTargets) != 1 || len(resp.SourcesToTargets[0]) != 2 {
		t.Fatalf("shape = %v, want 1×2", resp.SourcesToTargets)
	}
	near, far := resp.SourcesToTargets[0][0], resp.SourcesToTargets[0][1]
	if !near.Routed() || !far.Routed() {
		t.Fatalf("cells = %+v, %+v, want both routed", near, far)
	}
	if *near.Time <= 0 {
		t.Errorf("near time = %d, want positive", *near.Time)
	}
	if *far.Time <= *near.Time {
		t.Errorf("far time %d is not greater than near time %d", *far.Time, *near.Time)
	}

	if bLine := geo.HaversineKm(37.7, -122.4, 38.20, -122.39); *far.DistanceKm < bLine {
		t.Errorf("far distance = %.2f km, want at least the %.2f km straight line", *far.DistanceKm, bLine)
	}
}

func TestSyntheticRejectsUnknownCosting(t *testing.T) {
	client := valhalla.NewSyntheticClient()

	_, err := client.Isochrone(context.Background(), valhalla.IsochroneRequest{
		Origin: valhalla.LatLng{Lat: 1, Lng: 2}, Costing: valhalla.Costing("teleport"), BudgetSecs: 600,
	})
	if !errors.Is(err, valhalla.ErrBadRequest) {
		t.Errorf("Isochrone error = %v, want ErrBadRequest", err)
	}

	_, err = client.Matrix(context.Background(), valhalla.MatrixRequest{
		Origins:      []valhalla.LatLng{{Lat: 1, Lng: 2}},
		Destinations: []valhalla.LatLng{{Lat: 3, Lng: 4}},
		Costing:      valhalla.Costing("teleport"),
	})
	if !errors.Is(err, valhalla.ErrBadRequest) {
		t.Errorf("Matrix error = %v, want ErrBadRequest", err)
	}

	_, err = client.Route(context.Background(), valhalla.RouteRequest{
		Origin: valhalla.LatLng{Lat: 1, Lng: 2}, Destination: valhalla.LatLng{Lat: 3, Lng: 4},
		Costing: valhalla.Costing("teleport"),
	})
	if !errors.Is(err, valhalla.ErrBadRequest) {
		t.Errorf("Route error = %v, want ErrBadRequest", err)
	}
}

func TestSyntheticRouteIsTheStraightSegment(t *testing.T) {
	client := valhalla.NewSyntheticClient()

	resp, err := client.Route(context.Background(), valhalla.RouteRequest{
		Origin:      valhalla.LatLng{Lat: 37.7, Lng: -122.4},
		Destination: valhalla.LatLng{Lat: 37.71, Lng: -122.39},
		Costing:     valhalla.CostingPedestrian,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	want := []valhalla.LatLng{{Lat: 37.7, Lng: -122.4}, {Lat: 37.71, Lng: -122.39}}
	if len(resp.Shape) != len(want) {
		t.Fatalf("Shape = %v, want the two endpoints", resp.Shape)
	}
	for i, w := range want {
		if resp.Shape[i] != w {
			t.Errorf("Shape[%d] = %v, want %v", i, resp.Shape[i], w)
		}
	}
	if resp.Time <= 0 {
		t.Errorf("Time = %d, want a positive duration so the chainer can seed access seconds", resp.Time)
	}
	bLine := geo.HaversineKm(37.7, -122.4, 37.71, -122.39)
	if resp.DistanceKm < bLine {
		t.Errorf("DistanceKm = %.2f, want at least the %.2f km straight line", resp.DistanceKm, bLine)
	}
}

func TestSyntheticAcceptsMultimodalCosting(t *testing.T) {
	client := valhalla.NewSyntheticClient()
	origin := valhalla.LatLng{Lat: 37.7, Lng: -122.4}

	if _, err := client.Isochrone(context.Background(), valhalla.IsochroneRequest{
		Origin: origin, Costing: valhalla.CostingMultimodal, BudgetSecs: 1800,
	}); err != nil {
		t.Fatalf("Isochrone: %v", err)
	}
	route, err := client.Route(context.Background(), valhalla.RouteRequest{
		Origin: origin, Destination: valhalla.LatLng{Lat: 37.71, Lng: -122.39},
		Costing: valhalla.CostingMultimodal,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if route.Time <= 0 {
		t.Errorf("Time = %d, want a positive multimodal access time", route.Time)
	}
}

func TestSyntheticReportsNoStatus(t *testing.T) {
	if _, err := valhalla.NewSyntheticClient().Status(context.Background()); !errors.Is(err, valhalla.ErrNoStatus) {
		t.Errorf("Status error = %v, want ErrNoStatus", err)
	}
}
