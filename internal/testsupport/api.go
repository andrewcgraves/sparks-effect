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

// DefaultWorkerToken is the bearer token the fake API accepts. Tests that
// construct a store.Client against one should send this, not a made-up value,
// so a mistyped credential fails the same way a real one would.
const DefaultWorkerToken = "test-worker-token"

// FakeAPI is an in-process stand-in for the sparks-effect-api worker endpoints.
//
// It exists so the consumer integration tests can still run the whole loop —
// a real broker, a real HTTP client, a job row driven queued → running →
// succeeded — without a Postgres of this worker's own. The SQL those writes
// used to hit now lives in the API; what this fake has to get right is the
// HTTP contract the client speaks, including the 404 that means ErrJobNotFound
// and the auth gate that rejects anything but the shared token.
type FakeAPI struct {
	Server *httptest.Server
	Token  string

	mu    sync.Mutex
	jobs  map[string]*fakeJob
	cache map[store.IsochroneKey]fakeCacheRow
}

type fakeJob struct {
	status string
	errMsg string
	result []byte
}

type fakeCacheRow struct {
	geometry  json.RawMessage
	tilesetAt time.Time
}

// StartFakeAPI starts a server implementing the worker HTTP contract and
// registers cleanup to close it.
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

// URL is the base URL a store.Client should be pointed at.
func (f *FakeAPI) URL() string { return f.Server.URL }

// SeedRoutingJob inserts a queued routing job, mirroring what the API does
// before it publishes.
func (f *FakeAPI) SeedRoutingJob(jobID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jobs[jobID] = &fakeJob{status: statusQueued}
}

// JobStatus reads back a job's status, error and result.
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

// IsochroneCacheCount counts the rows held for one compile job.
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

// IsochroneCacheStamps returns the tileset stamp on every row held for one
// compile job, zero times included.
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
	w.WriteHeader(http.StatusNoContent)
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
		Result json.RawMessage `json:"result"`
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
		Key      store.IsochroneKey `json:"key"`
		Geometry json.RawMessage    `json:"geometry"`
	}
	out := struct {
		Entries []entry `json:"entries"`
	}{Entries: []entry{}}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, k := range body.Keys {
		if row, ok := f.cache[k]; ok {
			out.Entries = append(out.Entries, entry{Key: k, Geometry: row.geometry})
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
	for _, e := range body.Entries {
		if _, exists := f.cache[e.Key]; exists {
			continue
		}
		f.cache[e.Key] = fakeCacheRow{geometry: append(json.RawMessage(nil), e.Geometry...), tilesetAt: e.TilesetAt}
	}
	w.WriteHeader(http.StatusNoContent)
}
