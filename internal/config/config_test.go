package config_test

import (
	"testing"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/config"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

// The defaults have to be the limits the deployed container actually enforces,
// because a limit exceeded is a 400 and a 400 is permanent — a wrong default
// here fails jobs rather than merely slowing them down.
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

	// 320, not Stadia's 120: kustomize-config patches
	// service_limits.isochrone.max_time_contour on every pod start.
	if cfg.Limits.MaxIsoContourMins != 320 {
		t.Errorf("MaxIsoContourMins = %d, want 320 (the patched ceiling)", cfg.Limits.MaxIsoContourMins)
	}
	// 2500, not Stadia's 600: matrix limits are not patched, so Valhalla's own
	// max_matrix_location_pairs applies.
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

// Every limit is configuration, not a constant, because a deployment may patch
// Valhalla differently — that is the whole reason they moved out of the code.
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

// A limit set to nonsense falls back rather than taking effect. Zero
// concurrency would stall every chain forever and zero matrix pairs would send
// an empty matrix; neither is what anyone meant by setting the variable.
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

// Zero retries is a real choice — fail on the first transient error — and has
// to survive the parser that rejects zero everywhere else.
func TestZeroRetriesIsRespected(t *testing.T) {
	t.Setenv("ROUTING_MAX_RETRIES", "0")

	if got := config.Load().MaxRetries; got != 0 {
		t.Errorf("MaxRetries = %d, want 0", got)
	}
}

// An unset Valhalla URL is what selects the synthetic client, so it must stay
// empty rather than acquiring a plausible-looking default that would be
// dialled and fail.
func TestValhallaURLHasNoDefault(t *testing.T) {
	if got := config.Load().ValhallaURL; got != "" {
		t.Errorf("ValhallaURL = %q, want empty when unset", got)
	}
}
