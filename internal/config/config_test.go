package config_test

import (
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/config"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

func TestDefaultsMatchTheDeployedContainer(t *testing.T) {
	cfg := config.Load()

	if cfg.RoutingQueue != "routing.jobs" {
		t.Errorf("RoutingQueue = %q, want routing.jobs (must match what the API publishes to)", cfg.RoutingQueue)
	}
	if cfg.DeadLetterQueue == "" || cfg.DeadLetterQueue == cfg.RoutingQueue {
		t.Errorf("DeadLetterQueue = %q, want a distinct queue", cfg.DeadLetterQueue)
	}
	if cfg.MaxRetries <= 0 {
		t.Errorf("MaxRetries = %d, want a positive bound", cfg.MaxRetries)
	}

	if cfg.Limits.MaxIsoContourMins != 320 {
		t.Errorf("MaxIsoContourMins = %d, want 320 (the patched ceiling)", cfg.Limits.MaxIsoContourMins)
	}

	if cfg.Limits.MaxMatrixPairs != 2500 {
		t.Errorf("MaxMatrixPairs = %d, want 2500 (Valhalla's default)", cfg.Limits.MaxMatrixPairs)
	}
	if cfg.Limits.MatrixBLineAutoKm != 400 {
		t.Errorf("MatrixBLineAutoKm = %v, want 400", cfg.Limits.MatrixBLineAutoKm)
	}
	if cfg.Limits.MatrixBLineOtherKm != 200 {
		t.Errorf("MatrixBLineOtherKm = %v, want 200", cfg.Limits.MatrixBLineOtherKm)
	}
	if cfg.Limits.MaxConcurrency <= 0 {
		t.Errorf("MaxConcurrency = %d, want a positive bound", cfg.Limits.MaxConcurrency)
	}
}

func TestLimitsAreOverridable(t *testing.T) {
	t.Setenv("VALHALLA_MAX_ISO_CONTOUR_MINS", "120")
	t.Setenv("VALHALLA_MAX_MATRIX_PAIRS", "600")
	t.Setenv("VALHALLA_MATRIX_BLINE_AUTO_KM", "250.5")
	t.Setenv("VALHALLA_MATRIX_BLINE_OTHER_KM", "100")
	t.Setenv("VALHALLA_MAX_CONCURRENCY", "2")

	got := config.Load().Limits
	want := valhalla.Limits{
		MaxIsoContourMins:  120,
		MaxMatrixPairs:     600,
		MatrixBLineAutoKm:  250.5,
		MatrixBLineOtherKm: 100,
		MaxConcurrency:     2,
	}
	if got != want {
		t.Errorf("Limits = %+v, want %+v", got, want)
	}
}

func TestNonsenseLimitsFallBackToDefaults(t *testing.T) {
	t.Setenv("VALHALLA_MAX_CONCURRENCY", "0")
	t.Setenv("VALHALLA_MAX_MATRIX_PAIRS", "not-a-number")
	t.Setenv("VALHALLA_MAX_ISO_CONTOUR_MINS", "-5")

	got := config.Load().Limits
	def := valhalla.DefaultLimits()
	if got.MaxConcurrency != def.MaxConcurrency {
		t.Errorf("MaxConcurrency = %d, want the default %d", got.MaxConcurrency, def.MaxConcurrency)
	}
	if got.MaxMatrixPairs != def.MaxMatrixPairs {
		t.Errorf("MaxMatrixPairs = %d, want the default %d", got.MaxMatrixPairs, def.MaxMatrixPairs)
	}
	if got.MaxIsoContourMins != def.MaxIsoContourMins {
		t.Errorf("MaxIsoContourMins = %d, want the default %d", got.MaxIsoContourMins, def.MaxIsoContourMins)
	}
}

func TestZeroRetriesIsRespected(t *testing.T) {
	t.Setenv("ROUTING_MAX_RETRIES", "0")

	if got := config.Load().MaxRetries; got != 0 {
		t.Errorf("MaxRetries = %d, want 0", got)
	}
}

func TestValhallaURLHasNoDefault(t *testing.T) {
	if got := config.Load().ValhallaURL; got != "" {
		t.Errorf("ValhallaURL = %q, want empty when unset", got)
	}
}

func TestAPICredentialsHaveNoDefault(t *testing.T) {
	if got := config.Load().APIURL; got != "" {
		t.Errorf("APIURL = %q, want empty when unset", got)
	}
	if got := config.Load().WorkerToken; got != "" {
		t.Errorf("WorkerToken = %q, want empty when unset", got)
	}
}

func TestAPITimeoutIsOverridable(t *testing.T) {
	t.Setenv("API_TIMEOUT_SECONDS", "5")
	if got := config.Load().APITimeout; got != 5*time.Second {
		t.Errorf("APITimeout = %s, want 5s", got)
	}
}
