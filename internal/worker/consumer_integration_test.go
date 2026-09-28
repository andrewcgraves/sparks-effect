package worker_test

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/geo"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/isochrone"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/metrics"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/testsupport"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/worker"
)

const (
	testQueue       = "test.routing.jobs"
	testDeadLetter  = "test.routing.jobs.dead"
	goldenJobID     = "6f9619ff-8b86-d011-b42d-00c04fc964ff"
	goldenCompileID = "0f3b7c2a-4d1e-4a5b-9c8d-2e6f1a0b3c4d"
)

type harness struct {
	api       *testsupport.FakeAPI
	store     *store.Client
	ch        *amqp.Channel
	client    valhalla.Client
	url       string
	tilesetAt time.Time
	metrics   *metrics.Set
}

func newHarness(t *testing.T, client valhalla.Client) *harness {
	t.Helper()
	api := testsupport.StartFakeAPI(t)
	repo, err := store.Connect(context.Background(), api.URL(), api.Token, 0)
	if err != nil {
		t.Fatalf("connecting to fake API: %v", err)
	}
	url, ch := testsupport.Broker(t)
	testsupport.ResetQueues(t, ch, testQueue, testDeadLetter)
	return &harness{
		api: api, store: repo, ch: ch, client: client, url: url,
		tilesetAt: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC),
	}
}

func (h *harness) run(t *testing.T, maxRetries int) (stop, kill func()) {
	t.Helper()

	h.metrics = &metrics.Set{}
	cache := isochrone.EgressCache{
		Store: h.store, Stamp: isochrone.NewTilesetStamp(h.tilesetAt), Metrics: h.metrics,
	}
	handler := worker.NewHandler(h.store, h.client, valhalla.DefaultLimits(), cache, logger.Discard(), h.metrics)
	consumer := worker.NewConsumer(h.url, testQueue, testDeadLetter, maxRetries, handler, logger.Discard(), h.metrics)

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

			consumer.Close()
		})
	}
	return stop, kill
}

func (h *harness) waitForStatus(t *testing.T, jobID, want string) {
	t.Helper()
	testsupport.WaitFor(t, 15*time.Second, "job "+jobID+" to reach "+want, func() bool {
		status, _, _ := h.api.JobStatus(t, jobID)
		return status == want
	})
}

func TestEndToEnd_publishedJobIsComputedAndRecorded(t *testing.T) {
	h := newHarness(t, valhalla.NewSyntheticClient())
	h.api.SeedRoutingJob(goldenJobID)

	stop, _ := h.run(t, 3)
	defer stop()

	testsupport.Publish(t, h.ch, testQueue, goldenMessage(t))
	h.waitForStatus(t, goldenJobID, store.StatusSucceeded)

	_, errMsg, result := h.api.JobStatus(t, goldenJobID)
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

	testsupport.WaitFor(t, 5*time.Second, "the queue to drain", func() bool {
		return testsupport.QueueDepth(t, h.ch, testQueue) == 0
	})
	if got := testsupport.QueueDepth(t, h.ch, testDeadLetter); got != 0 {
		t.Errorf("dead-letter depth = %d, want 0", got)
	}
}

type countingClient struct {
	valhalla.Client
	isochroneCalls atomic.Int32
}

func (c *countingClient) Isochrone(ctx context.Context, req valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
	c.isochroneCalls.Add(1)
	return c.Client.Isochrone(ctx, req)
}

func TestEndToEnd_warmCacheReusesEgressPolygonsAcrossJobs(t *testing.T) {
	const secondJobID = "7a0620ff-8b86-d011-b42d-00c04fc964ff"

	client := &countingClient{Client: valhalla.NewSyntheticClient()}
	h := newHarness(t, client)
	h.api.SeedRoutingJob(goldenJobID)

	stop, _ := h.run(t, 3)
	defer stop()

	widerBudget := mutateGolden(t, func(m map[string]any) { m["budget_mins"] = 90 })
	testsupport.Publish(t, h.ch, testQueue, widerBudget)
	h.waitForStatus(t, goldenJobID, store.StatusSucceeded)
	coldCalls := client.isochroneCalls.Load()

	cached := h.api.IsochroneCacheCount(goldenCompileID)
	if cached == 0 {
		t.Fatal("the completed job cached no egress polygons")
	}

	if want := int(coldCalls) - 1; cached != want {
		t.Errorf("cached %d polygons against %d isochrone calls, want %d (all but the origin)", cached, coldCalls, want)
	}
	// The harness seeds a non-zero stamp while SyntheticClient.Status returns
	// ErrNoStatus, so Refresh must keep the last known stamp for this to pass.
	for _, at := range h.api.IsochroneCacheStamps(goldenCompileID) {
		if at.IsZero() || !at.Equal(h.tilesetAt) {
			t.Errorf("cache row stamped %v, want the server's tileset %s", at, h.tilesetAt)
		}
	}

	h.api.SeedRoutingJob(secondJobID)
	client.isochroneCalls.Store(0)
	testsupport.Publish(t, h.ch, testQueue,
		mutateGolden(t, func(m map[string]any) {
			m["routing_job_id"] = secondJobID
			m["budget_mins"] = 90
		}))
	h.waitForStatus(t, secondJobID, store.StatusSucceeded)
	warmCalls := client.isochroneCalls.Load()

	if warmCalls >= coldCalls {
		t.Errorf("warm job made %d isochrone calls against a cold %d; the cache saved nothing", warmCalls, coldCalls)
	}
	if warmCalls != 1 {
		t.Errorf("warm job made %d isochrone calls, want 1 (the origin)", warmCalls)
	}

	_, _, coldResult := h.api.JobStatus(t, goldenJobID)
	_, _, warmResult := h.api.JobStatus(t, secondJobID)
	if len(features(t, coldResult)) != len(features(t, warmResult)) {
		t.Errorf("warm result has %d features, cold has %d",
			len(features(t, warmResult)), len(features(t, coldResult)))
	}

	if got := h.api.IsochroneCacheCount(goldenCompileID); got != cached {
		t.Errorf("cache holds %d rows after the warm job, want the %d the cold one left", got, cached)
	}
}

