package config

import (
	"errors"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

type Config struct {
	AMQPURL           string
	RoutingQueue      string
	DeadLetterQueue   string
	MaxRetries        int
	APIURL            string
	WorkerToken       string
	APITimeout        time.Duration
	ValhallaURL       string
	ValhallaSynthetic bool
	ValhallaTimeout   time.Duration
	Limits            valhalla.Limits
	Debug             bool
}

const defaultRoutingQueue = "routing.jobs"

const defaultDeadLetterQueue = "routing.jobs.dead"

const defaultMaxRetries = 3

const (
	envAMQPURL           = "AMQP_URL"
	envRoutingQueue      = "ROUTING_QUEUE"
	envDeadLetterQueue   = "ROUTING_DEAD_LETTER_QUEUE"
	envMaxRetries        = "ROUTING_MAX_RETRIES"
	envAPIURL            = "API_URL"
	envWorkerToken       = "WORKER_TOKEN"
	envAPITimeout        = "API_TIMEOUT_SECONDS"
	envValhallaURL       = "VALHALLA_URL"
	envValhallaSynthetic = "VALHALLA_SYNTHETIC"
	envValhallaTimeout   = "VALHALLA_TIMEOUT_SECONDS"
	envMaxMatrixPairs    = "VALHALLA_MAX_MATRIX_PAIRS"
	envMaxIsoContourMins = "VALHALLA_MAX_ISO_CONTOUR_MINS"
	envMatrixBLineAutoKm = "VALHALLA_MATRIX_BLINE_AUTO_KM"
	envMatrixBLineOther  = "VALHALLA_MATRIX_BLINE_OTHER_KM"
	envMaxConcurrency    = "VALHALLA_MAX_CONCURRENCY"
	envLogLevel          = "LOG_LEVEL"
	envVerbose           = "VERBOSE"
)

var knownEnv = map[string]struct{}{
	envAMQPURL:           {},
	envRoutingQueue:      {},
	envDeadLetterQueue:   {},
	envMaxRetries:        {},
	envAPIURL:            {},
	envWorkerToken:       {},
	envAPITimeout:        {},
	envValhallaURL:       {},
	envValhallaSynthetic: {},
	envValhallaTimeout:   {},
	envMaxMatrixPairs:    {},
	envMaxIsoContourMins: {},
	envMatrixBLineAutoKm: {},
	envMatrixBLineOther:  {},
	envMaxConcurrency:    {},
	envLogLevel:          {},
	envVerbose:           {},
}

var watchedPrefixes = []string{
	"SPARKS_",
	"ROUTING_",
	"VALHALLA_",
	"AMQP_",
	"DATABASE_",
	"WORKER_",
	"API_",
}

func Load() Config {
	limits := valhalla.DefaultLimits()
	limits.MaxMatrixPairs = envInt(envMaxMatrixPairs, limits.MaxMatrixPairs)
	limits.MaxIsoContourMins = envInt(envMaxIsoContourMins, limits.MaxIsoContourMins)
	limits.MatrixBLineAutoKm = envFloat(envMatrixBLineAutoKm, limits.MatrixBLineAutoKm)
	limits.MatrixBLineOtherKm = envFloat(envMatrixBLineOther, limits.MatrixBLineOtherKm)
	limits.MaxConcurrency = envInt(envMaxConcurrency, limits.MaxConcurrency)

	return Config{
		AMQPURL:           os.Getenv(envAMQPURL),
		RoutingQueue:      getEnv(envRoutingQueue, defaultRoutingQueue),
		DeadLetterQueue:   getEnv(envDeadLetterQueue, defaultDeadLetterQueue),
		MaxRetries:        envIntAllowingZero(envMaxRetries, defaultMaxRetries),
		APIURL:            os.Getenv(envAPIURL),
		WorkerToken:       os.Getenv(envWorkerToken),
		APITimeout:        time.Duration(envInt(envAPITimeout, 30)) * time.Second,
		ValhallaURL:       os.Getenv(envValhallaURL),
		ValhallaSynthetic: os.Getenv(envValhallaSynthetic) == "true",
		ValhallaTimeout:   time.Duration(envInt(envValhallaTimeout, int(valhalla.DefaultTimeout.Seconds()))) * time.Second,
		Limits:            limits,
		Debug:             os.Getenv(envLogLevel) == "debug" || os.Getenv(envVerbose) == "true",
	}
}

// Validate reports whether this config is one the binary can honour. It
// performs no network I/O: a local .env, a unit test's t.Setenv, and a
// live pod Secret are judged by the same rules.
func (c Config) Validate() error {
	if c.AMQPURL == "" {
		return errors.New("AMQP_URL is required")
	}
	if c.APIURL == "" {
		return errors.New("API_URL is required")
	}
	if c.WorkerToken == "" {
		return errors.New("WORKER_TOKEN is required")
	}
	return c.ValhallaClientConfig().Validate()
}

func (c Config) ValhallaClientConfig() valhalla.ClientConfig {
	return valhalla.ClientConfig{
		URL:       c.ValhallaURL,
		Synthetic: c.ValhallaSynthetic,
		Timeout:   c.ValhallaTimeout,
	}
}

func UnrecognizedEnv() []string {
	seen := make(map[string]struct{})
	var out []string
	for _, kv := range os.Environ() {
		key, val, _ := strings.Cut(kv, "=")
		if val == "" {
			continue
		}
		if _, ok := knownEnv[key]; ok {
			continue
		}
		if !watchedEnv(key) || isServiceLinkEnv(key) {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	sort.Strings(out)
	return out
}

func watchedEnv(key string) bool {
	if key == envLogLevel || key == envVerbose {
		return true
	}
	for _, prefix := range watchedPrefixes {
		if strings.HasPrefix(key, prefix) {
			return true
		}
	}
	return false
}

func isServiceLinkEnv(key string) bool {
	if strings.HasSuffix(key, "_SERVICE_HOST") || strings.HasSuffix(key, "_SERVICE_PORT") {
		return true
	}
	idx := strings.Index(key, "_PORT")
	if idx < 0 {
		return false
	}
	rest := key[idx+len("_PORT"):]
	return rest == "" || strings.HasPrefix(rest, "_")
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func envIntAllowingZero(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return fallback
}

func envFloat(key string, fallback float64) float64 {
	if v := os.Getenv(key); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil && f > 0 {
			return f
		}
	}
	return fallback
}
