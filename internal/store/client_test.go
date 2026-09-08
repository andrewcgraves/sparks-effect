package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/testsupport"
)

const (
	testJobID       = "6f9619ff-8b86-d011-b42d-00c04fc964ff"
	otherJobID      = "11111111-2222-3333-4444-555555555555"
	firstCompileID  = "aa11bb22-cc33-dd44-ee55-ff6677889900"
	secondCompileID = "bb22cc33-dd44-ee55-ff66-778899001122"
)

func connect(t *testing.T, api *testsupport.FakeAPI) *store.Client {
	t.Helper()
	c, err := store.Connect(context.Background(), api.URL(), api.Token, 0)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return c
}

func TestRoutingJobLifecycle(t *testing.T) {
	api := testsupport.StartFakeAPI(t)
	api.SeedRoutingJob(testJobID)
	client := connect(t, api)
	ctx := context.Background()

	if status, _, _ := api.JobStatus(t, testJobID); status != store.StatusQueued {
		t.Fatalf("seeded status = %q, want queued", status)
	}

	if err := client.MarkRunning(ctx, testJobID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if status, _, _ := api.JobStatus(t, testJobID); status != store.StatusRunning {
		t.Errorf("status = %q, want running", status)
	}

	want := json.RawMessage(`{"type":"FeatureCollection","features":[]}`)
	if err := client.MarkSucceeded(ctx, testJobID, want); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}

	status, errMsg, result := api.JobStatus(t, testJobID)
	if status != store.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if errMsg != "" {
		t.Errorf("error = %q, want empty on success", errMsg)
	}
	var got map[string]any
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if got["type"] != "FeatureCollection" {
		t.Errorf("result type = %v, want FeatureCollection", got["type"])
	}
}

func TestMarkRunningIsIdempotent(t *testing.T) {
	api := testsupport.StartFakeAPI(t)
	api.SeedRoutingJob(testJobID)
	client := connect(t, api)
	ctx := context.Background()

	if err := client.MarkRunning(ctx, testJobID); err != nil {
		t.Fatalf("first MarkRunning: %v", err)
	}
	if err := client.MarkRunning(ctx, testJobID); err != nil {
		t.Errorf("second MarkRunning: %v, want nil — a redelivered job is already running", err)
	}
}

func TestMarkRunningWillNotReopenATerminalJob(t *testing.T) {
	api := testsupport.StartFakeAPI(t)
	client := connect(t, api)
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		close func(string) error
		want  string
	}{
		{"succeeded", func(id string) error { return client.MarkSucceeded(ctx, id, json.RawMessage(`{}`)) }, store.StatusSucceeded},
		{"failed", func(id string) error { return client.MarkFailed(ctx, id, "gave up") }, store.StatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api.SeedRoutingJob(testJobID)
			if err := tc.close(testJobID); err != nil {
				t.Fatalf("closing job: %v", err)
			}

			if err := client.MarkRunning(ctx, testJobID); !errors.Is(err, store.ErrJobNotFound) {
				t.Errorf("MarkRunning on a %s job = %v, want ErrJobNotFound", tc.want, err)
			}
			if status, _, _ := api.JobStatus(t, testJobID); status != tc.want {
				t.Errorf("status = %q, want it to stay %q", status, tc.want)
			}
		})
	}
}

func TestMarkSucceededOverwrites(t *testing.T) {
	api := testsupport.StartFakeAPI(t)
	api.SeedRoutingJob(testJobID)
	client := connect(t, api)
	ctx := context.Background()

	if err := client.MarkSucceeded(ctx, testJobID, json.RawMessage(`{"attempt":1}`)); err != nil {
		t.Fatalf("first MarkSucceeded: %v", err)
	}
	if err := client.MarkSucceeded(ctx, testJobID, json.RawMessage(`{"attempt":2}`)); err != nil {
		t.Fatalf("second MarkSucceeded: %v", err)
	}

	_, _, result := api.JobStatus(t, testJobID)
	var got map[string]any
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if got["attempt"] != float64(2) {
		t.Errorf("result = %v, want the second write to have won", got)
	}
}

