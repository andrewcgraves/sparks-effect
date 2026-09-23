package valhalla_test

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

var (
	wilshireWestern = valhalla.LatLng{Lat: 34.0618, Lng: -118.3090}
	unionStation    = valhalla.LatLng{Lat: 34.0562, Lng: -118.2365}
)

func acceptanceClient(t *testing.T) *valhalla.HTTPClient {
	t.Helper()
	base := os.Getenv("TEST_VALHALLA_URL")
	if base == "" {
		t.Skip("set TEST_VALHALLA_URL to a Valhalla server with transit tiles to run the acceptance checks")
	}

	return valhalla.NewHTTPClient(base).WithTimeout(5 * time.Minute)
}

func acceptanceDeparture(t *testing.T) time.Time {
	t.Helper()
	raw := os.Getenv("TEST_TRANSIT_DEPART_AT")
	if raw == "" {
		return valhalla.WeekdayDepartAt(time.Now(), valhalla.ServiceAreaZone())
	}
	at, err := time.ParseInLocation("2006-01-02T15:04", raw, valhalla.ServiceAreaZone())
	if err != nil {
		t.Fatalf("TEST_TRANSIT_DEPART_AT=%q is not YYYY-MM-DDTHH:MM: %v", raw, err)
	}
	return at
}

func TestAcceptanceTransitRouteBoardsAService(t *testing.T) {
	client := acceptanceClient(t)
	at := acceptanceDeparture(t)

	route, err := client.Route(context.Background(), valhalla.RouteRequest{
		Origin:      wilshireWestern,
		Destination: unionStation,
		Costing:     valhalla.CostingMultimodal,
		DepartAt:    at,
	})
	if err != nil {
		t.Fatalf("Route(multimodal) departing %s: %v", at.Format(time.RFC3339), err)
	}

	if !route.RodeTransit {
		t.Fatalf("multimodal route from Wilshire/Western to Union Station departing %s walked the whole way "+
			"(%d s, %.1f km, no transit maneuver).\n"+
			"Valhalla answers 200 for a multimodal request it can find no service for, so this is what an "+
			"expired GTFS calendar in the tiles looks like. Check that the feeds on the volume cover this "+
			"date — mdb-30's calendar.txt in particular — and that the tiles were rebuilt after the last "+
			"feed refresh (SPA-274).",
			at.Format(time.RFC3339), route.Time, route.DistanceKm)
	}
}

func TestAcceptanceTransitBeatsWalkingOnTheSamePair(t *testing.T) {
	client := acceptanceClient(t)
	at := acceptanceDeparture(t)
	ctx := context.Background()

	transit, err := client.Route(ctx, valhalla.RouteRequest{
		Origin: wilshireWestern, Destination: unionStation,
		Costing: valhalla.CostingMultimodal, DepartAt: at,
	})
	if err != nil {
		t.Fatalf("Route(multimodal): %v", err)
	}
	walk, err := client.Route(ctx, valhalla.RouteRequest{
		Origin: wilshireWestern, Destination: unionStation,
		Costing: valhalla.CostingPedestrian,
	})
	if err != nil {
		t.Fatalf("Route(pedestrian): %v", err)
	}

	if transit.Time >= walk.Time {
		t.Errorf("multimodal took %d s against pedestrian's %d s on the same pair departing %s; "+
			"riding a rail line seven kilometres into downtown must beat walking it",
			transit.Time, walk.Time, at.Format(time.RFC3339))
	}
}

func TestAcceptanceReportsTheTilesetUnderTest(t *testing.T) {
	client := acceptanceClient(t)

	status, err := client.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	t.Logf("valhalla %s, tiles built %s", status.Version, status.TilesetAt.Format(time.RFC3339))
	if status.TilesetAt.IsZero() {
		t.Error("the server reported no tileset timestamp; the cache records it beside every polygon")
	}
}

var (
	sunsetPin               = valhalla.LatLng{Lat: 37.773347286345896, Lng: -122.50371950034662}
	salesforceTransitCenter = valhalla.LatLng{Lat: 37.7897, Lng: -122.3967}
	lasVegasStrip           = valhalla.LatLng{Lat: 36.1147, Lng: -115.1728}
	contour180Mins          = 180 * 60
	contour240Mins          = 240 * 60
	minSunsetIsochroneKm2   = 600.0
)

