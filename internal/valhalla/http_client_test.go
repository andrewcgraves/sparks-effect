package valhalla_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

type captureServer struct {
	*httptest.Server
	path string
	body map[string]any
}

func newCaptureServer(t *testing.T, status int, response string) *captureServer {
	t.Helper()
	cs := &captureServer{}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.path = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &cs.body)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(response))
	}))
	t.Cleanup(cs.Close)
	return cs
}

const okIsochrone = `{"type":"FeatureCollection","features":[{"type":"Feature","properties":{},"geometry":{"type":"Polygon","coordinates":[]}}]}`

func TestIsochrone_requestShape(t *testing.T) {
	srv := newCaptureServer(t, http.StatusOK, okIsochrone)
	client := valhalla.NewHTTPClient(srv.URL)

	resp, err := client.Isochrone(context.Background(), valhalla.IsochroneRequest{
		Origin:     valhalla.LatLng{Lat: 37.7, Lng: -122.4},
		Costing:    valhalla.CostingPedestrian,
		BudgetSecs: 2700,
	})
	if err != nil {
		t.Fatalf("Isochrone: %v", err)
	}
	if len(resp.Features) != 1 {
		t.Errorf("Features: want 1, got %d", len(resp.Features))
	}

	if srv.path != "/isochrone" {
		t.Errorf("path = %q, want /isochrone", srv.path)
	}
	if got := srv.body["costing"]; got != "pedestrian" {
		t.Errorf("costing = %v, want pedestrian", got)
	}
	if _, ok := srv.body["date_time"]; ok {
		t.Error("date_time is a multimodal option; pedestrian must not send it")
	}

	locations, _ := srv.body["locations"].([]any)
	if len(locations) != 1 {
		t.Fatalf("locations: want 1, got %v", srv.body["locations"])
	}
	loc, _ := locations[0].(map[string]any)
	if loc["lat"] != 37.7 {
		t.Errorf("lat = %v, want 37.7", loc["lat"])
	}
	if loc["lon"] != -122.4 {
		t.Errorf("lon = %v, want -122.4", loc["lon"])
	}

	contours, _ := srv.body["contours"].([]any)
	if len(contours) != 1 {
		t.Fatalf("contours: want 1, got %v", srv.body["contours"])
	}
	contour, _ := contours[0].(map[string]any)
	if contour["time"] != float64(45) {
		t.Errorf("contour time = %v, want 45 minutes", contour["time"])
	}

	if srv.body["polygons"] != true {
		t.Errorf("polygons = %v, want true — without it Valhalla returns linestrings", srv.body["polygons"])
	}
	assertIsochroneDenoiseZero(t, srv.body)
}

func TestIsochrone_sendsDenoiseZero(t *testing.T) {
	tests := []struct {
		name    string
		costing valhalla.Costing
		depart  func(*testing.T) time.Time
	}{
		{"pedestrian", valhalla.CostingPedestrian, nil},
		{"bicycle", valhalla.CostingBicycle, nil},
		{"auto", valhalla.CostingAuto, nil},
		{"multimodal", valhalla.CostingMultimodal, morningDeparture},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCaptureServer(t, http.StatusOK, okIsochrone)
			req := valhalla.IsochroneRequest{
				Origin:     valhalla.LatLng{Lat: 37.7805, Lng: -122.4679},
				Costing:    tc.costing,
				BudgetSecs: 7200,
			}
			if tc.depart != nil {
				req.DepartAt = tc.depart(t)
			}
			if _, err := valhalla.NewHTTPClient(srv.URL).Isochrone(context.Background(), req); err != nil {
				t.Fatalf("Isochrone: %v", err)
			}
			assertIsochroneDenoiseZero(t, srv.body)
		})
	}
}

func assertIsochroneDenoiseZero(t *testing.T, body map[string]any) {
	t.Helper()
	got, ok := body["denoise"]
	if !ok {
		t.Fatal("denoise missing from /isochrone body — Valhalla defaults to 1.0 and drops every contour but the largest (SPA-302)")
	}
	if got != float64(valhalla.IsochroneDenoise) {
		t.Errorf("denoise = %v, want %v", got, valhalla.IsochroneDenoise)
	}
}

