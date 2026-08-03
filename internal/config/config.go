// Package config loads runtime configuration from the environment.
package config

import (
	"os"
	"strconv"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

// Config holds the settings needed to run the routing worker.
type Config struct {
	// AMQPURL is the broker the worker consumes routing jobs from. It is the
	// one setting with no useful default: a worker with no queue has nothing
	// to do, so an empty value is a startup failure rather than a degraded
	// mode.
	AMQPURL string
	// RoutingQueue is the queue routing jobs arrive on. It must match what
	// sparks-effect-api publishes to — one of the few strings shared across
	// the repository boundary, hence a setting rather than a constant.
	RoutingQueue string
	// DeadLetterQueue receives jobs that exhausted their retries. Nothing
	// consumes it; it exists so an exhausted job is inspectable rather than
	// discarded.
	DeadLetterQueue string
	// MaxRetries bounds how many times a transient failure is redelivered
	// before the job is dead-lettered and marked failed. Zero means a
	// transient failure fails the job on first sight.
	MaxRetries int
	// DatabaseURL is the Postgres connection string. The schema is owned by
	// sparks-effect-api's migration sequence; this worker only ever updates
	// rows in it, and runs no migrations of its own.
	DatabaseURL string
	// DBMaxConns caps the pgx pool size when > 0. Prefetch is 1, so one
	// connection is genuinely enough; the setting exists for the operator who
	// disagrees.
	DBMaxConns int
	// ValhallaURL points at the in-cluster routing service, e.g.
	// http://valhalla:8002. When empty the synthetic client is used instead,
	// which is what makes the whole loop runnable without a tileset.
	ValhallaURL string
	// ValhallaTimeout bounds one routing call.
	ValhallaTimeout time.Duration
	// Limits are the request ceilings the routing server enforces. They
	// default to what the deployed container actually applies; see
	// valhalla.DefaultLimits.
	Limits valhalla.Limits
	// Debug enables verbose logging. Set LOG_LEVEL=debug or VERBOSE=true.
	Debug bool
}

// defaultRoutingQueue is the queue name both sides of the routing contract use
// unless deliberately overridden. Defaulted rather than required because
// getting it wrong is silent — the API publishes happily to a queue nothing
// consumes — so the two repositories agreeing by default is worth more than
// forcing the operator to set it.
const defaultRoutingQueue = "routing.jobs"

// defaultDeadLetterQueue is where exhausted jobs land. Derived by suffix from
// the routing queue's default rather than shared with the API, which has no
// interest in it: only this side dead-letters anything.
const defaultDeadLetterQueue = "routing.jobs.dead"

// defaultMaxRetries bounds redelivery of a transient failure.
//
// Three, because the failures it covers are a routing server restarting or
// briefly overloaded, and a chain takes seconds — so three attempts spans long
// enough to ride out a pod rescheduling without holding a queue slot for
// minutes. Past that the job is failed rather than retried forever, because a
// caller polling it deserves an answer more than it deserves another attempt.
const defaultMaxRetries = 3

// Load reads configuration from environment variables, applying defaults for
// anything unset.
func Load() Config {
	limits := valhalla.DefaultLimits()
	limits.MaxMatrixPairs = envInt("VALHALLA_MAX_MATRIX_PAIRS", limits.MaxMatrixPairs)
	limits.MaxIsoContourMins = envInt("VALHALLA_MAX_ISO_CONTOUR_MINS", limits.MaxIsoContourMins)
	limits.MatrixBLineAutoKm = envFloat("VALHALLA_MATRIX_BLINE_AUTO_KM", limits.MatrixBLineAutoKm)
	limits.MatrixBLineOtherKm = envFloat("VALHALLA_MATRIX_BLINE_OTHER_KM", limits.MatrixBLineOtherKm)
	limits.MaxConcurrency = envInt("VALHALLA_MAX_CONCURRENCY", limits.MaxConcurrency)

	return Config{
		AMQPURL:         os.Getenv("AMQP_URL"),
		RoutingQueue:    getEnv("ROUTING_QUEUE", defaultRoutingQueue),
		DeadLetterQueue: getEnv("ROUTING_DEAD_LETTER_QUEUE", defaultDeadLetterQueue),
		// Zero is a meaningful setting here — fail on first transient error —
		// so it is read with envIntAllowingZero rather than envInt.
		MaxRetries:      envIntAllowingZero("ROUTING_MAX_RETRIES", defaultMaxRetries),
		DatabaseURL:     os.Getenv("DATABASE_URL"),
		DBMaxConns:      envInt("DATABASE_MAX_CONNS", 0),
		ValhallaURL:     os.Getenv("VALHALLA_URL"),
		ValhallaTimeout: time.Duration(envInt("VALHALLA_TIMEOUT_SECONDS", int(valhalla.DefaultTimeout.Seconds()))) * time.Second,
		Limits:          limits,
		Debug:           os.Getenv("LOG_LEVEL") == "debug" || os.Getenv("VERBOSE") == "true",
	}
}

func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

// envInt reads a positive integer, falling back on anything unset, unparseable,
// or non-positive. A zero or negative limit would disable the thing it bounds,
// which is never what setting it was meant to express.
func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

// envIntAllowingZero is envInt for settings where zero is a real choice.
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
