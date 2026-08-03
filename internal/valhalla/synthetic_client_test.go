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

// The synthetic client stands in for a real server in production when none is
// configured, so what it emits has to be GeoJSON a map can actually draw: a
// closed polygon ring, in [lng, lat] order, sized by the budget.
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
	// A shape that reaches a map without a real server behind it must say so.
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
	// GeoJSON requires the ring to close on its first point exactly.
	first, last := ring[0], ring[len(ring)-1]
	if first[0] != last[0] || first[1] != last[1] {
		t.Errorf("ring does not close: first %v, last %v", first, last)
	}

	// An hour's walk at 5 km/h over a 1.4 detour factor is ~3.6 km of reach.
	// Every vertex should sit about that far from the origin — the check is
	// loose because the ring is drawn in degrees, not on a sphere.
	wantKm := 5.0 / geo.DetourFactor
	for i, pt := range ring {
		gotKm := geo.HaversineKm(origin.Lat, origin.Lng, pt[1], pt[0])
		if math.Abs(gotKm-wantKm) > 0.5 {
			t.Errorf("vertex %d is %.2f km from origin, want ~%.2f km", i, gotKm, wantKm)
		}
	}
}

// A larger budget must produce a larger polygon, or the synthetic loop would
// pass while telling you nothing about whether the budget was plumbed through.
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
			{Lat: 37.71, Lng: -122.39}, // near
			{Lat: 38.20, Lng: -122.39}, // far
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
	if near.Time <= 0 {
		t.Errorf("near time = %d, want positive", near.Time)
	}
	if far.Time <= near.Time {
		t.Errorf("far time %d is not greater than near time %d", far.Time, near.Time)
	}
}

// An unknown costing is a bad request here for the same reason it is one on the
// real server: it is a permanent failure, and classifying it as anything else
// would have the worker retry a job that can never succeed.
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
}

// There is no tileset behind the synthetic client, and inventing a timestamp
// for one would put a fiction in the cache's diagnostic column that later reads
// as fact.
func TestSyntheticReportsNoStatus(t *testing.T) {
	if _, err := valhalla.NewSyntheticClient().Status(context.Background()); !errors.Is(err, valhalla.ErrNoStatus) {
		t.Errorf("Status error = %v, want ErrNoStatus", err)
	}
}