func TestAcceptanceSunsetRouteBoardsAService(t *testing.T) {
	client := acceptanceClient(t)
	at := acceptanceDeparture(t)

	route, err := client.Route(context.Background(), valhalla.RouteRequest{
		Origin:      sunsetPin,
		Destination: salesforceTransitCenter,
		Costing:     valhalla.CostingMultimodal,
		DepartAt:    at,
	})
	if err != nil {
		t.Fatalf("Route(Sunset to Salesforce Transit Center) departing %s: %v\n"+
			"error 442 is what the 2415 m default walk cap returns from this pin: the nearest "+
			"boardable stop is further than that, and pedestrian.multimodal_start_end_max_distance "+
			"is dropped silently so the 10 km override never applied (SPA-297).",
			at.Format(time.RFC3339), err)
	}
	if !route.RodeTransit {
		t.Fatalf("Sunset to Salesforce Transit Center departing %s walked the whole way "+
			"(%d s, %.1f km, no transit maneuver). The walk-cap override is not reaching Valhalla.",
			at.Format(time.RFC3339), route.Time, route.DistanceKm)
	}
}

var (
	sanJoseState       = valhalla.LatLng{Lat: 37.3352, Lng: -121.8811}
	santaTeresaStation = valhalla.LatLng{Lat: 37.2357, Lng: -121.7890}
)

func TestAcceptanceSanJoseRouteBoardsVTA(t *testing.T) {
	// Every other boarding check here passes on free Mobility Database rows
	// alone: LA Metro, Muni. VTA reaches the served graph through the 511
	// ingest, so this is the one that fails when the token-gated Bay Area
	// operators are missing (SPA-339). SJSU -> Diridon is the pair users
	// report, but at the default walking speed it walks by three minutes on a
	// healthy tileset (SPA-335), so it cannot tell broken from fine; Santa
	// Teresa is 14 km down the Blue Line and rides whenever VTA is there.
	client := acceptanceClient(t)
	at := acceptanceDeparture(t)

	route, err := client.Route(context.Background(), valhalla.RouteRequest{
		Origin:      sanJoseState,
		Destination: santaTeresaStation,
		Costing:     valhalla.CostingMultimodal,
		DepartAt:    at,
	})
	if err != nil {
		t.Fatalf("Route(San Jose State to Santa Teresa) departing %s: %v", at.Format(time.RFC3339), err)
	}
	t.Logf("San Jose State to Santa Teresa: %d s, operators %q", route.Time, route.Operators)
	if !route.RodeTransit {
		t.Fatalf("San Jose State to Santa Teresa departing %s walked the whole way (%d s, %.1f km). "+
			"This pair runs on VTA light rail, which only the 511 ingest supplies: check that "+
			"/gtfs_work/manifest.csv has a usable 511- row and that the served generation was built after it.",
			at.Format(time.RFC3339), route.Time, route.DistanceKm)
	}
	for _, op := range route.Operators {
		name := strings.ToLower(op)
		if name == "vta" || strings.Contains(name, "valley transportation authority") {
			return
		}
	}
	t.Errorf("San Jose State to Santa Teresa departing %s rode %q but not VTA; "+
		"the 511 operator SC is missing from the served graph", at.Format(time.RFC3339), route.Operators)
}

func TestAcceptanceTransitIsochroneIsAtLeastAsLargeAsWalk(t *testing.T) {
	client := acceptanceClient(t)
	at := acceptanceDeparture(t)
	ctx := context.Background()

	origins := []struct {
		name string
		pin  valhalla.LatLng
	}{
		{"SF Sunset (reported pin)", sunsetPin},
		{"LA Koreatown (Wilshire/Western)", wilshireWestern},
		{"Las Vegas Strip", lasVegasStrip},
		{"San Jose State", sanJoseState},
	}
	for _, origin := range origins {
		t.Run(origin.name, func(t *testing.T) {
			transit, err := client.Isochrone(ctx, valhalla.IsochroneRequest{
				Origin:     origin.pin,
				Costing:    valhalla.CostingMultimodal,
				BudgetSecs: contour180Mins,
				DepartAt:   at,
			})
			if err != nil {
				t.Fatalf("Isochrone(multimodal): %v", err)
			}
			walk, err := client.Isochrone(ctx, valhalla.IsochroneRequest{
				Origin:     origin.pin,
				Costing:    valhalla.CostingPedestrian,
				BudgetSecs: contour180Mins,
			})
			if err != nil {
				t.Fatalf("Isochrone(pedestrian): %v", err)
			}
			transitKm2, err := isochroneAreaKm2(transit)
			if err != nil {
				t.Fatalf("transit area: %v", err)
			}
			walkKm2, err := isochroneAreaKm2(walk)
			if err != nil {
				t.Fatalf("walk area: %v", err)
			}
			t.Logf("%s 180 min: transit %.1f km², walk %.1f km²", origin.name, transitKm2, walkKm2)
			if transitKm2 < walkKm2 {
				t.Errorf("multimodal isochrone is %.1f km² against pedestrian's %.1f km² at 180 min; "+
					"a transit surface smaller than walking from the same pin is the SPA-297 inversion "+
					"(wrong costing-option key, missing feeds, expired calendars)",
					transitKm2, walkKm2)
			}
		})
	}
}