func TestIsochrone_locationCarriesWidenedRadiusAndReachability(t *testing.T) {
	srv := newCaptureServer(t, http.StatusOK, okIsochrone)
	client := valhalla.NewHTTPClient(srv.URL)

	if _, err := client.Isochrone(context.Background(), valhalla.IsochroneRequest{
		Origin:     valhalla.LatLng{Lat: 37.7, Lng: -122.4},
		Costing:    valhalla.CostingPedestrian,
		BudgetSecs: 1800,
	}); err != nil {
		t.Fatalf("Isochrone: %v", err)
	}

	locations, _ := srv.body["locations"].([]any)
	loc, _ := locations[0].(map[string]any)
	if loc["radius"] != float64(valhalla.LocationRadiusM) {
		t.Errorf("radius = %v, want %d", loc["radius"], valhalla.LocationRadiusM)
	}
	if loc["minimum_reachability"] != float64(valhalla.LocationMinReachability) {
		t.Errorf("minimum_reachability = %v, want %d", loc["minimum_reachability"], valhalla.LocationMinReachability)
	}
}

func TestMatrix_requestShape(t *testing.T) {
	srv := newCaptureServer(t, http.StatusOK, `{"sources_to_targets":[[{"time":600,"distance":1.0},{"time":null,"distance":null}]]}`)
	client := valhalla.NewHTTPClient(srv.URL)

	resp, err := client.Matrix(context.Background(), valhalla.MatrixRequest{
		Origins:      []valhalla.LatLng{{Lat: 37.7, Lng: -122.4}},
		Destinations: []valhalla.LatLng{{Lat: 37.71, Lng: -122.39}, {Lat: 37.69, Lng: -122.41}},
		Costing:      valhalla.CostingAuto,
	})
	if err != nil {
		t.Fatalf("Matrix: %v", err)
	}

	if srv.path != "/sources_to_targets" {
		t.Errorf("path = %q, want /sources_to_targets", srv.path)
	}
	if got := srv.body["costing"]; got != "auto" {
		t.Errorf("costing = %v, want auto", got)
	}

	if got := srv.body["units"]; got != "kilometers" {
		t.Errorf("units = %v, want kilometers", got)
	}
	sources, _ := srv.body["sources"].([]any)
	if len(sources) != 1 {
		t.Errorf("sources: want 1, got %v", srv.body["sources"])
	}
	targets, _ := srv.body["targets"].([]any)
	if len(targets) != 2 {
		t.Errorf("targets: want 2, got %v", srv.body["targets"])
	}

	if src, _ := sources[0].(map[string]any); src["radius"] != float64(valhalla.LocationRadiusM) {
		t.Errorf("sources[0] radius = %v, want %d", src["radius"], valhalla.LocationRadiusM)
	}
	if tgt, _ := targets[0].(map[string]any); tgt["minimum_reachability"] != float64(valhalla.LocationMinReachability) {
		t.Errorf("targets[0] minimum_reachability = %v, want %d", tgt["minimum_reachability"], valhalla.LocationMinReachability)
	}

	if len(resp.SourcesToTargets) != 1 || len(resp.SourcesToTargets[0]) != 2 {
		t.Fatalf("SourcesToTargets shape = %v", resp.SourcesToTargets)
	}
	routed := resp.SourcesToTargets[0][0]
	if !routed.Routed() || *routed.Time != 600 || *routed.DistanceKm != 1.0 {
		t.Errorf("cell 0 = %+v, want a routed 600 s / 1.0 km", routed)
	}

	if declined := resp.SourcesToTargets[0][1]; declined.Routed() {
		t.Errorf("cell 1 = %+v, want an unrouted pair", declined)
	}
}

func TestNoAuthorizationHeaderIsSent(t *testing.T) {
	var sawAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(okIsochrone))
	}))
	defer srv.Close()

	_, err := valhalla.NewHTTPClient(srv.URL).Isochrone(context.Background(), valhalla.IsochroneRequest{
		Origin: valhalla.LatLng{Lat: 1, Lng: 2}, Costing: valhalla.CostingAuto, BudgetSecs: 600,
	})
	if err != nil {
		t.Fatalf("Isochrone: %v", err)
	}
	if sawAuth != "" {
		t.Errorf("Authorization header = %q, want none", sawAuth)
	}
}

