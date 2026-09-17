package metrics

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

func TestHandlerExposesZeroSeries(t *testing.T) {
	m := &Set{}
	body := scrape(t, m)

	for _, name := range []string{
		"routing_jobs_total",
		"isochrone_cache_total",
		"routing_dead_letters_total",
		"valhalla_errors_total",
	} {
		if !strings.Contains(body, name) {
			t.Errorf("/metrics is missing %s:\n%s", name, body)
		}
	}

	for _, sample := range []string{
		`routing_jobs_total{outcome="succeeded"} 0`,
		`routing_jobs_total{outcome="permanent"} 0`,
		`routing_jobs_total{outcome="transient"} 0`,
		`isochrone_cache_total{result="hit"} 0`,
		`isochrone_cache_total{result="miss"} 0`,
		`valhalla_errors_total{class="bad_request"} 0`,
		`valhalla_errors_total{class="rate_limit"} 0`,
		`valhalla_errors_total{class="upstream"} 0`,
		`routing_dead_letters_total 0`,
	} {
		if !strings.Contains(body, sample) {
			t.Errorf("/metrics is missing zero series %s:\n%s", sample, body)
		}
	}
}

func TestCountersIncrement(t *testing.T) {
	m := &Set{}
	m.Job("succeeded")
	m.Cache(ResultHit, 3)
	m.DeadLetter()

	body := scrape(t, m)
	for _, want := range []string{
		`routing_jobs_total{outcome="succeeded"} 1`,
		`routing_jobs_total{outcome="permanent"} 0`,
		`isochrone_cache_total{result="hit"} 3`,
		`isochrone_cache_total{result="miss"} 0`,
		`routing_dead_letters_total 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics response missing %q:\n%s", want, body)
		}
	}
}

func TestCacheIgnoresNonPositiveCounts(t *testing.T) {
	m := &Set{}
	m.Cache(ResultMiss, 0)
	m.Cache(ResultMiss, -1)
	if got := m.CacheCount(ResultMiss); got != 0 {
		t.Errorf("miss counter = %d, want 0", got)
	}
}

func TestUnknownLabelsAreIgnored(t *testing.T) {
	m := &Set{}
	m.Job("mystery")
	m.Cache("stale", 4)
	body := scrape(t, m)
	if strings.Contains(body, "mystery") || strings.Contains(body, "stale") {
		t.Errorf("unknown labels leaked into /metrics:\n%s", body)
	}
}

func TestObserveValhallaClassifiesErrors(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		class string
	}{
		{"bad_request", valhalla.ErrBadRequest, ClassBadRequest},
		{"rate_limit", valhalla.ErrRateLimit, ClassRateLimit},
		{"upstream", valhalla.ErrUpstream, ClassUpstream},
		{"wrapped rate limit", errors.Join(valhalla.ErrRateLimit, errors.New("429")), ClassRateLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Set{}
			client := m.ObserveValhalla(&valhalla.StubClient{IsochroneErr: tc.err})
			_, _ = client.Isochrone(context.Background(), valhalla.IsochroneRequest{})
			if got := m.ValhallaErrorCount(tc.class); got != 1 {
				t.Errorf("%s count = %d, want 1", tc.class, got)
			}
		})
	}
}

func TestObserveValhallaIgnoresUnclassifiedErrors(t *testing.T) {
	m := &Set{}
	client := m.ObserveValhalla(&valhalla.StubClient{IsochroneErr: errors.New("context canceled")})
	_, _ = client.Isochrone(context.Background(), valhalla.IsochroneRequest{})
	for _, class := range []string{ClassBadRequest, ClassRateLimit, ClassUpstream} {
		if got := m.ValhallaErrorCount(class); got != 0 {
			t.Errorf("%s = %d; unclassified errors are not Valhalla classes", class, got)
		}
	}
}

func TestObserveValhallaDoesNotCountStatusErrors(t *testing.T) {
	m := &Set{}
	client := m.ObserveValhalla(&valhalla.StubClient{
		IsochroneErr: valhalla.ErrUpstream,
		StatusErr:    valhalla.ErrUpstream,
	})

	_, _ = client.Status(context.Background())
	if got := m.ValhallaErrorCount(ClassUpstream); got != 0 {
		t.Errorf("upstream count after Status = %d, want 0", got)
	}

	_, _ = client.Isochrone(context.Background(), valhalla.IsochroneRequest{})
	if got := m.ValhallaErrorCount(ClassUpstream); got != 1 {
		t.Errorf("upstream count after Isochrone = %d, want 1", got)
	}
}

func TestServeListensUntilCancelled(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- serve(ctx, ln) }()

	var resp *http.Response
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		resp, err = http.Get("http://" + addr + "/metrics")
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		cancel()
		t.Fatalf("GET /metrics: %v", err)
	}

	raw, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if readErr != nil {
		cancel()
		t.Fatalf("reading /metrics: %v", readErr)
	}
	if !strings.Contains(string(raw), "routing_dead_letters_total") {
		cancel()
		t.Fatalf("/metrics missing routing_dead_letters_total:\n%s", raw)
	}

	cancel()
	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Serve: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Error("Serve did not return after cancel")
	}
}

func TestHandlerRejectsOtherPaths(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	w := httptest.NewRecorder()
	(&Set{}).Handler().ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("GET / status = %d, want 404", w.Code)
	}
}

func scrape(t *testing.T, m *Set) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", w.Code)
	}
	return w.Body.String()
}