func features(t *testing.T, result []byte) []any {
	t.Helper()
	var parsed struct {
		Features []any `json:"features"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		t.Fatalf("result is not valid JSON: %v", err)
	}
	return parsed.Features
}

func TestEndToEnd_permanentFailureFailsTheJobAndDoesNotRequeue(t *testing.T) {
	h := newHarness(t, valhalla.NewSyntheticClient())
	h.api.SeedRoutingJob(goldenJobID)

	stop, _ := h.run(t, 3)
	defer stop()

	testsupport.Publish(t, h.ch, testQueue,
		mutateGolden(t, func(m map[string]any) { m["mode"] = "teleport" }))
	h.waitForStatus(t, goldenJobID, store.StatusFailed)

	_, errMsg, _ := h.api.JobStatus(t, goldenJobID)
	if errMsg == "" {
		t.Error("job failed with no message; a caller has nothing to read")
	}

	time.Sleep(500 * time.Millisecond)
	if got := testsupport.QueueDepth(t, h.ch, testQueue); got != 0 {
		t.Errorf("queue depth = %d, want 0 — a permanent failure must not requeue", got)
	}
	if got := testsupport.QueueDepth(t, h.ch, testDeadLetter); got != 0 {
		t.Errorf("dead-letter depth = %d, want 0", got)
	}
}

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
	h.api.SeedRoutingJob(goldenJobID)

	stop, _ := h.run(t, maxRetries)
	defer stop()

	testsupport.Publish(t, h.ch, testQueue, goldenMessage(t))
	h.waitForStatus(t, goldenJobID, store.StatusFailed)

	testsupport.WaitFor(t, 5*time.Second, "the message to reach the dead-letter queue", func() bool {
		return testsupport.QueueDepth(t, h.ch, testDeadLetter) == 1
	})
	if got := attempts.Load(); got != maxRetries+1 {
		t.Errorf("routing attempts = %d, want %d (first delivery + %d retries)", got, maxRetries+1, maxRetries)
	}
	if got := testsupport.QueueDepth(t, h.ch, testQueue); got != 0 {
		t.Errorf("live queue depth = %d, want 0 — the message should have moved on", got)
	}
	if got := h.metrics.DeadLetterCount(); got != 1 {
		t.Errorf("dead-letter counter = %d, want 1", got)
	}

	_, errMsg, _ := h.api.JobStatus(t, goldenJobID)
	if errMsg == "" {
		t.Error("job failed with no message after exhausting retries")
	}
}

func TestEndToEnd_transientFailureThatRecoversSucceeds(t *testing.T) {
	synthetic := valhalla.NewSyntheticClient()
	var attempts atomic.Int32
	client := &valhalla.StubClient{
		MatrixResp: &valhalla.MatrixResponse{SourcesToTargets: [][]valhalla.MatrixCell{{{}, {}}}},
		IsochroneFn: func(r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
			if attempts.Add(1) == 1 {
				return nil, valhalla.ErrUpstream
			}
			return synthetic.Isochrone(context.Background(), r)
		},
	}

	h := newHarness(t, client)
	h.api.SeedRoutingJob(goldenJobID)

	stop, _ := h.run(t, 3)
	defer stop()

	testsupport.Publish(t, h.ch, testQueue, goldenMessage(t))
	h.waitForStatus(t, goldenJobID, store.StatusSucceeded)
}

func TestEndToEnd_workerKilledMidChainRedelivers(t *testing.T) {
	h := newHarness(t, nil)
	h.api.SeedRoutingJob(goldenJobID)

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

	killFirst()

	if status, _, _ := h.api.JobStatus(t, goldenJobID); status == store.StatusSucceeded {
		t.Fatal("the job completed before the worker was killed; the test proves nothing")
	}

	testsupport.WaitFor(t, 15*time.Second, "the unacked message to be redelivered to the queue", func() bool {
		return testsupport.QueueDepth(t, h.ch, testQueue) == 1
	})

	h.client = valhalla.NewSyntheticClient()
	stopSecond, _ := h.run(t, 3)
	defer stopSecond()

	h.waitForStatus(t, goldenJobID, store.StatusSucceeded)

	_, _, result := h.api.JobStatus(t, goldenJobID)
	if len(result) == 0 {
		t.Error("job succeeded with no result attached")
	}
	if got := testsupport.QueueDepth(t, h.ch, testDeadLetter); got != 0 {
		t.Errorf("dead-letter depth = %d, want 0 — a crash is not a job failure", got)
	}
}

func TestEndToEnd_messageForAMissingJobIsDiscarded(t *testing.T) {
	h := newHarness(t, valhalla.NewSyntheticClient())

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

func transitAccessClient(speedKmH float64) *countingClient {
	synth := &valhalla.SyntheticClient{SpeedKmH: map[valhalla.Costing]float64{
		valhalla.CostingMultimodal: speedKmH,
	}}
	return &countingClient{Client: &valhalla.StubClient{
		IsochroneFn: func(r valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
			return synth.Isochrone(context.Background(), r)
		},
		RouteFn: func(r valhalla.RouteRequest) (*valhalla.RouteResponse, error) {
			if r.Destination.Lat < 36 {
				return nil, valhalla.ErrBadRequest
			}
			return &valhalla.RouteResponse{
				Shape:      []valhalla.LatLng{r.Origin, r.Destination},
				Time:       600,
				DistanceKm: 10,
			}, nil
		},
	}}
}

func transitJobBody(t *testing.T, mut func(m map[string]any)) []byte {
	t.Helper()
	return mutateGolden(t, func(m map[string]any) {
		m["mode"] = "transit"
		m["budget_mins"] = 90
		if mut != nil {
			mut(m)
		}
	})
}

func TestEndToEnd_walkShapedTransitIsochroneIsNotCached(t *testing.T) {
	client := transitAccessClient(geo.WalkSpeedKmH)
	h := newHarness(t, client)
	h.api.SeedRoutingJob(goldenJobID)

	stop, _ := h.run(t, 3)
	defer stop()

	testsupport.Publish(t, h.ch, testQueue, transitJobBody(t, nil))
	h.waitForStatus(t, goldenJobID, store.StatusSucceeded)

	_, errMsg, result := h.api.JobStatus(t, goldenJobID)
	if errMsg != "" {
		t.Errorf("error = %q, want empty — a walk-shaped transit surface must still complete the job", errMsg)
	}
	if len(features(t, result)) == 0 {
		t.Error("succeeded with no features")
	}
	if got := h.api.IsochroneCacheCount(goldenCompileID); got != 0 {
		t.Errorf("cache holds %d rows, want 0 transit-keyed writes for a walk-shaped polygon", got)
	}
}

func TestEndToEnd_genuineTransitIsochroneCachesAndReuses(t *testing.T) {
	const secondJobID = "8b1731ff-8b86-d011-b42d-00c04fc964ff"

	client := transitAccessClient(geo.TransitSpeedKmH)
	h := newHarness(t, client)
	h.api.SeedRoutingJob(goldenJobID)

	stop, _ := h.run(t, 3)
	defer stop()

	testsupport.Publish(t, h.ch, testQueue, transitJobBody(t, nil))
	h.waitForStatus(t, goldenJobID, store.StatusSucceeded)
	coldCalls := client.isochroneCalls.Load()

	cached := h.api.IsochroneCacheCount(goldenCompileID)
	if cached == 0 {
		t.Fatal("the completed transit job cached no egress polygons")
	}
	if want := int(coldCalls) - 1; cached != want {
		t.Errorf("cached %d polygons against %d isochrone calls, want %d (all but the origin)", cached, coldCalls, want)
	}

	h.api.SeedRoutingJob(secondJobID)
	client.isochroneCalls.Store(0)
	testsupport.Publish(t, h.ch, testQueue, transitJobBody(t, func(m map[string]any) {
		m["routing_job_id"] = secondJobID
	}))
	h.waitForStatus(t, secondJobID, store.StatusSucceeded)
	warmCalls := client.isochroneCalls.Load()

	if warmCalls >= coldCalls {
		t.Errorf("warm job made %d isochrone calls against a cold %d; the cache saved nothing", warmCalls, coldCalls)
	}
	if warmCalls != 1 {
		t.Errorf("warm job made %d isochrone calls, want 1 (the origin)", warmCalls)
	}
	if got := h.api.IsochroneCacheCount(goldenCompileID); got != cached {
		t.Errorf("cache holds %d rows after the warm job, want the %d the cold one left", got, cached)
	}
}
