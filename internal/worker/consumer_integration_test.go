package worker_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/testsupport"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/worker"
)

const (
	testQueue      = "test.routing.jobs"
	testDeadLetter = "test.routing.jobs.dead"

	// The ids the golden fixture carries. Using them rather than fresh ones
	// means the row the worker updates is the row the published message names,
	// which is the thing being tested.
	goldenJobID     = "6f9619ff-8b86-d011-b42d-00c04fc964ff"
	goldenCompileID = "0f3b7c2a-4d1e-4a5b-9c8d-2e6f1a0b3c4d"
)

// harness is one worker wired to a real broker and a real database, with a
// routing client the test controls.
type harness struct {
	pool   *pgxpool.Pool
	ch     *amqp.Channel
	client valhalla.Client
	url    string
}

func newHarness(t *testing.T, client valhalla.Client) *harness {
	t.Helper()
	pool := testsupport.Postgres(t)
	url, ch := testsupport.Broker(t)
	testsupport.ResetQueues(t, ch, testQueue, testDeadLetter)
	return &harness{pool: pool, ch: ch, client: client, url: url}
}

// run starts a consumer and returns handles to stop it two ways.
//
// stop is the graceful path: cancel, wait for the delivery in hand to settle,
// close. kill is the crash: drop the broker connection immediately, without
// waiting for anything to settle, which is what leaves a delivery unacked and
// makes the broker redeliver it.
func (h *harness) run(t *testing.T, maxRetries int) (stop, kill func()) {
	t.Helper()
	handler := worker.NewHandler(store.NewRepo(h.pool), h.client, valhalla.DefaultLimits(), logger.Discard())
	consumer := worker.NewConsumer(h.url, testQueue, testDeadLetter, maxRetries, handler, logger.Discard())

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := consumer.Run(ctx); err != nil {
			t.Logf("consumer stopped: %v", err)
		}
	}()

	var once sync.Once
	stop = func() {
		once.Do(func() {
			cancel()
			wg.Wait()
			consumer.Close()
		})
	}
	kill = func() {
		once.Do(func() {
			cancel()
			// Deliberately no wg.Wait(): the consumer may be blocked inside a
			// routing call, and waiting for it would be waiting for the very
			// thing this is simulating the death of.
			consumer.Close()
		})
	}
	return stop, kill
}

func (h *harness) waitForStatus(t *testing.T, jobID, want string) {
	t.Helper()
	testsupport.WaitFor(t, 15*time.Second, "job "+jobID+" to reach "+want, func() bool {
		status, _, _ := testsupport.RoutingJobStatus(t, h.pool, jobID)
		return status == want
	})
}

