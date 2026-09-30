package store

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// workerStoreEnvelope is the HTTP contract SPA-273 left without a fixture on
// this side: the cache lookup/put wrappers and the job-transition bodies.
// The JSON tags live on the hand-copied types in this package; a renamed
// field decodes as its zero value and every cache lookup silently misses.
type workerStoreEnvelope struct {
	CacheLookupRequest  cacheLookupRequest  `json:"cache_lookup_request"`
	CacheLookupResponse cacheLookupResponse `json:"cache_lookup_response"`
	CachePutRequest     cachePutRequest     `json:"cache_put_request"`
	JobSucceeded        jobSucceededBody    `json:"job_succeeded"`
	JobFailed           jobFailedBody       `json:"job_failed"`
}

func goldenIsochroneKey() IsochroneKey {
	return IsochroneKey{
		CompileJobID: "0f3b7c2a-4d1e-4a5b-9c8d-2e6f1a0b3c4d",
		StationSlug:  "north",
		Mode:         "transit",
		ContourMins:  30,
		// departs_on is omitempty. A zero here would drop the SPA-269 field
		// from the fixture and leave the exact drift that motivated the key
		// change unguarded.
		DepartsOn: "2026-09-02",
		// budget_mins is omitempty too, and missing it is SPA-326's failure
		// mode: two budgets at one contour silently share a transit row.
		BudgetMins: 90,
	}
}

func goldenGeometry() json.RawMessage {
	return json.RawMessage(`{"type":"Polygon","coordinates":[[[-122.4,37.8],[-122.3,37.8],[-122.3,37.7],[-122.4,37.8]]]}`)
}

func goldenTilesetAt() time.Time {
	return time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
}

func goldenEnvelope() workerStoreEnvelope {
	key := goldenIsochroneKey()
	geom := goldenGeometry()
	return workerStoreEnvelope{
		CacheLookupRequest: cacheLookupRequest{Keys: []IsochroneKey{key}},
		CacheLookupResponse: cacheLookupResponse{
			Entries: []cacheLookupEntry{{Key: key, Geometry: geom}},
		},
		CachePutRequest: cachePutRequest{
			Entries: []CachedIsochrone{{
				Key:       key,
				Geometry:  geom,
				TilesetAt: goldenTilesetAt(),
			}},
		},
		JobSucceeded: jobSucceededBody{Result: geom},
		JobFailed:    jobFailedBody{Error: "valhalla unreachable"},
	}
}

func workerStoreGoldenPath() string {
	return filepath.Join("testdata", "worker-store.golden.json")
}

func TestWorkerStoreEnvelope_roundTripsThroughTheFixture(t *testing.T) {
	raw, err := os.ReadFile(workerStoreGoldenPath())
	if err != nil {
		t.Fatalf("reading golden fixture: %v", err)
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var got workerStoreEnvelope
	if err := dec.Decode(&got); err != nil {
		t.Fatalf("the fixture carries a field no struct in this package declares: %v", err)
	}

	want := goldenEnvelope()
	if got.CacheLookupRequest.Keys[0] != want.CacheLookupRequest.Keys[0] {
		t.Errorf("lookup key = %+v, want %+v", got.CacheLookupRequest.Keys[0], want.CacheLookupRequest.Keys[0])
	}
	if got.CacheLookupResponse.Entries[0].Key != want.CacheLookupResponse.Entries[0].Key {
		t.Errorf("lookup entry key = %+v, want %+v", got.CacheLookupResponse.Entries[0].Key, want.CacheLookupResponse.Entries[0].Key)
	}
	if got.CachePutRequest.Entries[0].Key != want.CachePutRequest.Entries[0].Key {
		t.Errorf("put key = %+v, want %+v", got.CachePutRequest.Entries[0].Key, want.CachePutRequest.Entries[0].Key)
	}
	if !got.CachePutRequest.Entries[0].TilesetAt.Equal(want.CachePutRequest.Entries[0].TilesetAt) {
		t.Errorf("tileset_at = %v, want %v", got.CachePutRequest.Entries[0].TilesetAt, want.CachePutRequest.Entries[0].TilesetAt)
	}
	if got.JobFailed.Error != want.JobFailed.Error {
		t.Errorf("error = %q, want %q", got.JobFailed.Error, want.JobFailed.Error)
	}

	out, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("re-marshalling: %v", err)
	}
	var decodedWant, decodedGot any
	if err := json.Unmarshal(raw, &decodedWant); err != nil {
		t.Fatalf("unmarshalling the fixture: %v", err)
	}
	if err := json.Unmarshal(out, &decodedGot); err != nil {
		t.Fatalf("unmarshalling the re-encoded envelope: %v", err)
	}
	if !reflect.DeepEqual(decodedGot, decodedWant) {
		t.Errorf("a decode/encode round trip changed the envelope.\ngot  %s\nwant %s", out, raw)
	}
}

func TestWorkerStoreEnvelope_isShapeComplete(t *testing.T) {
	raw, err := json.Marshal(goldenEnvelope())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := jsonKeysOf(decoded)

	// Named because a missing departs_on is the SPA-269 failure mode: the
	// fixture still matches, every cache lookup misses, and there is no
	// error anywhere in the system to find it by.
	if _, ok := got["departs_on"]; !ok {
		t.Error("departs_on is missing from the fixture")
	}
	if _, ok := got["budget_mins"]; !ok {
		t.Error("budget_mins is missing from the fixture")
	}
	if _, ok := got["tileset_at"]; !ok {
		t.Error("tileset_at is missing from the fixture")
	}
	for _, tag := range []string{
		"cache_lookup_request", "cache_lookup_response", "cache_put_request",
		"job_succeeded", "job_failed", "keys", "entries", "key", "geometry",
		"compile_job_id", "station_slug", "mode", "contour_mins",
		"result", "error",
	} {
		if _, ok := got[tag]; !ok {
			t.Errorf("fixture is missing %q — an omitempty field with a zero value would slip through identically", tag)
		}
	}
}

func jsonKeysOf(v any) map[string]struct{} {
	seen := map[string]struct{}{}
	collectJSONKeys(v, seen)
	return seen
}

func collectJSONKeys(v any, seen map[string]struct{}) {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			seen[k] = struct{}{}
			collectJSONKeys(child, seen)
		}
	case []any:
		for _, child := range x {
			collectJSONKeys(child, seen)
		}
	}
}