func TestAcceptanceSunsetIsochroneGrowsWithBudget(t *testing.T) {
	client := acceptanceClient(t)
	at := acceptanceDeparture(t)
	ctx := context.Background()

	at180, err := client.Isochrone(ctx, valhalla.IsochroneRequest{
		Origin:     sunsetPin,
		Costing:    valhalla.CostingMultimodal,
		BudgetSecs: contour180Mins,
		DepartAt:   at,
	})
	if err != nil {
		t.Fatalf("Isochrone(180): %v", err)
	}
	at240, err := client.Isochrone(ctx, valhalla.IsochroneRequest{
		Origin:     sunsetPin,
		Costing:    valhalla.CostingMultimodal,
		BudgetSecs: contour240Mins,
		DepartAt:   at,
	})
	if err != nil {
		t.Fatalf("Isochrone(240): %v", err)
	}
	km180, err := isochroneAreaKm2(at180)
	if err != nil {
		t.Fatalf("180 min area: %v", err)
	}
	km240, err := isochroneAreaKm2(at240)
	if err != nil {
		t.Fatalf("240 min area: %v", err)
	}
	t.Logf("Sunset multimodal: 180 min %.1f km², 240 min %.1f km²", km180, km240)
	if km180 < minSunsetIsochroneKm2 {
		t.Errorf("180 min multimodal isochrone is %.1f km², want > %.0f; "+
			"~15 km² is the 2415 m walk-cap blob (SPA-297)", km180, minSunsetIsochroneKm2)
	}
	if km240 <= km180 {
		t.Errorf("240 min multimodal isochrone is %.1f km², not larger than 180 min's %.1f km²; "+
			"a surface that has stopped expanding is still clipped on distance, not time", km240, km180)
	}
}

const earthRadiusKm = 6371.0

func isochroneAreaKm2(iso *valhalla.IsochroneResponse) (float64, error) {
	var total float64
	for _, raw := range iso.Features {
		var f struct {
			Geometry struct {
				Type        string          `json:"type"`
				Coordinates json.RawMessage `json:"coordinates"`
			} `json:"geometry"`
		}
		if err := json.Unmarshal(raw, &f); err != nil {
			return 0, err
		}
		switch f.Geometry.Type {
		case "Polygon":
			var coords [][][]float64
			if err := json.Unmarshal(f.Geometry.Coordinates, &coords); err != nil {
				return 0, err
			}
			total += polygonAreaKm2(coords)
		case "MultiPolygon":
			var coords [][][][]float64
			if err := json.Unmarshal(f.Geometry.Coordinates, &coords); err != nil {
				return 0, err
			}
			for _, poly := range coords {
				total += polygonAreaKm2(poly)
			}
		}
	}
	return total, nil
}

func polygonAreaKm2(rings [][][]float64) float64 {
	if len(rings) == 0 {
		return 0
	}
	area := math.Abs(ringAreaKm2(rings[0]))
	for _, hole := range rings[1:] {
		area -= math.Abs(ringAreaKm2(hole))
	}
	if area < 0 {
		return 0
	}
	return area
}

func ringAreaKm2(ring [][]float64) float64 {
	if len(ring) < 4 {
		return 0
	}
	var area float64
	n := len(ring) - 1
	for i := 0; i < n; i++ {
		lon1 := ring[i][0] * math.Pi / 180
		lon2 := ring[i+1][0] * math.Pi / 180
		lat1 := ring[i][1] * math.Pi / 180
		lat2 := ring[i+1][1] * math.Pi / 180
		area += (lon2 - lon1) * (2 + math.Sin(lat1) + math.Sin(lat2))
	}
	return area * earthRadiusKm * earthRadiusKm / 2
}

func TestIsochroneAreaOfAKnownSquare(t *testing.T) {
	feature, err := json.Marshal(map[string]any{
		"type": "Feature",
		"geometry": map[string]any{
			"type": "Polygon",
			"coordinates": [][][]float64{{
				{0, 0}, {0.01, 0}, {0.01, 0.01}, {0, 0.01}, {0, 0},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := isochroneAreaKm2(&valhalla.IsochroneResponse{Features: []json.RawMessage{feature}})
	if err != nil {
		t.Fatal(err)
	}

	if got < 1.2 || got > 1.3 {
		t.Errorf("area = %.4f km², want ~1.236", got)
	}
}
