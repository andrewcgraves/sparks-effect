package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

type Broker interface {
	Connected() bool
}

type Valhalla interface {
	Status(ctx context.Context) (valhalla.Status, error)
}

// Long enough for a busy Valhalla to answer /status, short enough that the
// API's poll of /healthz is not left waiting on a hung one.
const defaultValhallaTimeout = 2 * time.Second

type Monitor struct {
	broker          Broker
	valhalla        Valhalla
	valhallaTimeout time.Duration
	stuckAfter      time.Duration
	now             func() time.Time

	mu      sync.Mutex
	started time.Time
}

func New(broker Broker, routing Valhalla, stuckAfter time.Duration, now func() time.Time) *Monitor {
	return &Monitor{
		broker:          broker,
		valhalla:        routing,
		valhallaTimeout: defaultValhallaTimeout,
		stuckAfter:      stuckAfter,
		now:             now,
	}
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

func (m *Monitor) check(ctx context.Context) error {
	if !m.broker.Connected() {
		return fmt.Errorf("broker connection or channel is closed")
	}
	// Only the job in hand can be stuck. An idle worker with an open channel
	// is waiting on an empty queue, however long that lasts.
	if age := m.InProgress(); age >= m.stuckAfter {
		return fmt.Errorf("current job has been in progress for %s (stuck after %s)", age, m.stuckAfter)
	}
	// Valhalla goes last: it is the only check that costs a network call, and
	// there is no point paying for it once the worker is already unhealthy.
	return m.checkValhalla(ctx)
}

func (m *Monitor) checkValhalla(ctx context.Context) error {
	if m.valhalla == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, m.valhallaTimeout)
	defer cancel()

	_, err := m.valhalla.Status(ctx)
	switch {
	case err == nil:
		return nil
	// The synthetic client has no Valhalla behind it, so there is nothing to
	// ask and nothing that can be down.
	case errors.Is(err, valhalla.ErrNoStatus):
		return nil
	// The HTTP client flattens the context error into its message, so the
	// probe's own context is what says whether Valhalla ran out of time.
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("valhalla did not answer /status within %s", m.valhallaTimeout)
	default:
		return fmt.Errorf("valhalla /status failed: %v", err)
	}
}

func (m *Monitor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := m.check(r.Context()); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintln(w, err.Error())
		return
	}
	_, _ = fmt.Fprintln(w, "ok")
}