func TestErrorsAreClassifiedByCause(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		{"bad request", http.StatusBadRequest, `{"error_code":154,"error":"Path distance exceeds the max distance limit"}`, valhalla.ErrBadRequest},
		{"bad request, empty body", http.StatusBadRequest, ``, valhalla.ErrBadRequest},
		{"too many requests", http.StatusTooManyRequests, `{"error":"slow down"}`, valhalla.ErrRateLimit},
		{"server error", http.StatusInternalServerError, `{"error":"boom"}`, valhalla.ErrUpstream},
		{"unavailable", http.StatusServiceUnavailable, ``, valhalla.ErrUpstream},
		{"not found", http.StatusNotFound, ``, valhalla.ErrUpstream},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCaptureServer(t, tc.status, tc.body)
			client := valhalla.NewHTTPClient(srv.URL)

			_, err := client.Isochrone(context.Background(), valhalla.IsochroneRequest{
				Origin: valhalla.LatLng{Lat: 1, Lng: 2}, Costing: valhalla.CostingAuto, BudgetSecs: 600,
			})
			if !errors.Is(err, tc.want) {
				t.Errorf("Isochrone error = %v, want %v", err, tc.want)
			}

			_, err = client.Matrix(context.Background(), valhalla.MatrixRequest{
				Origins:      []valhalla.LatLng{{Lat: 1, Lng: 2}},
				Destinations: []valhalla.LatLng{{Lat: 3, Lng: 4}},
				Costing:      valhalla.CostingAuto,
			})
			if !errors.Is(err, tc.want) {
				t.Errorf("Matrix error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestBadRequestCarriesValhallasErrorCode(t *testing.T) {
	srv := newCaptureServer(t, http.StatusBadRequest,
		`{"error_code":150,"error":"Exceeded max matrix location pairs"}`)

	_, err := valhalla.NewHTTPClient(srv.URL).Matrix(context.Background(), valhalla.MatrixRequest{
		Origins:      []valhalla.LatLng{{Lat: 1, Lng: 2}},
		Destinations: []valhalla.LatLng{{Lat: 3, Lng: 4}},
		Costing:      valhalla.CostingAuto,
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"150", "Exceeded max matrix location pairs"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestUnreachableServerIsUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	_, err := valhalla.NewHTTPClient(url).Isochrone(context.Background(), valhalla.IsochroneRequest{
		Origin: valhalla.LatLng{Lat: 1, Lng: 2}, Costing: valhalla.CostingAuto, BudgetSecs: 600,
	})
	if !errors.Is(err, valhalla.ErrUpstream) {
		t.Errorf("error = %v, want ErrUpstream", err)
	}
}

func TestStatusReportsTilesetTimestamp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"3.5.1","tileset_last_modified":1750000000}`))
	}))
	defer srv.Close()

	got, err := valhalla.NewHTTPClient(srv.URL).Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if got.Version != "3.5.1" {
		t.Errorf("Version = %q, want 3.5.1", got.Version)
	}
	if want := time.Unix(1750000000, 0).UTC(); !got.TilesetAt.Equal(want) {
		t.Errorf("TilesetAt = %v, want %v", got.TilesetAt, want)
	}
}

func TestStatusWithoutTilesetLeavesTheTimeZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"version":"3.5.1"}`))
	}))
	defer srv.Close()

	got, err := valhalla.NewHTTPClient(srv.URL).Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if !got.TilesetAt.IsZero() {
		t.Errorf("TilesetAt = %v, want the zero time", got.TilesetAt)
	}
}

const shapeThreePoints = `_y_|fA~numhFg^n}@g^g^`

func okRoute(shape string) string {
	return `{"trip":{"legs":[{"shape":"` + shape + `"}],"summary":{"time":420,"length":0.5}}}`
}