func TestMarkFailedRecordsTheReason(t *testing.T) {
	api := testsupport.StartFakeAPI(t)
	api.SeedRoutingJob(testJobID)
	client := connect(t, api)

	const reason = "exhausted 3 retries: valhalla unreachable"
	if err := client.MarkFailed(context.Background(), testJobID, reason); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	status, errMsg, _ := api.JobStatus(t, testJobID)
	if status != store.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	if errMsg != reason {
		t.Errorf("error = %q, want %q", errMsg, reason)
	}
}

func TestUpdatesOnAMissingRowReportNotFound(t *testing.T) {
	api := testsupport.StartFakeAPI(t)
	client := connect(t, api)
	ctx := context.Background()

	if err := client.MarkRunning(ctx, otherJobID); !errors.Is(err, store.ErrJobNotFound) {
		t.Errorf("MarkRunning = %v, want ErrJobNotFound", err)
	}
	if err := client.MarkSucceeded(ctx, otherJobID, json.RawMessage(`{}`)); !errors.Is(err, store.ErrJobNotFound) {
		t.Errorf("MarkSucceeded = %v, want ErrJobNotFound", err)
	}
	if err := client.MarkFailed(ctx, otherJobID, "boom"); !errors.Is(err, store.ErrJobNotFound) {
		t.Errorf("MarkFailed = %v, want ErrJobNotFound", err)
	}
}

func key(compileJobID, slug string, contourMins int) store.IsochroneKey {
	return store.IsochroneKey{
		CompileJobID: compileJobID,
		StationSlug:  slug,
		Mode:         "walk",
		ContourMins:  contourMins,
	}
}

func polygon(slug string) json.RawMessage {
	return json.RawMessage(`{"type":"FeatureCollection","features":[{"slug":"` + slug + `"}]}`)
}

