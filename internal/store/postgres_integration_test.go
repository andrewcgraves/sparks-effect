package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/testsupport"
)

const (
	testJobID     = "6f9619ff-8b86-d011-b42d-00c04fc964ff"
	testCompileID = "0f3b7c2a-4d1e-4a5b-9c8d-2e6f1a0b3c4d"
	otherJobID    = "11111111-2222-3333-4444-555555555555"
)

// The lifecycle the API polls: queued, then running, then succeeded with the
// result attached.
func TestRoutingJobLifecycle(t *testing.T) {
	pool := testsupport.Postgres(t)
	testsupport.SeedRoutingJob(t, pool, testJobID, testCompileID)
	repo := store.NewRepo(pool)
	ctx := context.Background()

	if status, _, _ := testsupport.RoutingJobStatus(t, pool, testJobID); status != store.StatusQueued {
		t.Fatalf("seeded status = %q, want queued", status)
	}

	if err := repo.MarkRunning(ctx, testJobID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	if status, _, _ := testsupport.RoutingJobStatus(t, pool, testJobID); status != store.StatusRunning {
		t.Errorf("status = %q, want running", status)
	}

	want := json.RawMessage(`{"type":"FeatureCollection","features":[]}`)
	if err := repo.MarkSucceeded(ctx, testJobID, want); err != nil {
		t.Fatalf("MarkSucceeded: %v", err)
	}

	status, errMsg, result := testsupport.RoutingJobStatus(t, pool, testJobID)
	if status != store.StatusSucceeded {
		t.Errorf("status = %q, want succeeded", status)
	}
	if errMsg != "" {
		t.Errorf("error = %q, want empty on success", errMsg)
	}
	var got map[string]any
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if got["type"] != "FeatureCollection" {
		t.Errorf("result type = %v, want FeatureCollection", got["type"])
	}
}

// Redelivery after a crash re-marks a job that is already running. Refusing
// that would fail work that is otherwise perfectly recoverable, so MarkRunning
// is deliberately not conditional on the row being queued.
func TestMarkRunningIsIdempotent(t *testing.T) {
	pool := testsupport.Postgres(t)
	testsupport.SeedRoutingJob(t, pool, testJobID, testCompileID)
	repo := store.NewRepo(pool)
	ctx := context.Background()

	if err := repo.MarkRunning(ctx, testJobID); err != nil {
		t.Fatalf("first MarkRunning: %v", err)
	}
	if err := repo.MarkRunning(ctx, testJobID); err != nil {
		t.Errorf("second MarkRunning: %v, want nil — a redelivered job is already running", err)
	}
}

// A late redelivery must not drag a finished job backwards into running, or a
// client that already saw its answer would start polling again.
func TestMarkRunningWillNotReopenATerminalJob(t *testing.T) {
	pool := testsupport.Postgres(t)
	repo := store.NewRepo(pool)
	ctx := context.Background()

	for _, tc := range []struct {
		name  string
		close func(string) error
		want  string
	}{
		{"succeeded", func(id string) error { return repo.MarkSucceeded(ctx, id, json.RawMessage(`{}`)) }, store.StatusSucceeded},
		{"failed", func(id string) error { return repo.MarkFailed(ctx, id, "gave up") }, store.StatusFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testsupport.SeedRoutingJob(t, pool, testJobID, testCompileID)
			if err := tc.close(testJobID); err != nil {
				t.Fatalf("closing job: %v", err)
			}

			if err := repo.MarkRunning(ctx, testJobID); !errors.Is(err, store.ErrJobNotFound) {
				t.Errorf("MarkRunning on a %s job = %v, want ErrJobNotFound", tc.want, err)
			}
			if status, _, _ := testsupport.RoutingJobStatus(t, pool, testJobID); status != tc.want {
				t.Errorf("status = %q, want it to stay %q", status, tc.want)
			}

			testsupport.RoutingJobStatus(t, pool, testJobID)
			if _, err := pool.Exec(ctx, `DELETE FROM routing_jobs WHERE id = $1`, testJobID); err != nil {
				t.Fatalf("cleaning up: %v", err)
			}
		})
	}
}

