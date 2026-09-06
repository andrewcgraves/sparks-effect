package valhalla_test

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

// The transit acceptance checks: does the server this points at actually route
// anyone onto a train?
//
// Everything else in this package asserts what the client puts on the wire,
// against an httptest server that answers whatever the test hands it. That is
// the right shape for a request contract and it is worth nothing here, because
// the failure these exist to catch is upstream of the client entirely: tiles
// built from GTFS feeds whose calendars have expired. Valhalla does not error
// on those. It walks the whole way and answers 200 with a plausible shape, a
// plausible duration, and no service on it — so a test asserting a 200, or a
// polygon, or a duration, passes in exactly the broken state (SPA-274).
//
// They need a real Valhalla with real transit tiles, so they skip unless
// TEST_VALHALLA_URL is set:
//
//	make acceptance TEST_VALHALLA_URL=http://localhost:8002
//
// Against the cluster's server, that is a port-forward to the valhalla service.

// The known pair: Wilshire/Western, the western terminus of LA Metro's D Line,
// to Los Angeles Union Station, which is on it.
//
// It is chosen for being unambiguous rather than for being far. Seven
// kilometres is walkable — an hour and a half of it — so a server with no
// working transit still answers, which is what makes this a test rather than a
// tautology: the pair is one where the correct answer rides and the broken
// answer walks, and the two are told apart by the maneuvers, not the duration.
//
// mdb-30 (LA Metro Rail) is the feed that has to be in the tiles for this to
// pass. If it is dropped from GTFS_SUBDIVISIONS or denylisted, this test is
// asserting something the deployment no longer promises and should move to
// whatever pair replaces it.
var (
	wilshireWestern = valhalla.LatLng{Lat: 34.0618, Lng: -118.3090}
	unionStation    = valhalla.LatLng{Lat: 34.0562, Lng: -118.2365}
)

// acceptanceClient builds a client against TEST_VALHALLA_URL, or skips.
func acceptanceClient(t *testing.T) *valhalla.HTTPClient {
	t.Helper()
	base := os.Getenv("TEST_VALHALLA_URL")
	if base == "" {
		t.Skip("set TEST_VALHALLA_URL to a Valhalla server with transit tiles to run the acceptance checks")
	}
	// Generous next to DefaultTimeout: a 180–240 min multimodal isochrone
	// against a cold server with transit tiles is the slowest thing this
	// client ever asks for, and a timeout here would read as a transit failure.
	return valhalla.NewHTTPClient(base).WithTimeout(5 * time.Minute)
}

// acceptanceDeparture is when the acceptance checks ask to travel.
//
// It defaults to the same representative weekday morning a real chain uses, so
// what is asserted is what production does. TEST_TRANSIT_DEPART_AT overrides it
// with a YYYY-MM-DDTHH:MM local time, which is how a specific service date gets
// checked — the corrected LA Metro Rail feed covers a fortnight at a time, and
// "does the tileset still have live service the week after next" is a question
// worth being able to ask directly.
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

// A multimodal route over the known pair has to board something.
//
// This is the assertion the whole file exists for. A 200 with a polygon is
// exactly what the broken state returns, so anything weaker proves nothing:
// only a maneuver whose travel_mode is "transit" distinguishes a tileset with
// live GTFS service from one whose feeds all expired months ago.
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

// Riding beats walking on the same pair, which is the property a transit
// isochrone is actually built on.
//
// The maneuver check above proves a service was boarded; this proves boarding
// it was worth doing. A tileset carrying one stale feed can satisfy the first
// and still fail here, and a transit surface that does not reach further than
// the walking one is the user-visible complaint (SPA-272) whatever the
// maneuvers say.
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

// The tiles the server is serving, reported rather than asserted.
//
// Nothing here can say what tileset is correct, but "which tiles was this run
// against?" is the first question asked of a failure above, and it is answered
// by the same server in one call.
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

// The reported pin: outer Sunset, where the nearest surviving stop sits
// beyond Valhalla's 2415 m default walk cap. With the wrong costing-option
// key the origin never boards anything; with transit_start_end_max_distance
// at 10 km it does (SPA-297). Distinct from missing Muni feeds (SPA-288).
var (
	sunsetPin               = valhalla.LatLng{Lat: 37.773347286345896, Lng: -122.50371950034662}
	salesforceTransitCenter = valhalla.LatLng{Lat: 37.7897, Lng: -122.3967}
	lasVegasStrip           = valhalla.LatLng{Lat: 36.1147, Lng: -115.1728}
	contour180Mins          = 180 * 60
	contour240Mins          = 240 * 60
	minSunsetIsochroneKm2   = 600.0
)

// The user's reported trip has to board something. Error 442 / a walking
// itinerary is what the ignored walk-cap key produced; a transit maneuver
// is what the recognised key produces, even with Muni still missing.
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

// A multimodal isochrone must never come back smaller than the walk isochrone
// from the same origin and contour. That inversion is the user-visible
// absurdity of SPA-297 (Sunset transit 15 km² vs walk 163 km² at 180 min),
// and it is independent of which costing-option key caused it.
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

// From the reported pin, 180 minutes of multimodal reach has to be a real
// transit surface — hundreds of square kilometres, and still growing at 240
// minutes. The ignored walk-cap key produced a ~15 km² blob that stopped
// expanding at 180 because the origin never boarded.
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

// isochroneAreaKm2 is the spherical area of every polygon in the collection,
// outer rings minus holes. Used to compare transit and walk surfaces; it does
// not need to be a geodesic authority, only monotonic and in the right units.
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

// ringAreaKm2 is the spherical excess of a closed lon/lat ring, the same
// trapezoidal formula GeoJSON area libraries use on WGS84.
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

// The area helper has to report square kilometres in the right ballpark, or
// the acceptance comparisons above are comparing noise. A 0.01° square on
// the equator is about 1.23 km on a side.
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
	// 0.01° ≈ 1.11195 km at the equator; square ≈ 1.236 km².
	if got < 1.2 || got > 1.3 {
		t.Errorf("area = %.4f km², want ~1.236", got)
	}
}