func TestIsochroneCache_roundTrip(t *testing.T) {
	api := testsupport.StartFakeAPI(t)
	client := connect(t, api)
	ctx := context.Background()

	stored := key(firstCompileID, "station-a", 30)
	missing := key(firstCompileID, "station-b", 30)

	if err := client.Put(ctx, []store.CachedIsochrone{
		{Key: stored, Geometry: polygon("station-a"), TilesetAt: time.Now().UTC()},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := client.Get(ctx, []store.IsochroneKey{stored, missing})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Get returned %d entries, want 1 (the other key was never stored)", len(got))
	}
	var round map[string]any
	if err := json.Unmarshal(got[stored], &round); err != nil {
		t.Fatalf("cached geometry is not valid JSON: %v", err)
	}
	if round["type"] != "FeatureCollection" {
		t.Errorf("geometry type = %v, want FeatureCollection", round["type"])
	}
	if _, ok := got[missing]; ok {
		t.Error("Get returned an entry for a key that was never stored")
	}
}

func TestIsochroneCache_transitModeRoundTrips(t *testing.T) {
	api := testsupport.StartFakeAPI(t)
	client := connect(t, api)
	ctx := context.Background()

	stored := store.IsochroneKey{
		CompileJobID: firstCompileID,
		StationSlug:  "station-a",
		Mode:         "transit",
		ContourMins:  45,
	}
	if err := client.Put(ctx, []store.CachedIsochrone{
		{Key: stored, Geometry: polygon("transit-a")},
	}); err != nil {
		t.Fatalf("Put(mode=transit): %v", err)
	}

	got, err := client.Get(ctx, []store.IsochroneKey{stored, key(firstCompileID, "station-a", 45)})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := got[stored]; !ok {
		t.Fatal("transit-keyed row missing — the cache key must accept mode=transit")
	}
	walk := key(firstCompileID, "station-a", 45)
	if _, ok := got[walk]; ok {
		t.Error("a walk-keyed lookup was served the transit polygon")
	}
}

func TestIsochroneCache_serviceDateDiscriminatesTransitRows(t *testing.T) {
	api := testsupport.StartFakeAPI(t)
	client := connect(t, api)
	ctx := context.Background()

	onDate := func(date string) store.IsochroneKey {
		return store.IsochroneKey{
			CompileJobID: firstCompileID, StationSlug: "station-a",
			Mode: "transit", ContourMins: 45, DepartsOn: date,
		}
	}
	wednesday, thursday := onDate("2026-09-02"), onDate("2026-09-03")

	if err := client.Put(ctx, []store.CachedIsochrone{
		{Key: wednesday, Geometry: polygon("wednesday")},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := client.Get(ctx, []store.IsochroneKey{wednesday, thursday})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, ok := got[wednesday]; !ok {
		t.Error("the row written for Wednesday was not served back for Wednesday")
	}
	if _, ok := got[thursday]; ok {
		t.Error("Wednesday's polygon was served for Thursday's question")
	}

	if err := client.Put(ctx, []store.CachedIsochrone{
		{Key: thursday, Geometry: polygon("thursday")},
	}); err != nil {
		t.Fatalf("Put(thursday): %v", err)
	}
	if got := api.IsochroneCacheCount(firstCompileID); got != 2 {
		t.Errorf("cache holds %d rows for the compile job, want one per service date", got)
	}
}

func TestIsochroneCache_everyKeyFieldDiscriminates(t *testing.T) {
	api := testsupport.StartFakeAPI(t)
	client := connect(t, api)
	ctx := context.Background()

	stored := key(firstCompileID, "station-a", 30)
	if err := client.Put(ctx, []store.CachedIsochrone{{Key: stored, Geometry: polygon("station-a")}}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	differsBy := map[string]store.IsochroneKey{
		"compile job":  key(secondCompileID, "station-a", 30),
		"station slug": key(firstCompileID, "station-b", 30),
		"contour":      key(firstCompileID, "station-a", 31),
		"mode": {
			CompileJobID: firstCompileID, StationSlug: "station-a",
			Mode: "bike", ContourMins: 30,
		},
		"service date": {
			CompileJobID: firstCompileID, StationSlug: "station-a",
			Mode: "walk", ContourMins: 30, DepartsOn: "2026-09-02",
		},
	}

	for field, k := range differsBy {
		got, err := client.Get(ctx, []store.IsochroneKey{k})
		if err != nil {
			t.Fatalf("Get differing by %s: %v", field, err)
		}
		if len(got) != 0 {
			t.Errorf("a key differing only by %s was served the stored polygon", field)
		}
	}
}

func TestIsochroneCache_recompileDoesNotReadPreviousGeneration(t *testing.T) {
	api := testsupport.StartFakeAPI(t)
	client := connect(t, api)
	ctx := context.Background()

	before := key(firstCompileID, "station-a", 30)
	if err := client.Put(ctx, []store.CachedIsochrone{{Key: before, Geometry: polygon("before")}}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	after := key(secondCompileID, "station-a", 30)
	got, err := client.Get(ctx, []store.IsochroneKey{after})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("the recompiled graph read %d entries from the previous generation, want 0", len(got))
	}
	if n := api.IsochroneCacheCount(firstCompileID); n != 1 {
		t.Errorf("previous generation holds %d rows, want 1 — rotation must not purge anything", n)
	}
}

func TestIsochroneCache_tilesetIsRecordedButNotKeyed(t *testing.T) {
	api := testsupport.StartFakeAPI(t)
	client := connect(t, api)
	ctx := context.Background()

	built := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	k := key(firstCompileID, "station-a", 30)
	if err := client.Put(ctx, []store.CachedIsochrone{
		{Key: k, Geometry: polygon("a"), TilesetAt: built},
	}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	stamps := api.IsochroneCacheStamps(firstCompileID)
	if len(stamps) != 1 {
		t.Fatalf("cache rows = %d, want 1", len(stamps))
	}
	if !stamps[0].Equal(built) {
		t.Errorf("tileset_at = %s, want %s", stamps[0], built)
	}

	got, err := client.Get(ctx, []store.IsochroneKey{k})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Get found %d entries, want 1 — the tileset must not narrow the lookup", len(got))
	}
}

func TestIsochroneCache_repeatedPutIsNotAnError(t *testing.T) {
	api := testsupport.StartFakeAPI(t)
	client := connect(t, api)
	ctx := context.Background()

	entry := store.CachedIsochrone{Key: key(firstCompileID, "station-a", 30), Geometry: polygon("first")}
	if err := client.Put(ctx, []store.CachedIsochrone{entry}); err != nil {
		t.Fatalf("first Put: %v", err)
	}
	entry.Geometry = polygon("second")
	if err := client.Put(ctx, []store.CachedIsochrone{entry}); err != nil {
		t.Fatalf("second Put: %v", err)
	}

	if n := api.IsochroneCacheCount(firstCompileID); n != 1 {
		t.Errorf("cache rows = %d, want 1", n)
	}
	got, err := client.Get(ctx, []store.IsochroneKey{entry.Key})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	var round map[string]any
	if err := json.Unmarshal(got[entry.Key], &round); err != nil {
		t.Fatalf("cached geometry is not valid JSON: %v", err)
	}
	if round["features"].([]any)[0].(map[string]any)["slug"] != "first" {
		t.Errorf("second Put overwrote the first polygon; conflicts must be left alone")
	}
}

func TestIsochroneCache_emptyBatchesAreNoOps(t *testing.T) {
	api := testsupport.StartFakeAPI(t)
	client := connect(t, api)
	ctx := context.Background()

	got, err := client.Get(ctx, nil)
	if err != nil {
		t.Fatalf("Get(nil): %v", err)
	}
	if len(got) != 0 {
		t.Errorf("Get(nil) returned %d entries, want 0", len(got))
	}
	if err := client.Put(ctx, nil); err != nil {
		t.Fatalf("Put(nil): %v", err)
	}
}

func TestConnectRejectsABadToken(t *testing.T) {
	api := testsupport.StartFakeAPI(t)
	_, err := store.Connect(context.Background(), api.URL(), "wrong-token", 0)
	if err == nil {
		t.Fatal("Connect with a rejected token: want an error, got nil")
	}
}

func TestConnectRejectsAnUnreachableAPI(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	srv.Close()
	_, err := store.Connect(context.Background(), srv.URL, testsupport.DefaultWorkerToken, time.Second)
	if err == nil {
		t.Fatal("Connect against a closed server: want an error, got nil")
	}
}

func TestConnectRequiresAnAbsoluteURL(t *testing.T) {
	_, err := store.Connect(context.Background(), "not-a-url", "token", 0)
	if err == nil {
		t.Fatal("Connect with a relative API_URL: want an error, got nil")
	}
}

func TestServerErrorIsNotJobNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/internal/worker" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		http.Error(w, `{"error":"db down"}`, http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)

	client, err := store.Connect(context.Background(), srv.URL, "any", 0)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	err = client.MarkRunning(context.Background(), testJobID)
	if err == nil {
		t.Fatal("MarkRunning against a 500: want an error, got nil")
	}
	if errors.Is(err, store.ErrJobNotFound) {
		t.Fatal("a 500 was reported as ErrJobNotFound; the job would be discarded rather than retried")
	}
}

func TestClientSendsBearerToken(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	const token = "secret-token"
	client, err := store.Connect(context.Background(), srv.URL, token, 0)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if gotAuth != "Bearer "+token {
		t.Fatalf("ping Authorization = %q, want Bearer %s", gotAuth, token)
	}

	gotAuth = ""
	_ = client.MarkRunning(context.Background(), testJobID)
	if gotAuth != "Bearer "+token {
		t.Errorf("MarkRunning Authorization = %q, want Bearer %s", gotAuth, token)
	}
}
