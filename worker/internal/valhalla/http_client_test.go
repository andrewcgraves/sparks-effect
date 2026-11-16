package valhalla_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
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

func TestDepartureRollsOver_atTheMidnightEndingTheServiceDate(t *testing.T) {
	loc := pacific(t)
	tests := []struct {
		name string
		now  string
		want string
	}{
		{"a weekday rolls over at its own midnight", "2026-09-02T11:30:00-07:00", "2026-09-03T00:00:00-07:00"},
		{"friday rolls over into the weekend", "2026-09-04T18:00:00-07:00", "2026-09-05T00:00:00-07:00"},
		{"a weekend keeps monday until monday ends", "2026-09-05T18:00:00-07:00", "2026-09-08T00:00:00-07:00"},
		{"across the autumn clock change", "2026-10-30T09:00:00-07:00", "2026-10-31T00:00:00-07:00"},
		{"onto the day the clocks go back", "2026-11-01T09:00:00-08:00", "2026-11-03T00:00:00-08:00"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			now, err := time.Parse(time.RFC3339, tc.now)
			if err != nil {
				t.Fatalf("parse now: %v", err)
			}
			departAt := valhalla.WeekdayDepartAt(now, loc)
			got := valhalla.DepartureRollsOver(departAt)
			want, err := time.Parse(time.RFC3339, tc.want)
			if err != nil {
				t.Fatalf("parse want: %v", err)
			}
			if !got.Equal(want) {
				t.Errorf("DepartureRollsOver(%s) = %s, want %s", departAt, got.Format(time.RFC3339), tc.want)
			}

			// The contract, checked against the clock itself: the same departure
			// until the instant before, a different one from it.
			if before := valhalla.WeekdayDepartAt(got.Add(-time.Nanosecond), loc); !before.Equal(departAt) {
				t.Errorf("just before rollover the clock chose %s, want %s", before, departAt)
			}
			if after := valhalla.WeekdayDepartAt(got, loc); after.Equal(departAt) {
				t.Errorf("at rollover the clock still chose %s", after)
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

type fixtureStop struct {
	arrives, departs string
}

type fixtureManeuver struct {
	mode  string
	secs  float64
	stops []fixtureStop
}

// Valhalla writes a transit stop's clocks with the offset inline (its own
// test/gurka/test_gtfs.cc asserts "2023-02-27T23:58-05:00"), and echoes the
// origin's date_time bare, with the offset in a separate field. The fixtures
// do the same unless a test says otherwise.
func stopClock(hhmm string) string {
	return "2026-09-02T" + hhmm + "-07:00"
}

func bareClock(hhmm string) string {
	return "2026-09-02T" + hhmm
}

const originClock = "2026-09-02T08:00"

func walking(secs float64) fixtureManeuver {
	return fixtureManeuver{mode: "pedestrian", secs: secs}
}

func riding(secs float64, boards, alights string) fixtureManeuver {
	return fixtureManeuver{mode: "transit", secs: secs, stops: []fixtureStop{{departs: boards}, {arrives: alights}}}
}

func tripBody(t *testing.T, originClock string, summarySecs float64, maneuvers ...fixtureManeuver) string {
	t.Helper()
	ms := make([]map[string]any, len(maneuvers))
	for i, m := range maneuvers {
		ms[i] = map[string]any{"travel_mode": m.mode, "time": m.secs}
		if m.mode != "transit" {
			continue
		}
		stops := make([]map[string]any, len(m.stops))
		for j, st := range m.stops {
			stop := map[string]any{"type": "stop", "name": fmt.Sprintf("stop %d", j)}
			if st.arrives != "" {
				stop["arrival_date_time"] = st.arrives
			}
			if st.departs != "" {
				stop["departure_date_time"] = st.departs
			}
			stops[j] = stop
		}
		ms[i]["transit_info"] = map[string]any{"operator_name": "VTA", "transit_stops": stops}
	}
	trip := map[string]any{
		"legs":    []map[string]any{{"shape": shapeThreePoints, "maneuvers": ms}},
		"summary": map[string]any{"time": summarySecs, "length": 2.06},
	}
	if originClock != "" {
		trip["locations"] = []map[string]any{
			{"type": "break", "lat": 37.7, "lon": -122.4, "date_time": originClock, "time_zone_offset": "-07:00"},
			{"type": "break", "lat": 37.701, "lon": -122.4005},
		}
	}
	raw, err := json.Marshal(map[string]any{"trip": trip})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	return string(raw)
}

func routeFixture(t *testing.T, costing valhalla.Costing, body string) *valhalla.RouteResponse {
	t.Helper()
	srv := newCaptureServer(t, http.StatusOK, body)
	resp, err := valhalla.NewHTTPClient(srv.URL).Route(context.Background(), valhalla.RouteRequest{
		Origin:      valhalla.LatLng{Lat: 37.7, Lng: -122.4},
		Destination: valhalla.LatLng{Lat: 37.701, Lng: -122.4005},
		Costing:     costing,
		DepartAt:    morningDeparture(t),
	})
	if err != nil {
		t.Fatalf("Route: %v — an unreconciled split must never fail the route", err)
	}
	return resp
}

func assertBreakdown(t *testing.T, resp *valhalla.RouteResponse, want valhalla.Breakdown) {
	t.Helper()
	if resp.BreakdownErr != nil {
		t.Fatalf("BreakdownErr = %v, want a reconciled split", resp.BreakdownErr)
	}
	if resp.Breakdown == nil {
		t.Fatal("Breakdown is nil, want a split")
	}
	if *resp.Breakdown != want {
		t.Errorf("Breakdown = %+v, want %+v", *resp.Breakdown, want)
	}
	if got := resp.Breakdown.Total(); got != resp.Time {
		t.Errorf("walk + wait + ride = %d s, but the access time is %d s; the split must sum to it exactly", got, resp.Time)
	}
}

func assertUnreconciled(t *testing.T, resp *valhalla.RouteResponse) {
	t.Helper()
	if !errors.Is(resp.BreakdownErr, valhalla.ErrUnreconciled) {
		t.Errorf("BreakdownErr = %v, want ErrUnreconciled", resp.BreakdownErr)
	}
	if resp.Breakdown != nil {
		t.Errorf("Breakdown = %+v, want none: a split that does not reconcile is withheld, not rounded", *resp.Breakdown)
	}
}

// The forced SJSU -> Diridon itinerary SPA-335 recorded, departing 08:00: a
// 22.9 min walk reaches the platform at 08:22:54, the Green Line departs
// 08:31 and arrives 08:40, the transit maneuver's own time is 16.7 min with
// the wait folded inside it, and a 6.9 min walk follows. 46.5 min in all.
// The maneuver is a 9.0 min timetabled ride and a 7.7 min wait; the clock gap
// from the platform to the 08:31 departure is 8.1 min, 24 s more.
const (
	spa335Platform    = 600 + 700 + 74
	spa335Maneuver    = 1002
	spa335Egress      = 414
	spa335Total       = spa335Platform + spa335Maneuver + spa335Egress
	spa335Ride        = 540
	spa335Wait        = spa335Maneuver - spa335Ride
	spa335ClockGapSec = 486
)

func spa335TripReaching(platformSecs float64, clock func(string) string, alights string) []fixtureManeuver {
	return []fixtureManeuver{
		walking(600), walking(700), walking(platformSecs - 1300),
		{mode: "transit", secs: spa335Maneuver, stops: []fixtureStop{
			{departs: clock("08:31")},
			{arrives: clock("08:35"), departs: clock("08:35")},
			{arrives: clock(alights)},
		}},
		walking(spa335Egress),
	}
}

func spa335Trip() []fixtureManeuver {
	return spa335TripReaching(spa335Platform, stopClock, "08:40")
}

func spa335Split() valhalla.Breakdown {
	return valhalla.Breakdown{WalkSecs: spa335Platform + spa335Egress, WaitSecs: spa335Wait, RideSecs: spa335Ride}
}

func TestRoute_takesTheWaitOutOfTheTransitManeuverAsSPA335MeasuredIt(t *testing.T) {
	// The ride is the timetabled 08:31 to 08:40, 9.0 min, and the wait is what
	// the 16.7 min maneuver holds beyond it, 7.7 min: the wait comes out of
	// the maneuver. Adding the 8.1 min clock gap beside the maneuver instead
	// would report 54.6 min for a 46.5 min trip.
	if spa335Wait != 462 || spa335ClockGapSec-spa335Wait != 24 {
		t.Fatalf("fixture: wait %d s, %d s from the clock gap; SPA-335 measured 462 s and 24 s", spa335Wait, spa335ClockGapSec-spa335Wait)
	}
	tests := []struct {
		name   string
		origin string
		clock  func(string) string
	}{
		{"anchored on the origin's echoed date_time", originClock, stopClock},
		{"anchored on the departure the request sent", "", stopClock},
		{"an origin date_time carrying its offset", originClock + "-07:00", stopClock},
		{"stop clocks without an offset", originClock, bareClock},
		{"stop clocks in another offset still read as local wall clocks", originClock, func(hhmm string) string {
			return "2026-09-02T" + hhmm + "+09:00"
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := routeFixture(t, valhalla.CostingMultimodal,
				tripBody(t, tc.origin, spa335Total, spa335TripReaching(spa335Platform, tc.clock, "08:40")...))
			if resp.Time != spa335Total {
				t.Fatalf("Time = %d, want the unchanged %d s total", resp.Time, spa335Total)
			}
			if !resp.RodeTransit {
				t.Error("RodeTransit = false on a leg with a transit maneuver")
			}
			assertBreakdown(t, resp, spa335Split())
		})
	}
}

func TestRoute_reconcilesTheClockGapAgainstTheWaitToTheSecond(t *testing.T) {
	// The wait the maneuver leaves is checked against the clock gap from the
	// platform to the boarding departure. The boarding clock is minute
	// precision, so up to ReconcileToleranceSecs either way is its rounding;
	// a second more refuses the split. Moving the platform arrival moves the
	// gap and leaves the wait alone.
	tests := []struct {
		name         string
		platform     float64
		disagreement int
		reconciles   bool
	}{
		{"SPA-335's measured 24 s", spa335Platform, 24, true},
		{"exactly 60 s late", 1338, 60, true},
		{"61 s late", 1337, 61, false},
		{"exactly 60 s early", 1458, -60, true},
		{"61 s early", 1459, -61, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if gap := 1860 - int(tc.platform); gap-spa335Wait != tc.disagreement {
				t.Fatalf("fixture: a %d s gap is %d s from the %d s wait, not %d", gap, gap-spa335Wait, spa335Wait, tc.disagreement)
			}
			total := tc.platform + spa335Maneuver + spa335Egress
			resp := routeFixture(t, valhalla.CostingMultimodal,
				tripBody(t, originClock, total, spa335TripReaching(tc.platform, stopClock, "08:40")...))
			if resp.Time != int(total) {
				t.Errorf("Time = %d, want summary.time unchanged whatever the split does", resp.Time)
			}
			if !tc.reconciles {
				assertUnreconciled(t, resp)
				return
			}
			assertBreakdown(t, resp, valhalla.Breakdown{
				WalkSecs: int(tc.platform) + spa335Egress,
				WaitSecs: spa335Wait,
				RideSecs: spa335Ride,
			})
		})
	}
}

func TestRoute_theAlightingClockSetsTheRide(t *testing.T) {
	// The ride is alighting clock minus boarding clock, so moving the 08:40
	// arrival moves the ride and, inside the same maneuver, the wait.
	t.Run("a minute early still reconciles", func(t *testing.T) {
		resp := routeFixture(t, valhalla.CostingMultimodal,
			tripBody(t, originClock, spa335Total, spa335TripReaching(spa335Platform, stopClock, "08:39")...))
		// Ride 480 s, wait 522 s, 36 s under the 486 s clock gap.
		assertBreakdown(t, resp, valhalla.Breakdown{WalkSecs: spa335Platform + spa335Egress, WaitSecs: 522, RideSecs: 480})
	})
	t.Run("a minute late leaves a wait 84 s off the clock gap", func(t *testing.T) {
		resp := routeFixture(t, valhalla.CostingMultimodal,
			tripBody(t, originClock, spa335Total, spa335TripReaching(spa335Platform, stopClock, "08:41")...))
		assertUnreconciled(t, resp)
	})
}

func TestRoute_putsTheRoundingResidueOnTheWalk(t *testing.T) {
	// The components sum to the rounded total exactly, so the walk carries the
	// sub-second rounding of the total, the wait and the ride, plus any float
	// drift within SummaryDriftToleranceSecs, and nothing more: it never
	// strays from the walking maneuvers' sum by more than that.
	tests := []struct {
		name             string
		summary          float64
		maneuver, egress float64
		walk             int
	}{
		{"a total that rounds down", spa335Total + 0.4, spa335Maneuver, spa335Egress + 0.4, spa335Platform + spa335Egress},
		{"a total that rounds up", spa335Total + 0.6, spa335Maneuver, spa335Egress + 0.6, spa335Platform + spa335Egress + 1},
		{"a fractional transit maneuver", spa335Total + 0.4, spa335Maneuver + 0.4, spa335Egress, spa335Platform + spa335Egress},
		{"1.4 s of float drift", spa335Total + 1.4, spa335Maneuver, spa335Egress, spa335Platform + spa335Egress + 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			trip := spa335Trip()
			trip[3].secs, trip[4].secs = tc.maneuver, tc.egress
			resp := routeFixture(t, valhalla.CostingMultimodal, tripBody(t, originClock, tc.summary, trip...))
			assertBreakdown(t, resp, valhalla.Breakdown{WalkSecs: tc.walk, WaitSecs: spa335Wait, RideSecs: spa335Ride})
		})
	}
}

func TestRoute_refusesManeuversThatDriftFromTheTotal(t *testing.T) {
	// SPA-335's maneuvers sum to summary.time exactly. Past float rounding the
	// difference would land on the walk, so the split is refused instead.
	for _, drift := range []float64{3, -3} {
		t.Run(fmt.Sprintf("%+.0f s", drift), func(t *testing.T) {
			resp := routeFixture(t, valhalla.CostingMultimodal,
				tripBody(t, originClock, spa335Total+drift, spa335Trip()...))
			if resp.Time != spa335Total+int(drift) {
				t.Errorf("Time = %d, want summary.time unchanged whatever the split does", resp.Time)
			}
			assertUnreconciled(t, resp)
		})
	}
}

func TestRoute_aLegWalkedEndToEndIsAllWalk(t *testing.T) {
	// Walk = total holds without measuring anything, so a walked leg is split
	// whatever its maneuvers sum to and whatever clock Valhalla echoed.
	tests := []struct {
		name      string
		origin    string
		summary   float64
		maneuvers []fixtureManeuver
	}{
		{"maneuvers that sum to the total", "", 1800.4,
			[]fixtureManeuver{walking(900), walking(600), walking(300.4)}},
		{"maneuvers far from the total", originClock, 1800,
			[]fixtureManeuver{walking(900), walking(600)}},
		{"a bad origin clock it never needs", "08:00", 1800,
			[]fixtureManeuver{walking(1800)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := routeFixture(t, valhalla.CostingMultimodal, tripBody(t, tc.origin, tc.summary, tc.maneuvers...))
			if resp.RodeTransit {
				t.Error("RodeTransit = true on a leg with no transit maneuver")
			}
			assertBreakdown(t, resp, valhalla.Breakdown{WalkSecs: 1800})
		})
	}
}

func TestRoute_eachTransferMeasuresItsOwnWait(t *testing.T) {
	// 08:05:00 at the first platform, 2 min wait for 08:07, ride to 08:15,
	// a 2 min transfer walk to 08:17:00, 8 min wait for 08:25, ride to 08:40.
	// Measuring the second wait from the trip's start, or from the first
	// boarding, would get it wrong.
	resp := routeFixture(t, valhalla.CostingMultimodal, tripBody(t, originClock, 2460,
		walking(300),
		riding(600, stopClock("08:07"), stopClock("08:15")),
		walking(120),
		riding(1380, stopClock("08:25"), stopClock("08:40")),
		walking(60),
	))
	assertBreakdown(t, resp, valhalla.Breakdown{WalkSecs: 480, WaitSecs: 120 + 480, RideSecs: 480 + 900})
}

func TestRoute_aDepartureJustBeforeThePlatformClockIsNoWait(t *testing.T) {
	// 08:05:20 at the platform for an 08:05 departure: 20 s of minute
	// rounding, not a train that left before the rider arrived.
	resp := routeFixture(t, valhalla.CostingMultimodal, tripBody(t, originClock, 640+600,
		walking(320),
		riding(600, stopClock("08:05"), stopClock("08:15")),
		walking(320),
	))
	assertBreakdown(t, resp, valhalla.Breakdown{WalkSecs: 640, WaitSecs: 0, RideSecs: 600})
}

func TestRoute_refusesASplitTheClocksCannotPlace(t *testing.T) {
	tests := []struct {
		name      string
		origin    string
		maneuvers []fixtureManeuver
		summary   float64
	}{
		{"no boarding departure", originClock, []fixtureManeuver{
			walking(300), riding(600, "", stopClock("08:15")),
		}, 900},
		{"no alighting arrival", originClock, []fixtureManeuver{
			walking(300), riding(600, stopClock("08:07"), ""),
		}, 900},
		{"a stop clock that is not a clock", originClock, []fixtureManeuver{
			walking(300), riding(600, "08:07", stopClock("08:15")),
		}, 900},
		{"no stops at all", originClock, []fixtureManeuver{
			walking(300), {mode: "transit", secs: 600},
		}, 900},
		{"departs well before the rider reaches the platform", originClock, []fixtureManeuver{
			walking(300), riding(600, stopClock("08:03"), stopClock("08:15")),
		}, 900},
		{"alights before it boards", originClock, []fixtureManeuver{
			walking(300), riding(600, stopClock("08:07"), stopClock("08:06")),
		}, 900},
		{"an origin date_time that is not a clock", "08:00", []fixtureManeuver{
			walking(300), riding(600, stopClock("08:07"), stopClock("08:15")),
		}, 900},
		{"a negative maneuver time", originClock, []fixtureManeuver{
			walking(310), walking(-10), riding(600, stopClock("08:07"), stopClock("08:15")),
		}, 900},
		// Riding from the first second, 600 s by the timetable, inside a total
		// that rounds to 599 s: within the float drift allowance, but the
		// clocked wait and ride alone exceed the total and leave a negative walk.
		{"a ride longer than the whole leg", originClock, []fixtureManeuver{
			riding(600, stopClock("08:00"), stopClock("08:10")),
		}, 598.5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			resp := routeFixture(t, valhalla.CostingMultimodal, tripBody(t, tc.origin, tc.summary, tc.maneuvers...))
			if want := int(math.Round(tc.summary)); resp.Time != want {
				t.Errorf("Time = %d, want %d: the total stands without its split", resp.Time, want)
			}
			assertUnreconciled(t, resp)
		})
	}
}

func TestRoute_onlyMultimodalHasABreakdown(t *testing.T) {
	resp := routeFixture(t, valhalla.CostingPedestrian, tripBody(t, "", 1800, walking(1800)))
	if resp.Breakdown != nil || resp.BreakdownErr != nil {
		t.Errorf("pedestrian route: Breakdown = %v, BreakdownErr = %v; want neither", resp.Breakdown, resp.BreakdownErr)
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
