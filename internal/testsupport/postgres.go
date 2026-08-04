// Package testsupport brings up the backing services the integration tests
// need.
//
// It is a normal package rather than a set of _test.go helpers because two
// packages need the same database — internal/store, which writes to it, and
// internal/worker, which runs the whole loop over it — and a test-only helper
// in either would not be importable from the other. Nothing under cmd/ imports
// it, so the testing dependency it carries never reaches the binary.
package testsupport

import (
	"context"
	_ "embed"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// schemaSQL is the schema the integration tests run against. Embedded rather
// than read by path so it resolves from whichever package's directory the test
// happens to be running in.
//
// It is a copy of sparks-effect-api's 00014_routing_jobs.sql, not a migration
// of this repository's own; see schema.sql for why the real thing lives over
// there and what that costs.
//
//go:embed schema.sql
var schemaSQL string

// Job status values, duplicated from internal/store so this package does not
// import the thing most of its callers are testing.
const statusQueued = "queued"

// Postgres connects to the throwaway database named by TEST_DATABASE_URL and
// resets it to a known-empty schema.
//
// The gating rule matches sparks-effect-api's: an unset TEST_DATABASE_URL skips
// the test locally, so `make test` stays green with nothing running, but is a
// hard failure under CI — where a missing database means the job is broken, not
// that there is nothing to check.
func Postgres(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("TEST_DATABASE_URL is unset under CI; the integration suite must not silently skip")
		}
		t.Skip("TEST_DATABASE_URL not set; skipping Postgres integration test")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("connecting to test database: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("pinging test database: %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		t.Fatalf("applying test schema: %v", err)
	}
	return pool
}

// SeedCompileJob inserts the compile job row that routing_jobs and
// isochrone_cache both reference. Standalone because the cache is keyed on a
// compile job and has no interest in any particular routing job.
func SeedCompileJob(t *testing.T, pool *pgxpool.Pool, compileJobID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO jobs (id) VALUES ($1) ON CONFLICT DO NOTHING`, compileJobID); err != nil {
		t.Fatalf("seeding compile job: %v", err)
	}
}

// DeleteCompileJob removes a compile job, exercising the cascade that a deleted
// scenario or service triggers in the real schema.
func DeleteCompileJob(t *testing.T, pool *pgxpool.Pool, compileJobID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM jobs WHERE id = $1`, compileJobID); err != nil {
		t.Fatalf("deleting compile job: %v", err)
	}
}

// IsochroneCacheTilesetAt reads back the tileset stamp on one cache row, which
// is written but never read by the lookup path — so only a test that goes
// behind the store can see whether it landed.
func IsochroneCacheTilesetAt(t *testing.T, pool *pgxpool.Pool, compileJobID, stationSlug, mode string, contourMins int) *time.Time {
	t.Helper()
	var at *time.Time
	row := pool.QueryRow(context.Background(),
		`SELECT tileset_at FROM isochrone_cache
		  WHERE compile_job_id = $1 AND station_slug = $2 AND mode = $3 AND contour_mins = $4`,
		compileJobID, stationSlug, mode, contourMins)
	if err := row.Scan(&at); err != nil {
		t.Fatalf("reading tileset_at for %s/%s: %v", compileJobID, stationSlug, err)
	}
	return at
}

// IsochroneCacheStamps returns the tileset stamp on every row held for one
// compile job, nulls included.
func IsochroneCacheStamps(t *testing.T, pool *pgxpool.Pool, compileJobID string) []*time.Time {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT tileset_at FROM isochrone_cache WHERE compile_job_id = $1`, compileJobID)
	if err != nil {
		t.Fatalf("reading tileset stamps for %s: %v", compileJobID, err)
	}
	defer rows.Close()

	var stamps []*time.Time
	for rows.Next() {
		var at *time.Time
		if err := rows.Scan(&at); err != nil {
			t.Fatalf("scanning tileset stamp: %v", err)
		}
		stamps = append(stamps, at)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reading tileset stamps for %s: %v", compileJobID, err)
	}
	return stamps
}

// IsochroneCacheCount counts the rows held for one compile job.
func IsochroneCacheCount(t *testing.T, pool *pgxpool.Pool, compileJobID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM isochrone_cache WHERE compile_job_id = $1`, compileJobID).Scan(&n); err != nil {
		t.Fatalf("counting cache rows for %s: %v", compileJobID, err)
	}
	return n
}

// SeedRoutingJob inserts a queued routing job — and the compile job its foreign
// key requires — mirroring what sparks-effect-api does before it publishes.
func SeedRoutingJob(t *testing.T, pool *pgxpool.Pool, jobID, compileJobID string) {
	t.Helper()
	ctx := context.Background()

	SeedCompileJob(t, pool, compileJobID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO routing_jobs (id, status, compile_job_id, lat, lng, budget_mins, mode)
		 VALUES ($1, $2, $3, 37.79, -122.397, 45, 'walk')`,
		jobID, statusQueued, compileJobID); err != nil {
		t.Fatalf("seeding routing job: %v", err)
	}
}

// RoutingJobStatus reads back a job's status, error and result.
func RoutingJobStatus(t *testing.T, pool *pgxpool.Pool, jobID string) (status, errMessage string, result []byte) {
	t.Helper()
	row := pool.QueryRow(context.Background(),
		`SELECT status, error, result FROM routing_jobs WHERE id = $1`, jobID)
	if err := row.Scan(&status, &errMessage, &result); err != nil {
		t.Fatalf("reading routing job %s: %v", jobID, err)
	}
	return status, errMessage, result
}
