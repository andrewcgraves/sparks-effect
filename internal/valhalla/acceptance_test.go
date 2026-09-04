package valhalla_test

import (
	"context"
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
	// Generous next to DefaultTimeout: a multimodal request against a cold
	// server with transit tiles is the slowest thing this client ever asks for,
	// and a timeout here would read as a transit failure.
	return valhalla.NewHTTPClient(base).WithTimeout(2 * time.Minute)
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
