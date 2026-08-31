package worker_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/isochrone"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/worker"
)

// goldenCompileJobID is the compile job the golden message names — the identity
// every cache key from that message has to carry.
const goldenCompileJobID = "0f3b7c2a-4d1e-4a5b-9c8d-2e6f1a0b3c4d"

// fakeStore records the transitions a job was driven through, in order.
//
// Order is the point: "queued → running → succeeded" is an acceptance
// criterion, and a store that only remembered the final status would pass
// whether or not the job was ever marked running.
type fakeStore struct {
	mu     sync.Mutex
	states []string
	result json.RawMessage
	errMsg string

	runningErr   error
	succeededErr error
	failedErr    error
}

func (f *fakeStore) MarkRunning(_ context.Context, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.runningErr != nil {
		return f.runningErr
	}
	f.states = append(f.states, store.StatusRunning)
	return nil
}

func (f *fakeStore) MarkSucceeded(_ context.Context, _ string, result json.RawMessage) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.succeededErr != nil {
		return f.succeededErr
	}
	f.states = append(f.states, store.StatusSucceeded)
	f.result = result
	return nil
}

func (f *fakeStore) MarkFailed(_ context.Context, _ string, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failedErr != nil {
		return f.failedErr
	}
	f.states = append(f.states, store.StatusFailed)
	f.errMsg = message
	return nil
}

func (f *fakeStore) snapshot() ([]string, json.RawMessage, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.states...), f.result, f.errMsg
}

// goldenMessage is the fixture sparks-effect-api asserts it publishes, read
// from the routing package's testdata so there is exactly one copy of it in the
// repository.
func goldenMessage(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "routing", "testdata", "message.golden.json"))
	if err != nil {
		t.Fatalf("reading golden fixture: %v", err)
	}
	return b
}

// mutateGolden returns the golden message with one field changed, so a case
// only ever exercises the thing it names.
func mutateGolden(t *testing.T, mut func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(goldenMessage(t), &m); err != nil {
		t.Fatalf("unmarshalling golden fixture: %v", err)
	}
	mut(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-marshalling: %v", err)
	}
	return b
}

func newHandler(s store.RoutingJobStore, client valhalla.Client) *worker.Handler {
	return worker.NewHandler(s, client, valhalla.DefaultLimits(), isochrone.EgressCache{}, logger.Discard())
}

// recordingCache is the thinnest thing that answers as a cache: it holds
// nothing and remembers what it was asked. Whether reuse *works* is settled in
// internal/isochrone against a real one; what is in question here is only
// whether the handler hands its cache down to the chain it builds.
type recordingCache struct {
	mu   sync.Mutex
	gets []store.IsochroneKey
}

var _ store.IsochroneCache = (*recordingCache)(nil)

func (r *recordingCache) Get(_ context.Context, keys []store.IsochroneKey) (map[store.IsochroneKey]json.RawMessage, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.gets = append(r.gets, keys...)
	return nil, nil
}

func (r *recordingCache) Put(context.Context, []store.CachedIsochrone) error { return nil }

func (r *recordingCache) asked() []store.IsochroneKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]store.IsochroneKey(nil), r.gets...)
}

// The cache is configured on the handler but consulted by the chainer it builds
// per message, so the two are only connected if the handler passes it through.
func TestHandle_consultsTheEgressCacheForTheMessagesCompileJob(t *testing.T) {
	s := &fakeStore{}
	cache := &recordingCache{}
	h := worker.NewHandler(s, valhalla.NewSyntheticClient(), valhalla.DefaultLimits(),
		isochrone.EgressCache{Store: cache}, logger.Discard())

	// The golden fixture's 45-minute budget only reaches "north" on foot —
	// exactly the walk-only case SPA-188 stops caching. Widened here so
	// "south" is reached by riding svc-express from north, which is what
	// this test needs the cache to be asked about.
	msg := mutateGolden(t, func(m map[string]any) { m["budget_mins"] = 90 })

	if res := h.Handle(context.Background(), msg); res.Outcome != worker.OutcomeSucceeded {
		t.Fatalf("Outcome = %v (err %v), want succeeded", res.Outcome, res.Err)
	}

	asked := cache.asked()
	if len(asked) == 0 {
		t.Fatal("the cache was never consulted; the handler is not passing it to the chainer")
	}
	for _, k := range asked {
		if k.CompileJobID != goldenCompileJobID {
			t.Errorf("looked up compile job %q, want the message's %q", k.CompileJobID, goldenCompileJobID)
		}
		if k.Mode != "walk" {
			t.Errorf("looked up mode %q, want the message's walk", k.Mode)
		}
	}
}

