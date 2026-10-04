package health

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

type Broker interface {
	Connected() bool
}

type Monitor struct {
	broker     Broker
	stuckAfter time.Duration
	now        func() time.Time

	mu      sync.Mutex
	started time.Time
}

func New(broker Broker, stuckAfter time.Duration, now func() time.Time) *Monitor {
	return &Monitor{broker: broker, stuckAfter: stuckAfter, now: now}
}

func (m *Monitor) JobStarted() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = m.now()
}

func (m *Monitor) JobFinished() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.started = time.Time{}
}

func (m *Monitor) InProgress() time.Duration {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started.IsZero() {
		return 0
	}
	return m.now().Sub(m.started)
}

func (m *Monitor) check() error {
	if !m.broker.Connected() {
		return fmt.Errorf("broker connection or channel is closed")
	}
	// Only the job in hand can be stuck. An idle worker with an open channel
	// is waiting on an empty queue, however long that lasts.
	if age := m.InProgress(); age >= m.stuckAfter {
		return fmt.Errorf("current job has been in progress for %s (stuck after %s)", age, m.stuckAfter)
	}
	return nil
}

func (m *Monitor) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := m.check(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintln(w, err.Error())
		return
	}
	_, _ = fmt.Fprintln(w, "ok")
}
