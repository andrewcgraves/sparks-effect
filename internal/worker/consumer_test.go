package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	amqp "github.com/rabbitmq/amqp091-go"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/isochrone"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/metrics"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/store"
	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/valhalla"
)

func TestUserinfo(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{"simple", "amqp://user:pass@host:5672/", "user:pass"},
		{"password contains @", "amqp://user:p@ss@host:5672/", "user:p@ss"},
		{"no credentials", "amqp://host:5672/", ""},
		{"no scheme", "not-a-url", ""},
		{"no vhost path", "amqp://user:pass@host:5672", "user:pass"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := userinfo(tt.url); got != tt.want {
				t.Errorf("userinfo(%q) = %q, want %q", tt.url, got, tt.want)
			}
		})
	}
}

func TestRedactDialErr(t *testing.T) {
	rawURL := `amqp://user:p%ssword@rabbitmq.svc.cluster.local:5672/`
	underlying := errors.New(`parse "` + rawURL + `": invalid URL escape "%ss"`)

	err := redactDialErr(rawURL, underlying)

	if strings.Contains(err.Error(), "p%ssword") {
		t.Fatalf("redactDialErr leaked the credential: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("redactDialErr did not redact anything: %v", err)
	}
}

func TestRedactDialErrNoCredentials(t *testing.T) {
	underlying := errors.New("connection refused")
	err := redactDialErr("amqp://host:5672/", underlying)

	if err.Error() != underlying.Error() {
		t.Fatalf("expected error unchanged when URL has no credentials, got %q", err.Error())
	}
}

type nullStore struct{}

func (nullStore) MarkRunning(context.Context, string) error                    { return nil }
func (nullStore) MarkSucceeded(context.Context, string, store.Succeeded) error { return nil }
func (nullStore) MarkFailed(context.Context, string, string) error             { return nil }

func TestConsumerHandle_settlementLoggingAdoptsTheMessagesTrace(t *testing.T) {
	const wantTrace = "3fa85f64-5717-4562-b3fc-2c963f66afa6"

	body, err := os.ReadFile(filepath.Join("..", "routing", "testdata", "message.golden.json"))
	if err != nil {
		t.Fatalf("reading golden fixture: %v", err)
	}

	var buf bytes.Buffer
	log := logger.New(&buf, false)
	handler := NewHandler(nullStore{}, &valhalla.StubClient{IsochroneErr: valhalla.ErrUpstream},
		valhalla.DefaultLimits(), isochrone.EgressCache{}, log, nil)

	NewConsumer("amqp://unused", "q", "q.dead", 3, handler, log, nil).
		handle(context.Background(), amqp.Delivery{Body: body})

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected the handler and the consumer both to log, got: %q", buf.String())
	}
	for i, line := range lines {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("log line is not valid JSON: %v\nline: %s", err, line)
		}
		if parsed["trace_id"] != wantTrace {
			t.Errorf("log line %d has trace_id %v, want %q\nline: %s", i, parsed["trace_id"], wantTrace, line)
		}
	}
}

func TestConsumerHandle_countsDeadLettersWhenRetriesAreExhausted(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "routing", "testdata", "message.golden.json"))
	if err != nil {
		t.Fatalf("reading golden fixture: %v", err)
	}

	m := &metrics.Set{}
	handler := NewHandler(nullStore{}, &valhalla.StubClient{IsochroneErr: valhalla.ErrUpstream},
		valhalla.DefaultLimits(), isochrone.EgressCache{}, logger.Discard(), m)

	NewConsumer("amqp://unused", "q", "q.dead", 0, handler, logger.Discard(), m).
		handle(context.Background(), amqp.Delivery{Body: body})
	if got := m.DeadLetterCount(); got != 1 {
		t.Errorf("dead-letter count = %d, want 1", got)
	}
}

func TestTraceIDFromDelivery(t *testing.T) {
	tests := []struct {
		name    string
		headers amqp.Table
		want    string
	}{
		{"present", amqp.Table{traceIDHeader: "trace-abc-123"}, "trace-abc-123"},
		{"absent", amqp.Table{}, ""},
		{"nil headers", nil, ""},
		{"non-string value", amqp.Table{traceIDHeader: int32(7)}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := amqp.Delivery{Headers: tt.headers}
			if got := traceIDFromDelivery(d); got != tt.want {
				t.Errorf("traceIDFromDelivery(%+v) = %q, want %q", tt.headers, got, tt.want)
			}
		})
	}
}

type recordingTracker struct{ events []string }

func (r *recordingTracker) JobStarted()  { r.events = append(r.events, "started") }
func (r *recordingTracker) JobFinished() { r.events = append(r.events, "finished") }

func TestConsumerHandle_bracketsTheJobForTheLivenessProbe(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "routing", "testdata", "message.golden.json"))
	if err != nil {
		t.Fatalf("reading golden fixture: %v", err)
	}

	tracker := &recordingTracker{}
	client := &valhalla.StubClient{
		IsochroneFn: func(valhalla.IsochroneRequest) (*valhalla.IsochroneResponse, error) {
			tracker.events = append(tracker.events, "routing")
			return nil, valhalla.ErrBadRequest
		},
	}
	handler := NewHandler(nullStore{}, client, valhalla.DefaultLimits(), isochrone.EgressCache{}, logger.Discard(), nil)

	consumer := NewConsumer("amqp://unused", "q", "q.dead", 3, handler, logger.Discard(), nil)
	consumer.TrackJobs(tracker)
	consumer.handle(context.Background(), amqp.Delivery{Body: body})

	if len(tracker.events) < 3 || tracker.events[0] != "started" || tracker.events[len(tracker.events)-1] != "finished" {
		t.Errorf("events = %v, want started … routing … finished", tracker.events)
	}
}

func TestConsumerIsNotConnectedBeforeRun(t *testing.T) {
	c := NewConsumer("amqp://unused", "q", "q.dead", 3, nil, logger.Discard(), nil)
	if c.Connected() {
		t.Error("a consumer that has never dialled reports Connected")
	}
}