// Recomputing over an immutable graph gives the same answer, which is what
// makes acking after the write safe: a redelivered job overwrites its own
// result rather than conflicting with it.
func TestMarkSucceededOverwrites(t *testing.T) {
	pool := testsupport.Postgres(t)
	testsupport.SeedRoutingJob(t, pool, testJobID, testCompileID)
	repo := store.NewRepo(pool)
	ctx := context.Background()

	if err := repo.MarkSucceeded(ctx, testJobID, json.RawMessage(`{"attempt":1}`)); err != nil {
		t.Fatalf("first MarkSucceeded: %v", err)
	}
	if err := repo.MarkSucceeded(ctx, testJobID, json.RawMessage(`{"attempt":2}`)); err != nil {
		t.Fatalf("second MarkSucceeded: %v", err)
	}

	_, _, result := testsupport.RoutingJobStatus(t, pool, testJobID)
	var got map[string]any
	if err := json.Unmarshal(result, &got); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if got["attempt"] != float64(2) {
		t.Errorf("result = %v, want the second write to have won", got)
	}
}

// Marking a job failed clears nothing but says why, which is what stops a
// client polling it forever.
func TestMarkFailedRecordsTheReason(t *testing.T) {
	pool := testsupport.Postgres(t)
	testsupport.SeedRoutingJob(t, pool, testJobID, testCompileID)
	repo := store.NewRepo(pool)

	const reason = "exhausted 3 retries: valhalla unreachable"
	if err := repo.MarkFailed(context.Background(), testJobID, reason); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	status, errMsg, _ := testsupport.RoutingJobStatus(t, pool, testJobID)
	if status != store.StatusFailed {
		t.Errorf("status = %q, want failed", status)
	}
	if errMsg != reason {
		t.Errorf("error = %q, want %q", errMsg, reason)
	}
}

// A job whose row never existed, or has cascaded away with its compile job,
// reports ErrJobNotFound — which the worker treats as permanent, because there
// is nothing left to write an answer to.
func TestUpdatesOnAMissingRowReportNotFound(t *testing.T) {
	pool := testsupport.Postgres(t)
	repo := store.NewRepo(pool)
	ctx := context.Background()

	if err := repo.MarkRunning(ctx, otherJobID); !errors.Is(err, store.ErrJobNotFound) {
		t.Errorf("MarkRunning = %v, want ErrJobNotFound", err)
	}
	if err := repo.MarkSucceeded(ctx, otherJobID, json.RawMessage(`{}`)); !errors.Is(err, store.ErrJobNotFound) {
		t.Errorf("MarkSucceeded = %v, want ErrJobNotFound", err)
	}
	if err := repo.MarkFailed(ctx, otherJobID, "boom"); !errors.Is(err, store.ErrJobNotFound) {
		t.Errorf("MarkFailed = %v, want ErrJobNotFound", err)
	}
}

// Deleting a compile job cascades its routing jobs away. This is the situation
// ErrJobNotFound exists for, and it is worth exercising against real foreign
// keys rather than asserting the worker's handling of a case that could not
// arise.
func TestDeletingTheCompileJobCascades(t *testing.T) {
	pool := testsupport.Postgres(t)
	testsupport.SeedRoutingJob(t, pool, testJobID, testCompileID)
	repo := store.NewRepo(pool)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `DELETE FROM jobs WHERE id = $1`, testCompileID); err != nil {
		t.Fatalf("deleting compile job: %v", err)
	}

	if err := repo.MarkSucceeded(ctx, testJobID, json.RawMessage(`{}`)); !errors.Is(err, store.ErrJobNotFound) {
		t.Errorf("MarkSucceeded after cascade = %v, want ErrJobNotFound", err)
	}
}
