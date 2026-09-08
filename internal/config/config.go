package config

import (
	"os"
	"strconv"
	"time"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

type Config struct {
	AMQPURL         string
	RoutingQueue    string
	DeadLetterQueue string
	MaxRetries      int
	APIURL          string
	WorkerToken     string
	APITimeout      time.Duration
	ValhallaURL     string
	ValhallaTimeout time.Duration
	Limits          valhalla.Limits
	Debug           bool
}

const defaultRoutingQueue = "routing.jobs"

const defaultDeadLetterQueue = "routing.jobs.dead"

const defaultMaxRetries = 3

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

		MaxRetries:      envIntAllowingZero("ROUTING_MAX_RETRIES", defaultMaxRetries),
		APIURL:          os.Getenv("API_URL"),
		WorkerToken:     os.Getenv("WORKER_TOKEN"),
		APITimeout:      time.Duration(envInt("API_TIMEOUT_SECONDS", 30)) * time.Second,
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
