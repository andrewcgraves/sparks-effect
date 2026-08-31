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

// nullStore accepts every transition and remembers nothing. What the consumer
// does with a delivery does not depend on the row behind it, and the row itself
// is settled in handler_test.go and against a real database.
type nullStore struct{}

func (nullStore) MarkRunning(context.Context, string) error                    { return nil }
func (nullStore) MarkSucceeded(context.Context, string, json.RawMessage) error { return nil }
func (nullStore) MarkFailed(context.Context, string, string) error             { return nil }

// The trace rides in the message body, which only the handler decodes, so
// without the Result carrying it back out everything the consumer says about
// settling a delivery — requeued, dead-lettered, acked — would fall outside the
// trace the job itself logged under (SPA-227).
//
// A transient failure with no broker connection is the shortest path through
// that logging: the handler reports the failure, the consumer says it is
// requeueing, and the requeue itself then fails for want of a channel. Three
// lines from two components, all of which have to name the same trace.
func TestConsumerHandle_settlementLoggingAdoptsTheMessagesTrace(t *testing.T) {
	const wantTrace = "3fa85f64-5717-4562-b3fc-2c963f66afa6"

	body, err := os.ReadFile(filepath.Join("..", "routing", "testdata", "message.golden.json"))
	if err != nil {
		t.Fatalf("reading golden fixture: %v", err)
	}

	var buf bytes.Buffer
	log := logger.New(&buf, false)
	handler := NewHandler(nullStore{}, &valhalla.StubClient{IsochroneErr: valhalla.ErrUpstream},
		valhalla.DefaultLimits(), isochrone.EgressCache{}, log)

	// No broker: the delivery is never dialled, so nothing here talks to one.
	NewConsumer("amqp://unused", "q", "q.dead", 3, handler, log).
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