// The whole point of the worker: a message the API published becomes a
// succeeded row with a chained result attached, having passed through running
// on the way.
func TestHandle_drivesJobToSucceeded(t *testing.T) {
	s := &fakeStore{}
	h := newHandler(s, valhalla.NewSyntheticClient())

	res := h.Handle(context.Background(), goldenMessage(t))
	if res.Outcome != worker.OutcomeSucceeded {
		t.Fatalf("Outcome = %v (err %v), want succeeded", res.Outcome, res.Err)
	}
	if res.JobID != "6f9619ff-8b86-d011-b42d-00c04fc964ff" {
		t.Errorf("JobID = %q, want the golden fixture's", res.JobID)
	}

	states, result, _ := s.snapshot()
	want := []string{store.StatusRunning, store.StatusSucceeded}
	if len(states) != len(want) {
		t.Fatalf("states = %v, want %v", states, want)
	}
	for i := range want {
		if states[i] != want[i] {
			t.Fatalf("states = %v, want %v", states, want)
		}
	}

	// The result has to be a FeatureCollection the API can hand back untouched.
	var parsed struct {
		Type     string            `json:"type"`
		Features []json.RawMessage `json:"features"`
		Metadata struct {
			CompileJobID string `json:"compile_job_id"`
			Mode         string `json:"mode"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if parsed.Type != "FeatureCollection" {
		t.Errorf("result type = %q, want FeatureCollection", parsed.Type)
	}
	if len(parsed.Features) == 0 {
		t.Error("result carries no features")
	}
	if parsed.Metadata.CompileJobID != "0f3b7c2a-4d1e-4a5b-9c8d-2e6f1a0b3c4d" {
		t.Errorf("metadata compile_job_id = %q, want the fixture's", parsed.Metadata.CompileJobID)
	}
	if parsed.Metadata.Mode != "walk" {
		t.Errorf("metadata mode = %q, want walk", parsed.Metadata.Mode)
	}
}

// A permanent failure marks the job failed and does not ask to be retried.
// Requeueing any of these would loop forever against a server with room for one
// chain at a time.
func TestHandle_permanentFailuresFailTheJobWithoutRetry(t *testing.T) {
	tests := []struct {
		name   string
		body   []byte
		client valhalla.Client
	}{
		{
			name: "unknown mode",
			body: mutateGolden(t, func(m map[string]any) { m["mode"] = "teleport" }),
		},
		{
			name: "future schema version",
			body: mutateGolden(t, func(m map[string]any) { m["schema_version"] = 99 }),
		},
		{
			name: "no graph",
			body: mutateGolden(t, func(m map[string]any) { m["graph"] = nil }),
		},
		{
			name: "zero budget",
			body: mutateGolden(t, func(m map[string]any) { m["budget_mins"] = 0 }),
		},
		{
			name:   "valhalla rejects the request",
			body:   goldenMessage(t),
			client: &valhalla.StubClient{IsochroneErr: valhalla.ErrBadRequest},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := tc.client
			if client == nil {
				client = valhalla.NewSyntheticClient()
			}
			s := &fakeStore{}
			h := newHandler(s, client)

			res := h.Handle(context.Background(), tc.body)
			if res.Outcome != worker.OutcomePermanent {
				t.Errorf("Outcome = %v, want permanent (err %v)", res.Outcome, res.Err)
			}

			states, _, errMsg := s.snapshot()
			if len(states) == 0 || states[len(states)-1] != store.StatusFailed {
				t.Errorf("states = %v, want to end at failed", states)
			}
			if errMsg == "" {
				t.Error("job was failed with no message; a caller has nothing to read")
			}
		})
	}
}

// A transient failure asks to be retried and deliberately does NOT mark the job
// failed: another attempt is still to come, and a client polling the row would
// otherwise be told it was over while the worker was still working on it.
func TestHandle_transientFailureRequeuesWithoutFailingTheJob(t *testing.T) {
	tests := []struct {
		name   string
		client valhalla.Client
	}{
		{"valhalla unreachable", &valhalla.StubClient{IsochroneErr: valhalla.ErrUpstream}},
		{"valhalla rate limited", &valhalla.StubClient{IsochroneErr: valhalla.ErrRateLimit}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &fakeStore{}
			h := newHandler(s, tc.client)

			res := h.Handle(context.Background(), goldenMessage(t))
			if res.Outcome != worker.OutcomeTransient {
				t.Errorf("Outcome = %v, want transient (err %v)", res.Outcome, res.Err)
			}

			states, _, _ := s.snapshot()
			for _, st := range states {
				if st == store.StatusFailed {
					t.Error("job was marked failed on a transient error; retries are still to come")
				}
			}
		})
	}
}

// A database that cannot be written to is transient, not permanent: the chain
// itself was fine and the answer is worth another attempt.
func TestHandle_storeFailuresAreTransient(t *testing.T) {
	dbDown := errors.New("connection refused")

	t.Run("cannot mark running", func(t *testing.T) {
		s := &fakeStore{runningErr: dbDown}
		res := newHandler(s, valhalla.NewSyntheticClient()).Handle(context.Background(), goldenMessage(t))
		if res.Outcome != worker.OutcomeTransient {
			t.Errorf("Outcome = %v, want transient", res.Outcome)
		}
	})

	t.Run("cannot record the result", func(t *testing.T) {
		s := &fakeStore{succeededErr: dbDown}
		res := newHandler(s, valhalla.NewSyntheticClient()).Handle(context.Background(), goldenMessage(t))
		if res.Outcome != worker.OutcomeTransient {
			t.Errorf("Outcome = %v, want transient — the chain succeeded, only the write failed", res.Outcome)
		}
	})
}

// A job whose row has gone — its compile job cascaded away — is permanent.
// There is nothing left to write an answer to, so retrying cannot help.
func TestHandle_missingRowIsPermanent(t *testing.T) {
	s := &fakeStore{runningErr: store.ErrJobNotFound}
	res := newHandler(s, valhalla.NewSyntheticClient()).Handle(context.Background(), goldenMessage(t))

	if res.Outcome != worker.OutcomePermanent {
		t.Errorf("Outcome = %v, want permanent", res.Outcome)
	}
}

// A message too malformed to name a job can only be discarded: there is no row
// to record the failure against. It must still be acked, or it redelivers
// forever.
func TestHandle_unreadableMessageIsDiscarded(t *testing.T) {
	s := &fakeStore{}
	res := newHandler(s, valhalla.NewSyntheticClient()).Handle(context.Background(), []byte("not json"))

	if res.Outcome != worker.OutcomePermanent {
		t.Errorf("Outcome = %v, want permanent", res.Outcome)
	}
	if res.JobID != "" {
		t.Errorf("JobID = %q, want empty — the message named no job", res.JobID)
	}
	if states, _, _ := s.snapshot(); len(states) != 0 {
		t.Errorf("states = %v, want none — there is no row to write to", states)
	}
}

// goldenTraceID is the trace the golden message carries.
const goldenTraceID = "3fa85f64-5717-4562-b3fc-2c963f66afa6"

// traceIDsIn reports the trace_id on every line the handler logged, using "" for
// a line that carried none, so a test can assert the whole trail rather than
// whichever line happened to be first.
func traceIDsIn(t *testing.T, out string) []string {
	t.Helper()
	out = strings.TrimSpace(out)
	if out == "" {
		t.Fatal("the handler logged nothing; there is no trail to check")
	}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("log line is not valid JSON: %v\nline: %s", err, line)
		}
		id, _ := parsed["trace_id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// The trace lives in the message body, and sparks-effect-api sets no
// x-trace-id header at all, so a handler that only read the header logged every
// job untraced and cross-service correlation did not work (SPA-227).
func TestHandle_logsUnderTheTraceTheMessageCarries(t *testing.T) {
	var buf strings.Builder
	s := &fakeStore{}
	h := worker.NewHandler(s, valhalla.NewSyntheticClient(), valhalla.DefaultLimits(),
		isochrone.EgressCache{}, logger.New(&buf, false))

	res := h.Handle(context.Background(), goldenMessage(t))
	if res.Outcome != worker.OutcomeSucceeded {
		t.Fatalf("Outcome = %v (err %v), want succeeded", res.Outcome, res.Err)
	}
	// The consumer reads it back off the Result to log the delivery's
	// settlement under the same trace.
	if res.TraceID != goldenTraceID {
		t.Errorf("res.TraceID = %q, want the fixture's %q", res.TraceID, goldenTraceID)
	}

	for i, id := range traceIDsIn(t, buf.String()) {
		if id != goldenTraceID {
			t.Errorf("log line %d has trace_id %q, want %q", i, id, goldenTraceID)
		}
	}
}

// A message rejected out of hand is exactly when the trace is most worth
// having: the API recorded a request, and this is the only place that says what
// became of it.
func TestHandle_logsARejectedMessageUnderItsTrace(t *testing.T) {
	var buf strings.Builder
	s := &fakeStore{}
	h := worker.NewHandler(s, valhalla.NewSyntheticClient(), valhalla.DefaultLimits(),
		isochrone.EgressCache{}, logger.New(&buf, false))

	body := mutateGolden(t, func(m map[string]any) { m["mode"] = "teleport" })
	if res := h.Handle(context.Background(), body); res.TraceID != goldenTraceID {
		t.Errorf("res.TraceID = %q, want the fixture's %q", res.TraceID, goldenTraceID)
	}

	for i, id := range traceIDsIn(t, buf.String()) {
		if id != goldenTraceID {
			t.Errorf("log line %d has trace_id %q, want %q", i, id, goldenTraceID)
		}
	}
}

// A message with no trace of its own does not erase the one the delivery header
// gave the context, and does not log an empty trace_id in its place.
func TestHandle_messageWithoutATraceKeepsTheHeadersTrace(t *testing.T) {
	var buf strings.Builder
	s := &fakeStore{}
	h := worker.NewHandler(s, valhalla.NewSyntheticClient(), valhalla.DefaultLimits(),
		isochrone.EgressCache{}, logger.New(&buf, false))

	body := mutateGolden(t, func(m map[string]any) { delete(m, "trace_id") })
	ctx := logger.WithTraceID(context.Background(), "from-the-header")

	res := h.Handle(ctx, body)
	if res.Outcome != worker.OutcomeSucceeded {
		t.Fatalf("Outcome = %v (err %v), want succeeded", res.Outcome, res.Err)
	}
	if res.TraceID != "" {
		t.Errorf("res.TraceID = %q, want empty — the message carried no trace", res.TraceID)
	}

	for i, id := range traceIDsIn(t, buf.String()) {
		if id != "from-the-header" {
			t.Errorf("log line %d has trace_id %q, want the header's", i, id)
		}
	}
}

// Fail is what the consumer calls once retries are exhausted, and it has to
// leave the row saying failed with the reason attached — that is what stops the
// client polling.
func TestFail_marksTheJobFailedWithItsCause(t *testing.T) {
	s := &fakeStore{}
	h := newHandler(s, valhalla.NewSyntheticClient())

	h.Fail(context.Background(), "job-1", errors.New("exhausted 3 retries: valhalla unreachable"))

	states, _, errMsg := s.snapshot()
	if len(states) != 1 || states[0] != store.StatusFailed {
		t.Fatalf("states = %v, want [failed]", states)
	}
	if !strings.Contains(errMsg, "exhausted 3 retries") {
		t.Errorf("error message = %q, want it to carry the cause", errMsg)
	}
}

// With no Valhalla configured the synthetic client serves the loop, and the
// polygons it produces say so — a shape that reaches a map without a real
// server behind it must be identifiable as such.
func TestHandle_syntheticClientMarksItsOutput(t *testing.T) {
	s := &fakeStore{}
	h := newHandler(s, valhalla.NewSyntheticClient())

	if res := h.Handle(context.Background(), goldenMessage(t)); res.Outcome != worker.OutcomeSucceeded {
		t.Fatalf("Outcome = %v, want succeeded (err %v)", res.Outcome, res.Err)
	}

	_, result, _ := s.snapshot()
	var parsed struct {
		Features []struct {
			Properties map[string]any `json:"properties"`
		} `json:"features"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if len(parsed.Features) == 0 {
		t.Fatal("result carries no features")
	}
	for i, f := range parsed.Features {
		if f.Properties["synthetic"] != true {
			t.Errorf("feature %d is not marked synthetic: %v", i, f.Properties)
		}
	}
}
