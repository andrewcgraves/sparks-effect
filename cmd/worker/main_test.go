package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/config"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
)

func TestParseArgsCheck(t *testing.T) {
	opts, err := parseArgs([]string{"-check"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.check {
		t.Fatal("check = false, want true")
	}
	if opts.envFile != "" {
		t.Fatalf("envFile = %q, want empty", opts.envFile)
	}
}

func TestParseArgsEnvFile(t *testing.T) {
	opts, err := parseArgs([]string{"-env-file", "staging.env", "-check"})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.check || opts.envFile != "staging.env" {
		t.Fatalf("opts = %+v", opts)
	}
}

func TestInspectConfigAcceptsSyntheticWithoutLiveServices(t *testing.T) {
	cfg := config.Config{
		AMQPURL:           "amqp://guest:guest@localhost:5672/",
		APIURL:            "http://127.0.0.1:8080",
		WorkerToken:       "local-token",
		ValhallaSynthetic: true,
	}
	if err := inspectConfig(cfg, logger.Discard()); err != nil {
		t.Fatalf("inspectConfig() = %v, want nil (synthetic local env needs no live Valhalla)", err)
	}
}

func TestInspectConfigRejectsMissingRequired(t *testing.T) {
	err := inspectConfig(config.Config{ValhallaSynthetic: true}, logger.Discard())
	if err == nil {
		t.Fatal("inspectConfig() = nil, want AMQP_URL required")
	}
	if !strings.Contains(err.Error(), "AMQP_URL") {
		t.Fatalf("inspectConfig() = %v, want it to name AMQP_URL", err)
	}
}

func TestLoadDotenvMissingFile(t *testing.T) {
	if err := loadDotenv(filepath.Join(t.TempDir(), "missing.env")); err == nil {
		t.Fatal("loadDotenv(missing) = nil, want error")
	}
}

func TestLoadDotenvEmptyPathDoesNotFail(t *testing.T) {
	if err := loadDotenv(""); err != nil {
		t.Fatalf("loadDotenv(\"\") = %v, want nil (missing .env is ignored)", err)
	}
}

func TestLoadDotenvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "local.env")
	if err := os.WriteFile(path, []byte("VALHALLA_SYNTHETIC=true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VALHALLA_SYNTHETIC", "")
	if err := loadDotenv(path); err != nil {
		t.Fatal(err)
	}
	if got := os.Getenv("VALHALLA_SYNTHETIC"); got != "true" {
		t.Fatalf("VALHALLA_SYNTHETIC = %q after loading %s, want true", got, path)
	}
}
