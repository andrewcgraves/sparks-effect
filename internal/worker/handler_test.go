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
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/metrics"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/worker"
)

const goldenCompileJobID = "0f3b7c2a-4d1e-4a5b-9c8d-2e6f1a0b3c4d"

type fakeStore struct {
	mu           sync.Mutex
	states       []string
	result       json.RawMessage
	errMsg       string
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

func goldenMessage(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "routing", "testdata", "message.golden.json"))
	if err != nil {
		t.Fatalf("reading golden fixture: %v", err)
	}
	return b
}

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
	return worker.NewHandler(s, client, valhalla.DefaultLimits(), isochrone.EgressCache{}, logger.Discard(), nil)
}

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

func TestHandle_consultsTheEgressCacheForTheMessagesCompileJob(t *testing.T) {
	s := &fakeStore{}
	cache := &recordingCache{}
	h := worker.NewHandler(s, valhalla.NewSyntheticClient(), valhalla.DefaultLimits(),
		isochrone.EgressCache{Store: cache}, logger.Discard(), nil)

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

func TestHandle_missingRowIsPermanent(t *testing.T) {
	s := &fakeStore{runningErr: store.ErrJobNotFound}
	res := newHandler(s, valhalla.NewSyntheticClient()).Handle(context.Background(), goldenMessage(t))

	if res.Outcome != worker.OutcomePermanent {
		t.Errorf("Outcome = %v, want permanent", res.Outcome)
	}
}

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

const goldenTraceID = "3fa85f64-5717-4562-b3fc-2c963f66afa6"

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

func TestHandle_logsUnderTheTraceTheMessageCarries(t *testing.T) {
	var buf strings.Builder
	s := &fakeStore{}
	h := worker.NewHandler(s, valhalla.NewSyntheticClient(), valhalla.DefaultLimits(),
		isochrone.EgressCache{}, logger.New(&buf, false), nil)

	res := h.Handle(context.Background(), goldenMessage(t))
	if res.Outcome != worker.OutcomeSucceeded {
		t.Fatalf("Outcome = %v (err %v), want succeeded", res.Outcome, res.Err)
	}

	if res.TraceID != goldenTraceID {
		t.Errorf("res.TraceID = %q, want the fixture's %q", res.TraceID, goldenTraceID)
	}

	for i, id := range traceIDsIn(t, buf.String()) {
		if id != goldenTraceID {
			t.Errorf("log line %d has trace_id %q, want %q", i, id, goldenTraceID)
		}
	}
}

func TestHandle_logsARejectedMessageUnderItsTrace(t *testing.T) {
	var buf strings.Builder
	s := &fakeStore{}
	h := worker.NewHandler(s, valhalla.NewSyntheticClient(), valhalla.DefaultLimits(),
		isochrone.EgressCache{}, logger.New(&buf, false), nil)

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

func TestHandle_messageWithoutATraceKeepsTheHeadersTrace(t *testing.T) {
	var buf strings.Builder
	s := &fakeStore{}
	h := worker.NewHandler(s, valhalla.NewSyntheticClient(), valhalla.DefaultLimits(),
		isochrone.EgressCache{}, logger.New(&buf, false), nil)

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

func TestHandle_countsJobOutcomes(t *testing.T) {
	tests := []struct {
		name   string
		body   []byte
		client valhalla.Client
		want   string
	}{
		{"succeeded", goldenMessage(t), valhalla.NewSyntheticClient(), "succeeded"},
		{"permanent", mutateGolden(t, func(m map[string]any) { m["mode"] = "teleport" }), valhalla.NewSyntheticClient(), "permanent"},
		{"transient", goldenMessage(t), &valhalla.StubClient{IsochroneErr: valhalla.ErrUpstream}, "transient"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &metrics.Set{}
			h := worker.NewHandler(&fakeStore{}, tc.client, valhalla.DefaultLimits(), isochrone.EgressCache{}, logger.Discard(), m)
			res := h.Handle(context.Background(), tc.body)
			if res.Outcome.String() != tc.want {
				t.Fatalf("Outcome = %s, want %s (err %v)", res.Outcome, tc.want, res.Err)
			}
			if got := m.JobCount(tc.want); got != 1 {
				t.Errorf("%s count = %d, want 1", tc.want, got)
			}
		})
	}
}

func TestFail_doesNotCountAJob(t *testing.T) {
	m := &metrics.Set{}
	h := worker.NewHandler(&fakeStore{}, valhalla.NewSyntheticClient(), valhalla.DefaultLimits(),
		isochrone.EgressCache{}, logger.Discard(), m)
	h.Fail(context.Background(), "job-1", errors.New("exhausted 3 retries"))

	for _, outcome := range []string{"succeeded", "permanent", "transient"} {
		if got := m.JobCount(outcome); got != 0 {
			t.Errorf("JobsTotal{%s} = %d; Fail is not a settlement", outcome, got)
		}
	}
}
