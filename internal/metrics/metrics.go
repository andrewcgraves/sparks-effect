package metrics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

const (
	ResultHit  = "hit"
	ResultMiss = "miss"

	ClassBadRequest = "bad_request"
	ClassRateLimit  = "rate_limit"
	ClassUpstream   = "upstream"
)

var (
	jobOutcomes       = []string{"succeeded", "permanent", "transient"}
	cacheResults      = []string{ResultHit, ResultMiss}
	valhallaClasses   = []string{ClassBadRequest, ClassRateLimit, ClassUpstream}
	transitAccessRode = []string{"true", "false"}
)

var Process = &Set{}

type Set struct {
	jobs          [3]atomic.Uint64
	cache         [2]atomic.Uint64
	deadLetters   atomic.Uint64
	valhalla      [3]atomic.Uint64
	transitAccess [2]atomic.Uint64
	inProgress    atomic.Pointer[func() time.Duration]
}

func (m *Set) Job(outcome string) {
	if m == nil {
		return
	}
	if i, ok := index(outcome, jobOutcomes); ok {
		m.jobs[i].Add(1)
	}
}

func (m *Set) Cache(result string, n int) {
	if m == nil || n <= 0 {
		return
	}
	if i, ok := index(result, cacheResults); ok {
		m.cache[i].Add(uint64(n))
	}
}

func (m *Set) DeadLetter() {
	if m == nil {
		return
	}
	m.deadLetters.Add(1)
}

func (m *Set) TransitAccess(rode bool, n int) {
	if m == nil || n <= 0 {
		return
	}
	if i, ok := index(rodeLabel(rode), transitAccessRode); ok {
		m.transitAccess[i].Add(uint64(n))
	}
}

func (m *Set) ObserveJobInProgress(f func() time.Duration) {
	if m == nil {
		return
	}
	m.inProgress.Store(&f)
}

func (m *Set) jobInProgressSeconds() float64 {
	f := m.inProgress.Load()
	if f == nil {
		return 0
	}
	return (*f)().Seconds()
}

func (m *Set) JobCount(outcome string) uint64 {
	if m == nil {
		return 0
	}
	i, ok := index(outcome, jobOutcomes)
	if !ok {
		return 0
	}
	return m.jobs[i].Load()
}

func (m *Set) CacheCount(result string) uint64 {
	if m == nil {
		return 0
	}
	i, ok := index(result, cacheResults)
	if !ok {
		return 0
	}
	return m.cache[i].Load()
}

func (m *Set) DeadLetterCount() uint64 {
	if m == nil {
		return 0
	}
	return m.deadLetters.Load()
}

func (m *Set) ValhallaErrorCount(class string) uint64 {
	if m == nil {
		return 0
	}
	i, ok := index(class, valhallaClasses)
	if !ok {
		return 0
	}
	return m.valhalla[i].Load()
}

func (m *Set) TransitAccessCount(rode bool) uint64 {
	if m == nil {
		return 0
	}
	i, ok := index(rodeLabel(rode), transitAccessRode)
	if !ok {
		return 0
	}
	return m.transitAccess[i].Load()
}

func (m *Set) observeValhalla(err error) {
	if m == nil || err == nil {
		return
	}
	var class string
	switch {
	case errors.Is(err, valhalla.ErrBadRequest):
		class = ClassBadRequest
	case errors.Is(err, valhalla.ErrRateLimit):
		class = ClassRateLimit
	case errors.Is(err, valhalla.ErrUpstream):
		class = ClassUpstream
	default:
		return
	}
	if i, ok := index(class, valhallaClasses); ok {
		m.valhalla[i].Add(1)
	}
}

func Handler() http.Handler {
	return Process.Handler()
}

func (m *Set) Handler() http.Handler {
	if m == nil {
		m = Process
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/metrics", m.writeMetrics)
	return mux
}

func (m *Set) writeMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writeFamily(w, "routing_jobs_total", "Routing jobs settled by outcome.", "outcome", jobOutcomes, m.jobs[:])
	writeFamily(w, "isochrone_cache_total", "Egress isochrone cache lookups by result.", "result", cacheResults, m.cache[:])
	write(w, "# HELP routing_dead_letters_total Jobs moved to the dead-letter queue after retries ran out.\n")
	write(w, "# TYPE routing_dead_letters_total counter\n")
	write(w, "routing_dead_letters_total %d\n", m.deadLetters.Load())
	writeFamily(w, "valhalla_errors_total", "Valhalla errors by class.", "class", valhallaClasses, m.valhalla[:])
	writeFamily(w, "transit_access_legs_total", "Successfully routed transit-mode pairwise access legs by whether Valhalla actually rode transit (vs walked the whole way).", "rode", transitAccessRode, m.transitAccess[:])
	write(w, "# HELP routing_job_in_progress_seconds How long the job in hand has been running; 0 when idle.\n")
	write(w, "# TYPE routing_job_in_progress_seconds gauge\n")
	write(w, "routing_job_in_progress_seconds %g\n", m.jobInProgressSeconds())
}

func writeFamily(w io.Writer, name, help, label string, values []string, counters []atomic.Uint64) {
	write(w, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
	for i, value := range values {
		write(w, "%s{%s=%q} %d\n", name, label, value, counters[i].Load())
	}
}

func write(w io.Writer, format string, args ...any) {
	_, _ = fmt.Fprintf(w, format, args...)
}

func index(value string, values []string) (int, bool) {
	for i, candidate := range values {
		if value == candidate {
			return i, true
		}
	}
	return 0, false
}

func rodeLabel(rode bool) string {
	if rode {
		return "true"
	}
	return "false"
}

type valhallaClient struct {
	valhalla.Client
	metrics *Set
}

func ObserveValhalla(client valhalla.Client) valhalla.Client {
	return Process.ObserveValhalla(client)
}

func (m *Set) ObserveValhalla(client valhalla.Client) valhalla.Client {
	if m == nil {
		return client
	}
	return &valhallaClient{Client: client, metrics: m}
}

func (c *valhallaClient) Isochrone(ctx context.Context, req valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
	resp, err := c.Client.Isochrone(ctx, req)
	c.metrics.observeValhalla(err)
	return resp, err
}

func (c *valhallaClient) Matrix(ctx context.Context, req valhalla.MatrixRequest) (*valhalla.MatrixResponse, error) {
	resp, err := c.Client.Matrix(ctx, req)
	c.metrics.observeValhalla(err)
	return resp, err
}

func (c *valhallaClient) Route(ctx context.Context, req valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
	resp, err := c.Client.Route(ctx, req)
	c.metrics.observeValhalla(err)
	return resp, err
}

func Serve(ctx context.Context, addr string, handler http.Handler) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("metrics: listen %s: %w", addr, err)
	}
	return serve(ctx, ln, handler)
}

func serve(ctx context.Context, ln net.Listener, handler http.Handler) error {
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		err := <-errCh
		if err == nil || errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("metrics: %w", err)
	}
}
