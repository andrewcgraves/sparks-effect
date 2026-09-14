package config_test

import (
	"testing"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/config"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

func TestDefaultsMatchTheDeployedContainer(t *testing.T) {
	t.Setenv("METRICS_ADDR", "")
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
	if cfg.MetricsAddr != ":9090" {
		t.Errorf("MetricsAddr = %q, want :9090", cfg.MetricsAddr)
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
	t.Setenv("VALHALLA_URL", "")
	if got := config.Load().ValhallaURL; got != "" {
		t.Errorf("ValhallaURL = %q, want empty when unset", got)
	}
}

func TestValhallaSyntheticDefaultsFalse(t *testing.T) {
	t.Setenv("VALHALLA_SYNTHETIC", "")
	if config.Load().ValhallaSynthetic {
		t.Error("ValhallaSynthetic = true, want false when unset")
	}
}

func setRequiredEnv(t *testing.T) {
	t.Helper()
	t.Setenv("AMQP_URL", "amqp://guest:guest@localhost:5672/")
	t.Setenv("API_URL", "http://api.example")
	t.Setenv("WORKER_TOKEN", "secret")
	t.Setenv("VALHALLA_URL", "http://valhalla:8002")
	t.Setenv("VALHALLA_SYNTHETIC", "")
}

func TestValidateAcceptsCompleteConfig(t *testing.T) {
	setRequiredEnv(t)
	if err := config.Load().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateRejectsMissingRequired(t *testing.T) {
	cases := []struct {
		unset string
		want  string
	}{
		{"AMQP_URL", "AMQP_URL is required"},
		{"API_URL", "API_URL is required"},
		{"WORKER_TOKEN", "WORKER_TOKEN is required"},
		{"VALHALLA_URL", "VALHALLA_URL is required (set VALHALLA_SYNTHETIC=true to serve synthetic isochrones)"},
	}
	for _, tc := range cases {
		t.Run(tc.unset, func(t *testing.T) {
			setRequiredEnv(t)
			t.Setenv(tc.unset, "")
			if err := config.Load().Validate(); err == nil || err.Error() != tc.want {
				t.Errorf("Validate() = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateAcceptsSyntheticOptIn(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("VALHALLA_URL", "")
	t.Setenv("VALHALLA_SYNTHETIC", "true")

	cfg := config.Load()
	if !cfg.ValhallaSynthetic {
		t.Fatal("ValhallaSynthetic = false, want true")
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}
}

func TestValidateRejectsURLAndSynthetic(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("VALHALLA_SYNTHETIC", "true")

	const want = "VALHALLA_URL and VALHALLA_SYNTHETIC=true are mutually exclusive"
	if err := config.Load().Validate(); err == nil || err.Error() != want {
		t.Errorf("Validate() = %v, want %q", err, want)
	}
}

func TestUnrecognizedEnvNamesLeftoverAndUnknown(t *testing.T) {
	setRequiredEnv(t)
	t.Setenv("DATABASE_URL", "postgres://legacy")
	t.Setenv("DATABASE_MAX_CONNS", "8")
	t.Setenv("SPARKS_NEW_FLAG", "1")

	got := config.UnrecognizedEnv()
	for _, want := range []string{"DATABASE_URL", "DATABASE_MAX_CONNS", "SPARKS_NEW_FLAG"} {
		if !containsName(got, want) {
			t.Errorf("UnrecognizedEnv() = %v, missing %q", got, want)
		}
	}
	for _, known := range []string{"API_URL", "AMQP_URL", "VALHALLA_URL", "WORKER_TOKEN"} {
		if containsName(got, known) {
			t.Errorf("UnrecognizedEnv() = %v, unexpectedly includes known %q", got, known)
		}
	}
}

func TestUnrecognizedEnvSkipsKubernetesServiceLinks(t *testing.T) {
	t.Setenv("VALHALLA_SERVICE_HOST", "10.0.0.8")
	t.Setenv("VALHALLA_SERVICE_PORT", "8002")
	t.Setenv("VALHALLA_PORT", "tcp://10.0.0.8:8002")
	t.Setenv("VALHALLA_PORT_8002_TCP", "tcp://10.0.0.8:8002")
	t.Setenv("VALHALLA_PORT_8002_TCP_ADDR", "10.0.0.8")
	t.Setenv("API_SERVICE_HOST", "10.0.0.9")

	got := config.UnrecognizedEnv()
	for _, name := range []string{
		"VALHALLA_SERVICE_HOST", "VALHALLA_SERVICE_PORT", "VALHALLA_PORT",
		"VALHALLA_PORT_8002_TCP", "VALHALLA_PORT_8002_TCP_ADDR", "API_SERVICE_HOST",
	} {
		if containsName(got, name) {
			t.Errorf("UnrecognizedEnv() = %v, includes Kubernetes service-link %q", got, name)
		}
	}
}

func TestUnrecognizedEnvIgnoresEmptyValues(t *testing.T) {
	t.Setenv("SPARKS_EMPTY", "")
	if containsName(config.UnrecognizedEnv(), "SPARKS_EMPTY") {
		t.Error("empty SPARKS_EMPTY should not be reported as supplied")
	}
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func TestAPICredentialsHaveNoDefault(t *testing.T) {
	t.Setenv("API_URL", "")
	t.Setenv("WORKER_TOKEN", "")
	if got := config.Load().APIURL; got != "" {
		t.Errorf("APIURL = %q, want empty when unset", got)
	}
	if got := config.Load().WorkerToken; got != "" {
		t.Errorf("WorkerToken = %q, want empty when unset", got)
	}
}

func TestMetricsAddrIsOverridable(t *testing.T) {
	t.Setenv("METRICS_ADDR", "127.0.0.1:9191")
	if got := config.Load().MetricsAddr; got != "127.0.0.1:9191" {
		t.Errorf("MetricsAddr = %q, want 127.0.0.1:9191", got)
	}
}

func TestAPITimeoutIsOverridable(t *testing.T) {
	t.Setenv("API_TIMEOUT_SECONDS", "5")
	if got := config.Load().APITimeout; got != 5*time.Second {
		t.Errorf("APITimeout = %s, want 5s", got)
	}
}