func TestRoute_requestShape(t *testing.T) {
	srv := newCaptureServer(t, http.StatusOK, okRoute(shapeThreePoints))
	client := valhalla.NewHTTPClient(srv.URL)

	resp, err := client.Route(context.Background(), valhalla.RouteRequest{
		Origin:      valhalla.LatLng{Lat: 37.7, Lng: -122.4},
		Destination: valhalla.LatLng{Lat: 37.701, Lng: -122.4005},
		Costing:     valhalla.CostingPedestrian,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}

	if srv.path != "/route" {
		t.Errorf("path = %q, want /route", srv.path)
	}
	if got := srv.body["costing"]; got != "pedestrian" {
		t.Errorf("costing = %v, want pedestrian", got)
	}
	if got := srv.body["units"]; got != "kilometers" {
		t.Errorf("units = %v, want kilometers", got)
	}
	if _, ok := srv.body["date_time"]; ok {
		t.Error("date_time is a multimodal option; pedestrian must not send it")
	}
	locations, _ := srv.body["locations"].([]any)
	if len(locations) != 2 {
		t.Fatalf("locations: want 2, got %v", srv.body["locations"])
	}

	first, _ := locations[0].(map[string]any)
	if first["lat"] != 37.7 || first["lon"] != -122.4 {
		t.Errorf("locations[0] = %v, want lat 37.7 lon -122.4", first)
	}
	last, _ := locations[1].(map[string]any)
	if last["lat"] != 37.701 || last["lon"] != -122.4005 {
		t.Errorf("locations[1] = %v, want lat 37.701 lon -122.4005", last)
	}

	if first["radius"] != float64(valhalla.LocationRadiusM) {
		t.Errorf("locations[0] radius = %v, want %d", first["radius"], valhalla.LocationRadiusM)
	}
	if last["minimum_reachability"] != float64(valhalla.LocationMinReachability) {
		t.Errorf("locations[1] minimum_reachability = %v, want %d", last["minimum_reachability"], valhalla.LocationMinReachability)
	}

	want := []valhalla.LatLng{
		{Lat: 37.7, Lng: -122.4},
		{Lat: 37.7005, Lng: -122.401},
		{Lat: 37.701, Lng: -122.4005},
	}
	if len(resp.Shape) != len(want) {
		t.Fatalf("Shape len = %d, want %d: %v", len(resp.Shape), len(want), resp.Shape)
	}
	for i, w := range want {
		if !nearly(resp.Shape[i].Lat, w.Lat) || !nearly(resp.Shape[i].Lng, w.Lng) {
			t.Errorf("Shape[%d] = %v, want %v", i, resp.Shape[i], w)
		}
	}
	if resp.Time != 420 {
		t.Errorf("Time = %d, want 420 (trip.summary.time)", resp.Time)
	}
	if resp.DistanceKm != 0.5 {
		t.Errorf("DistanceKm = %v, want 0.5 (trip.summary.length)", resp.DistanceKm)
	}
}

func TestRouteShapeIsDecodedAtSixDigitPrecision(t *testing.T) {
	srv := newCaptureServer(t, http.StatusOK, okRoute(shapeThreePoints))

	resp, err := valhalla.NewHTTPClient(srv.URL).Route(context.Background(), valhalla.RouteRequest{
		Origin:      valhalla.LatLng{Lat: 37.7, Lng: -122.4},
		Destination: valhalla.LatLng{Lat: 37.701, Lng: -122.4005},
		Costing:     valhalla.CostingPedestrian,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if !nearly(resp.Shape[0].Lat, 37.7) {
		t.Errorf("first point lat = %v, want 37.7 (decoded at 1e6, not 1e5)", resp.Shape[0].Lat)
	}
}

func TestRouteJoinsLegsWithoutRepeatingTheSharedPoint(t *testing.T) {
	two := `{"trip":{"legs":[{"shape":"` + shapeThreePoints + `"},{"shape":"` + shapeThreePoints + `"}]}}`
	srv := newCaptureServer(t, http.StatusOK, two)

	resp, err := valhalla.NewHTTPClient(srv.URL).Route(context.Background(), valhalla.RouteRequest{
		Origin: valhalla.LatLng{Lat: 1, Lng: 2}, Destination: valhalla.LatLng{Lat: 3, Lng: 4},
		Costing: valhalla.CostingPedestrian,
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if len(resp.Shape) != 5 {
		t.Errorf("Shape len = %d, want 5 (3 + 3 sharing one point)", len(resp.Shape))
	}
}

func TestRouteWithoutADrawableShapeIsUpstream(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"no legs", `{"trip":{"legs":[]}}`},
		{"empty shape", `{"trip":{"legs":[{"shape":""}]}}`},
		{"single point", `{"trip":{"legs":[{"shape":"_y_|fA~numhF"}]}}`},

		{"truncated pair", `{"trip":{"legs":[{"shape":"_y_|fA"}]}}`},

		{"unterminated value", `{"trip":{"legs":[{"shape":"~~~~~~~~~~~~~~~~"}]}}`},

		{"byte below the encodable range", `{"trip":{"legs":[{"shape":"_y_|fA ~numhFg^"}]}}`},
		{"byte above the encodable range", `{"trip":{"legs":[{"shape":"_y_|fAÿÿ"}]}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newCaptureServer(t, http.StatusOK, tt.body)

			_, err := valhalla.NewHTTPClient(srv.URL).Route(context.Background(), valhalla.RouteRequest{
				Origin: valhalla.LatLng{Lat: 1, Lng: 2}, Destination: valhalla.LatLng{Lat: 3, Lng: 4},
				Costing: valhalla.CostingPedestrian,
			})
			if !errors.Is(err, valhalla.ErrUpstream) {
				t.Errorf("err = %v, want ErrUpstream", err)
			}
		})
	}
}

func TestRouteWithNoPathIsABadRequest(t *testing.T) {
	srv := newCaptureServer(t, http.StatusBadRequest,
		`{"error_code":442,"error":"No path could be found for input"}`)

	_, err := valhalla.NewHTTPClient(srv.URL).Route(context.Background(), valhalla.RouteRequest{
		Origin: valhalla.LatLng{Lat: 1, Lng: 2}, Destination: valhalla.LatLng{Lat: 3, Lng: 4},
		Costing: valhalla.CostingPedestrian,
	})
	if !errors.Is(err, valhalla.ErrBadRequest) {
		t.Errorf("err = %v, want ErrBadRequest", err)
	}
}

func nearly(got, want float64) bool {
	const tolerance = 1e-9
	d := got - want
	return d < tolerance && d > -tolerance
}

func TestTrailingSlashInBaseURLIsTolerated(t *testing.T) {
	srv := newCaptureServer(t, http.StatusOK, okIsochrone)

	_, err := valhalla.NewHTTPClient(srv.URL+"/").Isochrone(context.Background(), valhalla.IsochroneRequest{
		Origin: valhalla.LatLng{Lat: 1, Lng: 2}, Costing: valhalla.CostingAuto, BudgetSecs: 600,
	})
	if err != nil {
		t.Fatalf("Isochrone: %v", err)
	}
	if srv.path != "/isochrone" {
		t.Errorf("path = %q, want /isochrone (no double slash)", srv.path)
	}
}

func pacific(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatalf("load America/Los_Angeles: %v", err)
	}
	return loc
}

func TestWeekdayDepartAtPicksTheNextWeekdayMorning(t *testing.T) {
	loc := pacific(t)
	tests := []struct {
		now  string
		want string
	}{
		{"2026-08-31T09:00:00-07:00", "2026-08-31T08:00:00-07:00"},
		{"2026-08-28T03:00:00-07:00", "2026-08-28T08:00:00-07:00"},
		{"2026-08-29T15:00:00-07:00", "2026-08-31T08:00:00-07:00"},
		{"2026-08-30T15:00:00-07:00", "2026-08-31T08:00:00-07:00"},
	}
	for _, tc := range tests {
		now, err := time.Parse(time.RFC3339, tc.now)
		if err != nil {
			t.Fatalf("parse now: %v", err)
		}
		got := valhalla.WeekdayDepartAt(now, loc)
		want, err := time.Parse(time.RFC3339, tc.want)
		if err != nil {
			t.Fatalf("parse want: %v", err)
		}
		if !got.Equal(want) {
			t.Errorf("WeekdayDepartAt(%s) = %s, want %s", tc.now, got.Format(time.RFC3339), tc.want)
		}
		if got.Weekday() == time.Saturday || got.Weekday() == time.Sunday {
			t.Errorf("WeekdayDepartAt(%s) landed on a weekend", tc.now)
		}
		if got.Hour() != 8 || got.Minute() != 0 {
			t.Errorf("WeekdayDepartAt(%s) = %s, want 08:00", tc.now, got)
		}
	}
}

func TestWeekdayDepartAt_resolvesOnTheRidersCalendarNotUTC(t *testing.T) {
	loc := pacific(t)
	tests := []struct {
		name string
		now  string
		want string
	}{
		{"midday stays today", "2026-09-02T11:30:00-07:00", "2026-09-02T08:00"},
		{"evening stays today", "2026-09-02T18:00:00-07:00", "2026-09-02T08:00"},
		{"evening before a calendar boundary stays today", "2026-09-15T18:00:00-07:00", "2026-09-15T08:00"},
		{"friday evening stays friday", "2026-09-04T18:00:00-07:00", "2026-09-04T08:00"},
		{"saturday still moves to monday", "2026-09-05T18:00:00-07:00", "2026-09-07T08:00"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tc.now)
			if err != nil {
				t.Fatalf("parse now: %v", err)
			}
			got := valhalla.WeekdayDepartAt(now, loc).Format("2006-01-02T15:04")
			if got != tc.want {
				t.Errorf("WeekdayDepartAt(%s) = %s, want %s", tc.now, got, tc.want)
			}
		})
	}
}

func TestServiceAreaZoneIsTheCorridorsZone(t *testing.T) {
	loc := valhalla.ServiceAreaZone()
	if loc == nil {
		t.Fatal("ServiceAreaZone returned nil")
	}
	if loc.String() != "America/Los_Angeles" {
		t.Errorf("ServiceAreaZone = %s, want America/Los_Angeles", loc)
	}

	if loc == time.UTC {
		t.Error("ServiceAreaZone fell back to UTC")
	}
}

func TestServiceDateIsTheDepartureDate(t *testing.T) {
	depart := time.Date(2026, 9, 2, 8, 0, 0, 0, pacific(t))
	if got := valhalla.ServiceDate(depart); got != "2026-09-02" {
		t.Errorf("ServiceDate = %q, want 2026-09-02", got)
	}

	if got := valhalla.ServiceDate(depart.Add(18 * time.Hour)); got != "2026-09-03" {
		t.Errorf("ServiceDate past midnight = %q, want 2026-09-03", got)
	}
}

func morningDeparture(t *testing.T) time.Time {
	t.Helper()
	return time.Date(2026, 9, 2, 8, 0, 0, 0, pacific(t))
}

func assertMultimodalOptions(t *testing.T, body map[string]any) {
	t.Helper()
	if got := body["costing"]; got != "multimodal" {
		t.Errorf("costing = %v, want multimodal", got)
	}
	dt, _ := body["date_time"].(map[string]any)
	if dt == nil {
		t.Fatal("date_time missing — multimodal costing is load-bearing on a specified departure")
	}
	if dt["type"] != float64(1) {
		t.Errorf("date_time.type = %v, want 1 (depart at)", dt["type"])
	}
	value, _ := dt["value"].(string)
	if _, err := time.Parse("2006-01-02T15:04", value); err != nil {
		t.Errorf("date_time.value = %q, want YYYY-MM-DDTHH:MM: %v", value, err)
	}
	if !strings.HasSuffix(value, "T08:00") {
		t.Errorf("date_time.value = %q, want a 08:00 departure", value)
	}
	opts, _ := body["costing_options"].(map[string]any)
	ped, _ := opts["pedestrian"].(map[string]any)
	if ped == nil {
		t.Fatal("costing_options.pedestrian missing")
	}
	if ped["transit_start_end_max_distance"] != float64(valhalla.TransitStartEndMaxDistanceM) {
		t.Errorf("transit_start_end_max_distance = %v, want %d", ped["transit_start_end_max_distance"], valhalla.TransitStartEndMaxDistanceM)
	}
	assertPedestrianCostingOptionsValhallaHonours(t, ped)
}

var pedestrianCostingOptionsValhallaHonours = map[string]struct{}{
	"type":                           {},
	"max_distance":                   {},
	"walking_speed":                  {},
	"step_penalty":                   {},
	"max_grade":                      {},
	"max_hiking_difficulty":          {},
	"mode_factor":                    {},
	"walkway_factor":                 {},
	"sidewalk_factor":                {},
	"alley_factor":                   {},
	"driveway_factor":                {},
	"transit_start_end_max_distance": {},
	"transit_transfer_max_distance":  {},
	"bss_rent_cost":                  {},
	"bss_rent_penalty":               {},
	"use_hills":                      {},
	"elevator_penalty":               {},
}

func assertPedestrianCostingOptionsValhallaHonours(t *testing.T, ped map[string]any) {
	t.Helper()
	for key := range ped {
		if _, ok := pedestrianCostingOptionsValhallaHonours[key]; !ok {
			t.Errorf("costing_options.pedestrian.%s is not a key Valhalla's pedestrian costing engine reads; unknown keys are dropped silently (SPA-297)", key)
		}
	}
}

func TestIsochrone_multimodalRequestShape(t *testing.T) {
	srv := newCaptureServer(t, http.StatusOK, okIsochrone)
	_, err := valhalla.NewHTTPClient(srv.URL).Isochrone(context.Background(), valhalla.IsochroneRequest{
		Origin:     valhalla.LatLng{Lat: 37.7, Lng: -122.4},
		Costing:    valhalla.CostingMultimodal,
		BudgetSecs: 1800,
		DepartAt:   morningDeparture(t),
	})
	if err != nil {
		t.Fatalf("Isochrone: %v", err)
	}
	assertMultimodalOptions(t, srv.body)
	assertIsochroneDenoiseZero(t, srv.body)
}

func TestRoute_multimodalRequestShape(t *testing.T) {
	srv := newCaptureServer(t, http.StatusOK, okRoute(shapeThreePoints))
	_, err := valhalla.NewHTTPClient(srv.URL).Route(context.Background(), valhalla.RouteRequest{
		Origin:      valhalla.LatLng{Lat: 37.7, Lng: -122.4},
		Destination: valhalla.LatLng{Lat: 37.701, Lng: -122.4005},
		Costing:     valhalla.CostingMultimodal,
		DepartAt:    morningDeparture(t),
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	if srv.path != "/route" {
		t.Errorf("path = %q, want /route", srv.path)
	}
	assertMultimodalOptions(t, srv.body)
}

func TestRenamedWalkCapKeyIsNotARecognisedCostingOption(t *testing.T) {
	if _, ok := pedestrianCostingOptionsValhallaHonours["multimodal_start_end_max_distance"]; ok {
		t.Fatal("multimodal_start_end_max_distance is in the allowlist; that is the key Valhalla ignores (SPA-297)")
	}
}

func departValue(t *testing.T, body map[string]any) string {
	t.Helper()
	dt, _ := body["date_time"].(map[string]any)
	if dt == nil {
		t.Fatal("date_time missing")
	}
	value, _ := dt["value"].(string)
	return value
}

func TestMultimodalSendsTheRequestsOwnDeparture(t *testing.T) {
	depart := time.Date(2026, 9, 2, 13, 6, 0, 0, pacific(t))

	t.Run("isochrone", func(t *testing.T) {
		srv := newCaptureServer(t, http.StatusOK, okIsochrone)
		_, err := valhalla.NewHTTPClient(srv.URL).Isochrone(context.Background(), valhalla.IsochroneRequest{
			Origin:     valhalla.LatLng{Lat: 37.7, Lng: -122.4},
			Costing:    valhalla.CostingMultimodal,
			BudgetSecs: 1800,
			DepartAt:   depart,
		})
		if err != nil {
			t.Fatalf("Isochrone: %v", err)
		}
		if got := departValue(t, srv.body); got != "2026-09-02T13:06" {
			t.Errorf("date_time.value = %q, want 2026-09-02T13:06", got)
		}
	})

	t.Run("route", func(t *testing.T) {
		srv := newCaptureServer(t, http.StatusOK, okRoute(shapeThreePoints))
		_, err := valhalla.NewHTTPClient(srv.URL).Route(context.Background(), valhalla.RouteRequest{
			Origin:      valhalla.LatLng{Lat: 37.7, Lng: -122.4},
			Destination: valhalla.LatLng{Lat: 37.701, Lng: -122.4005},
			Costing:     valhalla.CostingMultimodal,
			DepartAt:    depart,
		})
		if err != nil {
			t.Fatalf("Route: %v", err)
		}
		if got := departValue(t, srv.body); got != "2026-09-02T13:06" {
			t.Errorf("date_time.value = %q, want 2026-09-02T13:06", got)
		}
	})
}

func TestMultimodalWithoutADepartureIsRefused(t *testing.T) {
	var called atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called.Add(1)
		_, _ = w.Write([]byte(okIsochrone))
	}))
	defer srv.Close()
	client := valhalla.NewHTTPClient(srv.URL)

	t.Run("isochrone", func(t *testing.T) {
		_, err := client.Isochrone(context.Background(), valhalla.IsochroneRequest{
			Origin:     valhalla.LatLng{Lat: 37.7, Lng: -122.4},
			Costing:    valhalla.CostingMultimodal,
			BudgetSecs: 1800,
		})
		if !errors.Is(err, valhalla.ErrBadRequest) {
			t.Errorf("Isochrone err = %v, want ErrBadRequest", err)
		}
	})

	t.Run("route", func(t *testing.T) {
		_, err := client.Route(context.Background(), valhalla.RouteRequest{
			Origin:      valhalla.LatLng{Lat: 37.7, Lng: -122.4},
			Destination: valhalla.LatLng{Lat: 37.701, Lng: -122.4005},
			Costing:     valhalla.CostingMultimodal,
		})
		if !errors.Is(err, valhalla.ErrBadRequest) {
			t.Errorf("Route err = %v, want ErrBadRequest", err)
		}
	})

	if n := called.Load(); n != 0 {
		t.Errorf("the server was called %d times; a refused request must not reach the wire", n)
	}
}

func TestNonMultimodalWithoutADepartureIsFine(t *testing.T) {
	srv := newCaptureServer(t, http.StatusOK, okIsochrone)
	_, err := valhalla.NewHTTPClient(srv.URL).Isochrone(context.Background(), valhalla.IsochroneRequest{
		Origin:     valhalla.LatLng{Lat: 37.7, Lng: -122.4},
		Costing:    valhalla.CostingPedestrian,
		BudgetSecs: 1800,
	})
	if err != nil {
		t.Fatalf("Isochrone: %v", err)
	}
	if _, ok := srv.body["date_time"]; ok {
		t.Error("date_time is a multimodal option; pedestrian must not send it")
	}
}

func TestNonMultimodalNeverSendsADeparture(t *testing.T) {
	srv := newCaptureServer(t, http.StatusOK, okIsochrone)
	_, err := valhalla.NewHTTPClient(srv.URL).Isochrone(context.Background(), valhalla.IsochroneRequest{
		Origin:     valhalla.LatLng{Lat: 37.7, Lng: -122.4},
		Costing:    valhalla.CostingPedestrian,
		BudgetSecs: 1800,
		DepartAt:   time.Date(2026, 9, 2, 13, 6, 0, 0, pacific(t)),
	})
	if err != nil {
		t.Fatalf("Isochrone: %v", err)
	}
	if _, ok := srv.body["date_time"]; ok {
		t.Error("date_time is a multimodal option; pedestrian must not send it")
	}
}

func routeWithModes(shape string, modes ...string) string {
	maneuvers := ""
	for i, m := range modes {
		if i > 0 {
			maneuvers += ","
		}
		maneuvers += `{"travel_mode":"` + m + `"}`
	}
	return `{"trip":{"legs":[{"shape":"` + shape + `","maneuvers":[` + maneuvers +
		`]}],"summary":{"time":420,"length":0.5}}}`
}

func TestRoute_reportsWhetherItActuallyRodeTransit(t *testing.T) {
	tests := []struct {
		name  string
		modes []string
		want  bool
	}{
		{"walked the whole way", []string{"pedestrian", "pedestrian"}, false},
		{"boarded something", []string{"pedestrian", "transit", "pedestrian"}, true},
		{"no maneuvers at all", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := newCaptureServer(t, http.StatusOK, routeWithModes(shapeThreePoints, tc.modes...))
			resp, err := valhalla.NewHTTPClient(srv.URL).Route(context.Background(), valhalla.RouteRequest{
				Origin:      valhalla.LatLng{Lat: 37.7, Lng: -122.4},
				Destination: valhalla.LatLng{Lat: 37.701, Lng: -122.4005},
				Costing:     valhalla.CostingMultimodal,
				DepartAt:    morningDeparture(t),
			})
			if err != nil {
				t.Fatalf("Route: %v", err)
			}
			if resp.RodeTransit != tc.want {
				t.Errorf("RodeTransit = %v, want %v", resp.RodeTransit, tc.want)
			}
		})
	}
}

func TestRoute_namesTheOperatorsItRode(t *testing.T) {
	body := `{"trip":{"legs":[{"shape":"` + shapeThreePoints + `","maneuvers":[` +
		`{"travel_mode":"pedestrian"},` +
		`{"travel_mode":"transit","transit_info":{"operator_name":"VTA"}},` +
		`{"travel_mode":"transit","transit_info":{"operator_name":"Caltrain"}},` +
		`{"travel_mode":"transit","transit_info":{"operator_name":"VTA"}},` +
		`{"travel_mode":"transit","transit_info":{}},` +
		`{"travel_mode":"pedestrian"}` +
		`]}],"summary":{"time":420,"length":0.5}}}`
	srv := newCaptureServer(t, http.StatusOK, body)
	resp, err := valhalla.NewHTTPClient(srv.URL).Route(context.Background(), valhalla.RouteRequest{
		Origin:      valhalla.LatLng{Lat: 37.7, Lng: -122.4},
		Destination: valhalla.LatLng{Lat: 37.701, Lng: -122.4005},
		Costing:     valhalla.CostingMultimodal,
		DepartAt:    morningDeparture(t),
	})
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	want := []string{"VTA", "Caltrain"}
	if !slices.Equal(resp.Operators, want) {
		t.Errorf("Operators = %q, want %q", resp.Operators, want)
	}
}

func TestMatrix_multimodalIsRejectedWithoutACall(t *testing.T) {
	var called int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		atomic.AddInt32(&called, 1)
	}))
	defer srv.Close()

	_, err := valhalla.NewHTTPClient(srv.URL).Matrix(context.Background(), valhalla.MatrixRequest{
		Origins:      []valhalla.LatLng{{Lat: 1, Lng: 2}},
		Destinations: []valhalla.LatLng{{Lat: 3, Lng: 4}},
		Costing:      valhalla.CostingMultimodal,
	})
	if !errors.Is(err, valhalla.ErrBadRequest) {
		t.Errorf("err = %v, want ErrBadRequest", err)
	}
	if !strings.Contains(err.Error(), "multimodal") {
		t.Errorf("error %q does not mention multimodal", err)
	}
	if atomic.LoadInt32(&called) != 0 {
		t.Error("Matrix hit the server for multimodal costing; it must refuse locally")
	}
}

func TestTransitCostingRejectionsAreBadRequests(t *testing.T) {
	bodies := []string{
		`{"error_code":140,"error":"Action does not support multimodal costing: sources_to_targets"}`,
		`{"error_code":171,"error":"No matching transit service near this location"}`,
		`{"error_code":442,"error":"No path could be found for input"}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			srv := newCaptureServer(t, http.StatusBadRequest, body)
			_, err := valhalla.NewHTTPClient(srv.URL).Route(context.Background(), valhalla.RouteRequest{
				Origin:      valhalla.LatLng{Lat: 1, Lng: 2},
				Destination: valhalla.LatLng{Lat: 3, Lng: 4},
				Costing:     valhalla.CostingMultimodal,
			})
			if !errors.Is(err, valhalla.ErrBadRequest) {
				t.Errorf("err = %v, want ErrBadRequest", err)
			}
		})
	}
}
