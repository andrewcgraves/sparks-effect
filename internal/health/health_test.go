package health

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

type fakeBroker struct{ connected bool }

func (b *fakeBroker) Connected() bool { return b.connected }

type fakeValhalla struct {
	err   error
	hang  bool
	calls int
}

func (v *fakeValhalla) Status(ctx context.Context) (valhalla.Status, error) {
	v.calls++
	if v.hang {
		<-ctx.Done()
		return valhalla.Status{}, ctx.Err()
	}
	return valhalla.Status{Version: "3.5.1"}, v.err
}

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newMonitor(stuckAfter time.Duration) (*Monitor, *fakeBroker, *fakeClock) {
	m, broker, _, clock := newMonitorWithValhalla(stuckAfter)
	return m, broker, clock
}

func newMonitorWithValhalla(stuckAfter time.Duration) (*Monitor, *fakeBroker, *fakeValhalla, *fakeClock) {
	broker := &fakeBroker{connected: true}
	routing := &fakeValhalla{}
	clock := &fakeClock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	return New(broker, routing, stuckAfter, clock.Now), broker, routing, clock
}

func probe(t *testing.T, m *Monitor) (int, string) {
	t.Helper()
	w := httptest.NewRecorder()
	m.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	return w.Code, w.Body.String()
}

func TestIdleConnectedWorkerIsHealthy(t *testing.T) {
	m, _, clock := newMonitor(10 * time.Minute)
	clock.Advance(time.Hour)

	if code, body := probe(t, m); code != http.StatusOK {
		t.Errorf("idle worker: status = %d (%s), want 200", code, body)
	}
}

func TestDroppedBrokerConnectionIsUnhealthy(t *testing.T) {
	m, broker, _ := newMonitor(10 * time.Minute)
	broker.connected = false

	code, body := probe(t, m)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if !strings.Contains(body, "broker") {
		t.Errorf("reason %q does not name the broker", body)
	}
}

func TestJobUnderThresholdIsHealthy(t *testing.T) {
	m, _, clock := newMonitor(10 * time.Minute)
	m.JobStarted()
	clock.Advance(10*time.Minute - time.Second)

	if code, body := probe(t, m); code != http.StatusOK {
		t.Errorf("job at 9m59s: status = %d (%s), want 200", code, body)
	}
}

func TestJobPastThresholdIsUnhealthy(t *testing.T) {
	m, _, clock := newMonitor(10 * time.Minute)
	m.JobStarted()
	clock.Advance(10 * time.Minute)

	code, body := probe(t, m)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("job at 10m: status = %d, want 503", code)
	}
	if !strings.Contains(body, "10m0s") {
		t.Errorf("reason %q does not say how long the job has run", body)
	}
}

func TestFinishedJobClearsTheStall(t *testing.T) {
	m, _, clock := newMonitor(10 * time.Minute)
	m.JobStarted()
	clock.Advance(time.Hour)
	m.JobFinished()

	if code, body := probe(t, m); code != http.StatusOK {
		t.Errorf("after the long job settled: status = %d (%s), want 200", code, body)
	}
}

func TestEachJobGetsItsOwnClock(t *testing.T) {
	m, _, clock := newMonitor(10 * time.Minute)
	m.JobStarted()
	clock.Advance(8 * time.Minute)
	m.JobFinished()
	m.JobStarted()
	clock.Advance(8 * time.Minute)

	if code, body := probe(t, m); code != http.StatusOK {
		t.Errorf("two 8m jobs back to back: status = %d (%s), want 200", code, body)
	}
}

func TestInProgressReportsTheCurrentJobsAge(t *testing.T) {
	m, _, clock := newMonitor(10 * time.Minute)
	if got := m.InProgress(); got != 0 {
		t.Errorf("idle InProgress = %v, want 0", got)
	}

	m.JobStarted()
	clock.Advance(90 * time.Second)
	if got := m.InProgress(); got != 90*time.Second {
		t.Errorf("InProgress = %v, want 1m30s", got)
	}

	m.JobFinished()
	if got := m.InProgress(); got != 0 {
		t.Errorf("after finish InProgress = %v, want 0", got)
	}
}

func TestNilMonitorIsANoOp(t *testing.T) {
	var m *Monitor
	m.JobStarted()
	m.JobFinished()
	if got := m.InProgress(); got != 0 {
		t.Errorf("nil InProgress = %v, want 0", got)
	}
}

func TestValhallaAnsweringIsHealthy(t *testing.T) {
	m, _, routing, _ := newMonitorWithValhalla(10 * time.Minute)

	if code, body := probe(t, m); code != http.StatusOK {
		t.Errorf("status = %d (%s), want 200", code, body)
	}
	if routing.calls != 1 {
		t.Errorf("Valhalla /status asked %d times, want 1", routing.calls)
	}
}

func TestValhallaErrorIsUnhealthy(t *testing.T) {
	m, _, routing, _ := newMonitorWithValhalla(10 * time.Minute)
	routing.err = errors.New("valhalla: upstream error: status 503: not ready")

	code, body := probe(t, m)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if !strings.Contains(body, "valhalla") || !strings.Contains(body, "not ready") {
		t.Errorf("reason %q does not name Valhalla and pass on its error", body)
	}
}

func TestValhallaTooSlowIsUnhealthy(t *testing.T) {
	m, _, routing, _ := newMonitorWithValhalla(10 * time.Minute)
	m.valhallaTimeout = 10 * time.Millisecond
	routing.hang = true

	code, body := probe(t, m)
	if code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", code)
	}
	if !strings.Contains(body, "did not answer") || !strings.Contains(body, "10ms") {
		t.Errorf("reason %q does not say Valhalla ran out of time", body)
	}
}

func TestRealValhallaNotReadyIsUnhealthy(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/status" {
			t.Errorf("probe asked Valhalla for %s, want /status", r.URL.Path)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	m := New(&fakeBroker{connected: true}, valhalla.NewHTTPClient(srv.URL), 10*time.Minute, time.Now)

	if code, body := probe(t, m); code != http.StatusServiceUnavailable {
		t.Errorf("Valhalla answering 503: status = %d (%s), want 503", code, body)
	}
}

func TestSyntheticClientPassesTheCheck(t *testing.T) {
	broker := &fakeBroker{connected: true}
	m := New(broker, valhalla.NewSyntheticClient(), 10*time.Minute, time.Now)

	if code, body := probe(t, m); code != http.StatusOK {
		t.Errorf("real synthetic client: status = %d (%s), want 200", code, body)
	}
}

func TestNoValhallaConfiguredSkipsTheCheck(t *testing.T) {
	m := New(&fakeBroker{connected: true}, nil, 10*time.Minute, time.Now)

	if code, body := probe(t, m); code != http.StatusOK {
		t.Errorf("nil Valhalla: status = %d (%s), want 200", code, body)
	}
}

func TestUnhealthyWorkerDoesNotAskValhalla(t *testing.T) {
	m, broker, routing, clock := newMonitorWithValhalla(10 * time.Minute)

	broker.connected = false
	probe(t, m)
	broker.connected = true
	m.JobStarted()
	clock.Advance(10 * time.Minute)
	probe(t, m)

	if routing.calls != 0 {
		t.Errorf("Valhalla /status asked %d times while the worker was already unhealthy, want 0", routing.calls)
	}
}
