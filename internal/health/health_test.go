package health

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeBroker struct{ connected bool }

func (b *fakeBroker) Connected() bool { return b.connected }

type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newMonitor(stuckAfter time.Duration) (*Monitor, *fakeBroker, *fakeClock) {
	broker := &fakeBroker{connected: true}
	clock := &fakeClock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	return New(broker, stuckAfter, clock.Now), broker, clock
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
