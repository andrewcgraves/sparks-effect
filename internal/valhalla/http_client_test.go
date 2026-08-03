package valhalla_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

// captureServer records the last request body it was sent, so a test can assert
// on the JSON the client actually put on the wire rather than on the Go values
// it was given.
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

	// Valhalla names the longitude field "lon"; this codebase spells it "lng"
	// everywhere else, so the translation happens exactly here and is worth
	// pinning.
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

	// The contour goes out in minutes, not seconds. Sending 2700 would be a
	// 45-hour isochrone and a 400.
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
}

func TestMatrix_requestShape(t *testing.T) {
	srv := newCaptureServer(t, http.StatusOK, `{"sources_to_targets":[[{"time":600,"distance":1.0},{"time":-1,"distance":0}]]}`)
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
	if sources, _ := srv.body["sources"].([]any); len(sources) != 1 {
		t.Errorf("sources: want 1, got %v", srv.body["sources"])
	}
	if targets, _ := srv.body["targets"].([]any); len(targets) != 2 {
		t.Errorf("targets: want 2, got %v", srv.body["targets"])
	}

	if len(resp.SourcesToTargets) != 1 || len(resp.SourcesToTargets[0]) != 2 {
		t.Fatalf("SourcesToTargets shape = %v", resp.SourcesToTargets)
	}
	if resp.SourcesToTargets[0][0].Time != 600 {
		t.Errorf("cell 0 time = %d, want 600", resp.SourcesToTargets[0][0].Time)
	}
	// -1 is Valhalla's "unreachable", and it must survive decoding as -1
	// rather than being coerced to zero, which would read as "instant".
	if resp.SourcesToTargets[0][1].Time != -1 {
		t.Errorf("cell 1 time = %d, want -1 (unreachable)", resp.SourcesToTargets[0][1].Time)
	}
}

// No Authorization header. The server is a ClusterIP service with no ingress,
// so the network is the authorization; a stray vendor header carried over from
// the Stadia client would be sent to something that never asked for it.
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

// The status→cause mapping is what the retry decision rests on, so it is
// asserted per status rather than inferred.
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

// Valhalla's error_code catalogue is how a failed job says which limit it hit —
// 150 is too many matrix pairs, 154 a pair too far apart — so it has to survive
// into the message a caller reads off the row.
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

// An unreachable server is transient. This is the case that decides whether a
// job survives a Valhalla pod restart or is thrown away by it.
func TestUnreachableServerIsUpstream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	_, err := valhalla.NewHTTPClient(url).Isochrone(context.Background(), valhalla.IsochroneRequest{
		Origin: valhalla.LatLng{Lat: 1, Lng: 2}, Costing: valhalla.CostingAuto, BudgetSecs: 600,
	})
	if !errors.Is(err, valhalla.ErrUpstream) {
		t.Errorf("error = %v, want ErrUpstream", err)
	}
}

// tileset_last_modified is what the isochrone cache records alongside a cached
// polygon, and Valhalla exposes it without allow_verbose — which the deployed
// container leaves off.
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

// A server that reports no tileset timestamp must leave the field zero rather
// than claiming the Unix epoch, which would later read as a real date.
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
