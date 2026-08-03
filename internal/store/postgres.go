// Package store is this worker's half of the routing_jobs table.
//
// The table is co-owned by construction: sparks-effect-api inserts a row and
// polls it, this worker transitions it and writes its result. The schema —
// including this table — lives in that repository's migration sequence, because
// both it and the isochrone cache want real foreign keys into users and jobs,
// and a foreign key cannot cross a schema owned by a different migration
// sequence.
//
// So there are no migrations here, and there deliberately never will be. A
// second goose sequence against the same database would be two tools racing to
// own one schema_migrations table.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Job status values, matching the vocabulary sparks-effect-api uses for both
// compile jobs and routing jobs. Reusing it means a client polling either
// surface does not have to learn two spellings of the same lifecycle.
const (
	StatusQueued    = "queued"
	StatusRunning   = "running"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

// ErrJobNotFound reports an update that matched no row.
//
// It is not necessarily an error in the worker: a routing job whose compile job
// was deleted cascades away, so a message can outlive its row. The consumer
// treats it as permanent — there is nothing to write the answer to — and acks
// rather than redelivering forever.
var ErrJobNotFound = errors.New("store: routing job not found")

// RoutingJobStore is the seam the worker writes results through.
//
// Three calls, one per transition the worker is responsible for. There is no
// read: everything needed to compute the isochrone arrives on the message, so a
// worker that could not reach the database would still have nowhere useful to
// put an answer.
type RoutingJobStore interface {
	MarkRunning(ctx context.Context, jobID string) error
	MarkSucceeded(ctx context.Context, jobID string, result json.RawMessage) error
	MarkFailed(ctx context.Context, jobID, message string) error
}

// Repo is a Postgres-backed RoutingJobStore over a pgx connection pool.
type Repo struct {
	pool *pgxpool.Pool
}

var _ RoutingJobStore = (*Repo)(nil)

// Connect opens a pgx connection pool against databaseURL. If maxConns > 0 it
// overrides the pool's max connection count.
func Connect(ctx context.Context, databaseURL string, maxConns int) (*Repo, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("store: parsing DATABASE_URL: %w", err)
	}
	if maxConns > 0 {
		cfg.MaxConns = int32(maxConns)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("store: creating pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	return &Repo{pool: pool}, nil
}

// NewRepo wraps an existing pool. Used by the integration tests, which manage
// their own connection.
func NewRepo(pool *pgxpool.Pool) *Repo { return &Repo{pool: pool} }

// Close releases the connection pool.
func (r *Repo) Close() { r.pool.Close() }

// MarkRunning moves a job to running.
//
// It is the one transition that is allowed to be a no-op. A job redelivered
// after a crash is already running, and refusing to re-mark it would fail work
// that is otherwise perfectly recoverable — so the update matches any status
// short of terminal rather than requiring `queued`. Terminal statuses are
// excluded so a late redelivery cannot drag a finished job backwards.
func (r *Repo) MarkRunning(ctx context.Context, jobID string) error {
	return r.exec(ctx, jobID,
		`UPDATE routing_jobs
		    SET status = $2, updated_at = now()
		  WHERE id = $1
		    AND status NOT IN ($3, $4)`,
		jobID, StatusRunning, StatusSucceeded, StatusFailed)
}

// MarkSucceeded records the computed isochrone and completes the job.
//
// This is the write the consumer acks behind: until it lands, a crash
// redelivers the message and the chain runs again. That is safe because the
// statement is an unconditional overwrite keyed by id — recomputing an
// isochrone over an immutable graph produces the same answer, so a second
// write is indistinguishable from the first.
func (r *Repo) MarkSucceeded(ctx context.Context, jobID string, result json.RawMessage) error {
	return r.exec(ctx, jobID,
		`UPDATE routing_jobs
		    SET status = $2, result = $3, error = '', updated_at = now()
		  WHERE id = $1`,
		jobID, StatusSucceeded, []byte(result))
}

// MarkFailed records why a job cannot be completed.
//
// It is what stops a client polling forever, so it is called on every path that
// abandons a job — a message the worker cannot read, a request Valhalla
// refuses, retries exhausted. The message is stored as given; callers are
// expected to have wrapped it into something a person can act on.
//
// Unlike MarkRunning it does not exclude terminal statuses. A job that
// succeeded and is then reported failed is a case that should not arise, and if
// it does the failure is the more recent information.
func (r *Repo) MarkFailed(ctx context.Context, jobID, message string) error {
	return r.exec(ctx, jobID,
		`UPDATE routing_jobs
		    SET status = $2, error = $3, updated_at = now()
		  WHERE id = $1`,
		jobID, StatusFailed, message)
}

// exec runs an update and reports ErrJobNotFound when it matched nothing.
//
// A zero row count is genuinely ambiguous — the row may never have existed, or
// may have cascaded away, or (for MarkRunning) may already be terminal — and
// none of those is worth telling apart at the call site. All three mean the
// same thing to the consumer: stop trying to write to this job.
func (r *Repo) exec(ctx context.Context, jobID, sql string, args ...any) error {
	tag, err := r.pool.Exec(ctx, sql, args...)
	if err != nil {
		return fmt.Errorf("store: updating routing job %s: %w", jobID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: %s", ErrJobNotFound, jobID)
	}
	return nil
}
