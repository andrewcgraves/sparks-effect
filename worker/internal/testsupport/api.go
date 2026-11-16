package testsupport

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
)

const (
	statusQueued    = "queued"
	statusRunning   = "running"
	statusSucceeded = "succeeded"
	statusFailed    = "failed"
)

const DefaultWorkerToken = "test-worker-token"

type FakeAPI struct {
	Server *httptest.Server
	Token  string
	mu     sync.Mutex
	jobs   map[string]*fakeJob
	cache  map[store.IsochroneKey]fakeCacheRow
	pings  int
}

type fakeJob struct {
	status        string
	errMsg        string
	result        []byte
	tilesetAt     time.Time
	reusableUntil time.Time
}

type fakeCacheRow struct {
	geometry  json.RawMessage
	tilesetAt time.Time
}

func StartFakeAPI(t *testing.T) *FakeAPI {
	t.Helper()
	f := &FakeAPI{
		Token: DefaultWorkerToken,
		jobs:  map[string]*fakeJob{},
		cache: map[store.IsochroneKey]fakeCacheRow{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/internal/worker", f.requireAuth(f.ready))
	mux.HandleFunc("POST /api/internal/routing-jobs/{id}/running", f.requireAuth(f.markRunning))
	mux.HandleFunc("POST /api/internal/routing-jobs/{id}/succeeded", f.requireAuth(f.markSucceeded))
	mux.HandleFunc("POST /api/internal/routing-jobs/{id}/failed", f.requireAuth(f.markFailed))
	mux.HandleFunc("POST /api/internal/isochrone-cache/lookup", f.requireAuth(f.cacheLookup))
	mux.HandleFunc("POST /api/internal/isochrone-cache", f.requireAuth(f.cachePut))
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

func (f *FakeAPI) URL() string { return f.Server.URL }

func (f *FakeAPI) SeedRoutingJob(jobID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[jobID] = &fakeJob{status: statusQueued}
}

func (f *FakeAPI) JobStatus(t *testing.T, jobID string) (status, errMessage string, result []byte) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[jobID]
	if !ok {
		t.Fatalf("reading routing job %s: not found", jobID)
	}
	return j.status, j.errMsg, append([]byte(nil), j.result...)
}

func (f *FakeAPI) JobReuseClocks(t *testing.T, jobID string) (tilesetAt, reusableUntil time.Time) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[jobID]
	if !ok {
		t.Fatalf("reading routing job %s: not found", jobID)
	}
	return j.tilesetAt, j.reusableUntil
}

func (f *FakeAPI) IsochroneCacheCount(compileJobID string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for k := range f.cache {
		if k.CompileJobID == compileJobID {
			n++
		}
	}
	return n
}

func (f *FakeAPI) IsochroneCacheStamps(compileJobID string) []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	var stamps []time.Time
	for k, row := range f.cache {
		if k.CompileJobID == compileJobID {
			stamps = append(stamps, row.tilesetAt)
		}
	}
	return stamps
}

func (f *FakeAPI) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		const prefix = "bearer "
		header := r.Header.Get("Authorization")
		if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			return
		}
		token := strings.TrimSpace(header[len(prefix):])
		if token == "" || token != f.Token {
			http.Error(w, `{"error":"authentication required"}`, http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (f *FakeAPI) ready(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	f.pings++
	f.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (f *FakeAPI) Pings() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pings
}

func (f *FakeAPI) markRunning(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok || j.status == statusSucceeded || j.status == statusFailed {
		http.Error(w, `{"error":"routing job not found"}`, http.StatusNotFound)
		return
	}
	j.status = statusRunning
	w.WriteHeader(http.StatusNoContent)
}

func (f *FakeAPI) markSucceeded(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Result        json.RawMessage `json:"result"`
		TilesetAt     time.Time       `json:"tileset_at"`
		ReusableUntil time.Time       `json:"reusable_until"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		http.Error(w, `{"error":"routing job not found"}`, http.StatusNotFound)
		return
	}
	j.status = statusSucceeded
	j.result = append([]byte(nil), body.Result...)
	j.tilesetAt, j.reusableUntil = body.TilesetAt, body.ReusableUntil
	j.errMsg = ""
	w.WriteHeader(http.StatusNoContent)
}

func (f *FakeAPI) markFailed(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Error string `json:"error"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	j, ok := f.jobs[id]
	if !ok {
		http.Error(w, `{"error":"routing job not found"}`, http.StatusNotFound)
		return
	}
	j.status = statusFailed
	j.errMsg = body.Error
	w.WriteHeader(http.StatusNoContent)
}

func (f *FakeAPI) cacheLookup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Keys []store.IsochroneKey `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	type entry struct {
		Key       store.IsochroneKey `json:"key"`
		Geometry  json.RawMessage    `json:"geometry"`
		TilesetAt time.Time          `json:"tileset_at,omitzero"`
	}
	out := struct {
		Entries []entry `json:"entries"`
	}{Entries: []entry{}}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range body.Keys {
		if row, ok := f.cache[k]; ok {
			out.Entries = append(out.Entries, entry{Key: k, Geometry: row.geometry, TilesetAt: row.tilesetAt})
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (f *FakeAPI) cachePut(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Entries []store.CachedIsochrone `json:"entries"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, `{"error":"invalid body"}`, http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	// The tileset half of the API's write guard (SPA-328): a conflicting row
	// is replaced only by a stamped one newer than it, so a recompute after a
	// map cycle lands and a stale or synthetic worker cannot overwrite.
	for _, e := range body.Entries {
		if stored, exists := f.cache[e.Key]; exists {
			if e.TilesetAt.IsZero() || (!stored.tilesetAt.IsZero() && !e.TilesetAt.After(stored.tilesetAt)) {
				continue
			}
		}
		f.cache[e.Key] = fakeCacheRow{geometry: append(json.RawMessage(nil), e.Geometry...), tilesetAt: e.TilesetAt}
	}
	w.WriteHeader(http.StatusNoContent)
}