// The headline acceptance criterion: a message the API published is consumed,
// computed, and its row driven queued → running → succeeded with the chained
// result attached.
func TestEndToEnd_publishedJobIsComputedAndRecorded(t *testing.T) {
	h := newHarness(t, valhalla.NewSyntheticClient())
	testsupport.SeedRoutingJob(t, h.pool, goldenJobID, goldenCompileID)

	stop, _ := h.run(t, 3)
	defer stop()

	testsupport.Publish(t, h.ch, testQueue, goldenMessage(t))
	h.waitForStatus(t, goldenJobID, store.StatusSucceeded)

	_, errMsg, result := testsupport.RoutingJobStatus(t, h.pool, goldenJobID)
	if errMsg != "" {
		t.Errorf("error = %q, want empty", errMsg)
	}

	var parsed struct {
		Type     string `json:"type"`
		Features []any  `json:"features"`
		Metadata struct {
			CompileJobID string `json:"compile_job_id"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	if parsed.Type != "FeatureCollection" {
		t.Errorf("result type = %q, want FeatureCollection", parsed.Type)
	}
	if len(parsed.Features) == 0 {
		t.Error("result carries no features")
	}
	if parsed.Metadata.CompileJobID != goldenCompileID {
		t.Errorf("compile_job_id = %q, want %q", parsed.Metadata.CompileJobID, goldenCompileID)
	}

	// Acked, so nothing is left behind on either queue.
	testsupport.WaitFor(t, 5*time.Second, "the queue to drain", func() bool {
		return testsupport.QueueDepth(t, h.ch, testQueue) == 0
	})
	if got := testsupport.QueueDepth(t, h.ch, testDeadLetter); got != 0 {
		t.Errorf("dead-letter depth = %d, want 0", got)
	}
}

// A permanent failure marks the job failed and does not requeue. Redelivering
// it would loop forever against a server with room for one chain at a time.
func TestEndToEnd_permanentFailureFailsTheJobAndDoesNotRequeue(t *testing.T) {
	h := newHarness(t, valhalla.NewSyntheticClient())
	testsupport.SeedRoutingJob(t, h.pool, goldenJobID, goldenCompileID)

	stop, _ := h.run(t, 3)
	defer stop()

	testsupport.Publish(t, h.ch, testQueue,
		mutateGolden(t, func(m map[string]any) { m["mode"] = "teleport" }))
	h.waitForStatus(t, goldenJobID, store.StatusFailed)

	_, errMsg, _ := testsupport.RoutingJobStatus(t, h.pool, goldenJobID)
	if errMsg == "" {
		t.Error("job failed with no message; a caller has nothing to read")
	}

	// Give a requeue time to show up if the classification were wrong. Neither
	// queue should ever hold anything: a permanent failure is acked outright,
	// not dead-lettered.
	time.Sleep(500 * time.Millisecond)
	if got := testsupport.QueueDepth(t, h.ch, testQueue); got != 0 {
		t.Errorf("queue depth = %d, want 0 — a permanent failure must not requeue", got)
	}
	if got := testsupport.QueueDepth(t, h.ch, testDeadLetter); got != 0 {
		t.Errorf("dead-letter depth = %d, want 0", got)
	}
}

// A transient failure is retried a bounded number of times, then dead-lettered
// and the job marked failed — so the poller stops waiting rather than watching
// a spinner forever.
func TestEndToEnd_transientFailureRetriesThenDeadLetters(t *testing.T) {
	const maxRetries = 2

	var attempts atomic.Int32
	client := &valhalla.StubClient{
		IsochroneFn: func(valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
			attempts.Add(1)
			return nil, valhalla.ErrUpstream
		},
	}

	h := newHarness(t, client)
	testsupport.SeedRoutingJob(t, h.pool, goldenJobID, goldenCompileID)

	stop, _ := h.run(t, maxRetries)
	defer stop()

	testsupport.Publish(t, h.ch, testQueue, goldenMessage(t))
	h.waitForStatus(t, goldenJobID, store.StatusFailed)

	// The first delivery plus maxRetries redeliveries. More would mean the
	// bound is not being enforced; fewer, that it is too tight.
	testsupport.WaitFor(t, 5*time.Second, "the message to reach the dead-letter queue", func() bool {
		return testsupport.QueueDepth(t, h.ch, testDeadLetter) == 1
	})
	if got := attempts.Load(); got != maxRetries+1 {
		t.Errorf("routing attempts = %d, want %d (first delivery + %d retries)", got, maxRetries+1, maxRetries)
	}
	if got := testsupport.QueueDepth(t, h.ch, testQueue); got != 0 {
		t.Errorf("live queue depth = %d, want 0 — the message should have moved on", got)
	}

	_, errMsg, _ := testsupport.RoutingJobStatus(t, h.pool, goldenJobID)
	if errMsg == "" {
		t.Error("job failed with no message after exhausting retries")
	}
}

// A transient failure that clears before the bound is reached completes
// normally — the retry path has to be able to succeed, not just to give up
// tidily.
func TestEndToEnd_transientFailureThatRecoversSucceeds(t *testing.T) {
	synthetic := valhalla.NewSyntheticClient()
	var attempts atomic.Int32
	client := &valhalla.StubClient{
		MatrixResp: &valhalla.MatrixResponse{SourcesToTargets: [][]valhalla.MatrixCell{{{Time: -1}, {Time: -1}}}},
		IsochroneFn: func(r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
			if attempts.Add(1) == 1 {
				return nil, valhalla.ErrUpstream
			}
			return synthetic.Isochrone(context.Background(), r)
		},
	}

	h := newHarness(t, client)
	testsupport.SeedRoutingJob(t, h.pool, goldenJobID, goldenCompileID)

	stop, _ := h.run(t, 3)
	defer stop()

	testsupport.Publish(t, h.ch, testQueue, goldenMessage(t))
	h.waitForStatus(t, goldenJobID, store.StatusSucceeded)
}

// Killing the worker mid-chain redelivers the job rather than losing it.
//
// The ack happens only after the row is written, so a delivery interrupted
// before that is still unacknowledged when the connection drops — and the
// broker gives it to the next consumer. This is the crash path, distinct from
// the worker's own bounded retry: nothing here reaches the requeue logic,
// because the first worker never gets to decide anything.
//
// The first worker is stopped while it is inside a routing call that has not
// returned. The second, with a working client, has to finish the job.
func TestEndToEnd_workerKilledMidChainRedelivers(t *testing.T) {
	h := newHarness(t, nil)
	testsupport.SeedRoutingJob(t, h.pool, goldenJobID, goldenCompileID)

	// blocked is released only at the very end, so the first worker is still
	// genuinely inside the call when its connection is dropped.
	blocked := make(chan struct{})
	defer close(blocked)
	entered := make(chan struct{}, 1)
	h.client = &valhalla.StubClient{
		IsochroneFn: func(valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-blocked
			return nil, valhalla.ErrUpstream
		},
	}

	_, killFirst := h.run(t, 3)
	testsupport.Publish(t, h.ch, testQueue, goldenMessage(t))

	select {
	case <-entered:
	case <-time.After(15 * time.Second):
		killFirst()
		t.Fatal("the first worker never started a routing call")
	}

	// The delivery is in flight and unacknowledged. Kill the connection under
	// it — the first worker never acks, nacks, or republishes anything.
	killFirst()

	if status, _, _ := testsupport.RoutingJobStatus(t, h.pool, goldenJobID); status == store.StatusSucceeded {
		t.Fatal("the job completed before the worker was killed; the test proves nothing")
	}

	// The broker must have put the unacked message back.
	testsupport.WaitFor(t, 15*time.Second, "the unacked message to be redelivered to the queue", func() bool {
		return testsupport.QueueDepth(t, h.ch, testQueue) == 1
	})

	// A second worker, with a client that works, picks it up and finishes it.
	h.client = valhalla.NewSyntheticClient()
	stopSecond, _ := h.run(t, 3)
	defer stopSecond()

	h.waitForStatus(t, goldenJobID, store.StatusSucceeded)

	_, _, result := testsupport.RoutingJobStatus(t, h.pool, goldenJobID)
	if len(result) == 0 {
		t.Error("job succeeded with no result attached")
	}
	if got := testsupport.QueueDepth(t, h.ch, testDeadLetter); got != 0 {
		t.Errorf("dead-letter depth = %d, want 0 — a crash is not a job failure", got)
	}
}

// A message naming a routing job with no row is acked and discarded: there is
// nothing to write an answer to, so redelivering it forever would achieve
// nothing but occupying the queue.
func TestEndToEnd_messageForAMissingJobIsDiscarded(t *testing.T) {
	h := newHarness(t, valhalla.NewSyntheticClient())
	// Deliberately not seeded.

	stop, _ := h.run(t, 3)
	defer stop()

	testsupport.Publish(t, h.ch, testQueue, goldenMessage(t))

	testsupport.WaitFor(t, 10*time.Second, "the message to be discarded", func() bool {
		return testsupport.QueueDepth(t, h.ch, testQueue) == 0
	})
	time.Sleep(300 * time.Millisecond)
	if got := testsupport.QueueDepth(t, h.ch, testQueue); got != 0 {
		t.Errorf("queue depth = %d, want 0 — a job with no row must not be retried forever", got)
	}
}
