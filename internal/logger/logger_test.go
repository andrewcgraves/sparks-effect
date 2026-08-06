package logger_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/andrewcgraves/sparks-effect-routing-worker/internal/logger"
)

func TestDebug_noopWhenOff(t *testing.T) {
	var buf bytes.Buffer
	lg := logger.New(&buf, false)
	lg.Debug(context.Background(), "should not appear", "detail", "here")
	if buf.Len() != 0 {
		t.Errorf("Debug wrote output when debug=false: %q", buf.String())
	}
}

func TestDebug_writesWhenOn(t *testing.T) {
	var buf bytes.Buffer
	lg := logger.New(&buf, true)
	lg.Debug(context.Background(), "hello", "who", "world")
	out := buf.String()
	if !strings.Contains(out, `"hello"`) {
		t.Errorf("Debug output missing expected message: %q", out)
	}
	if !strings.Contains(out, `"who":"world"`) {
		t.Errorf("Debug output missing expected field: %q", out)
	}
	if !strings.Contains(out, `"level":"DEBUG"`) {
		t.Errorf("Debug output missing DEBUG level: %q", out)
	}
}

func TestDiscard_noOutput(t *testing.T) {
	lg := logger.Discard()
	lg.Debug(context.Background(), "no output")
	lg.Info(context.Background(), "also no output")
}

func TestInfo_alwaysWrites(t *testing.T) {
	var buf bytes.Buffer
	lg := logger.New(&buf, false)
	lg.Info(context.Background(), "always visible", "job_id", "abc123")
	out := buf.String()
	if !strings.Contains(out, `"always visible"`) {
		t.Errorf("Info did not write when debug=false: %q", out)
	}
	if !strings.Contains(out, `"level":"INFO"`) {
		t.Errorf("Info output missing INFO level: %q", out)
	}
}

func TestOutput_isValidJSON(t *testing.T) {
	var buf bytes.Buffer
	lg := logger.New(&buf, true)
	lg.Info(context.Background(), "job outcome", "job_id", "abc123", "outcome", "succeeded", "count", 3)

	var line map[string]any
	if err := json.Unmarshal(buf.Bytes(), &line); err != nil {
		t.Fatalf("log line is not valid JSON: %v\noutput: %s", err, buf.String())
	}
	if line["msg"] != "job outcome" {
		t.Errorf("msg = %v, want %q", line["msg"], "job outcome")
	}
	if line["job_id"] != "abc123" {
		t.Errorf("job_id = %v, want %q", line["job_id"], "abc123")
	}
}

func TestWarn_and_Error_alwaysWrite(t *testing.T) {
	var buf bytes.Buffer
	lg := logger.New(&buf, false)

	lg.Warn(context.Background(), "degraded", "reason", "cache unavailable")
	lg.Error(context.Background(), "failed", "err", "boom")

	out := buf.String()
	if !strings.Contains(out, `"level":"WARN"`) {
		t.Errorf("Warn output missing WARN level: %q", out)
	}
	if !strings.Contains(out, `"level":"ERROR"`) {
		t.Errorf("Error output missing ERROR level: %q", out)
	}
}

func TestWithTraceID_attachesToEveryRecord(t *testing.T) {
	var buf bytes.Buffer
	lg := logger.New(&buf, true)
	ctx := logger.WithTraceID(context.Background(), "trace-abc-123")

	lg.Info(ctx, "job received", "job_id", "job-1")
	lg.Debug(ctx, "chain step")

	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err != nil {
			t.Fatalf("log line is not valid JSON: %v\nline: %s", err, line)
		}
		if parsed["trace_id"] != "trace-abc-123" {
			t.Errorf("trace_id = %v, want %q in line %s", parsed["trace_id"], "trace-abc-123", line)
		}
	}
}

func TestWithTraceID_emptyIDOmitsField(t *testing.T) {
	var buf bytes.Buffer
	lg := logger.New(&buf, false)
	ctx := logger.WithTraceID(context.Background(), "")

	lg.Info(ctx, "no trace here")

	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("log line is not valid JSON: %v", err)
	}
	if _, ok := parsed["trace_id"]; ok {
		t.Errorf("trace_id present for empty id: %v", parsed)
	}
}

func TestTraceID_roundTrips(t *testing.T) {
	ctx := logger.WithTraceID(context.Background(), "trace-xyz")
	id, ok := logger.TraceID(ctx)
	if !ok || id != "trace-xyz" {
		t.Errorf("TraceID = (%q, %v), want (\"trace-xyz\", true)", id, ok)
	}

	if _, ok := logger.TraceID(context.Background()); ok {
		t.Error("TraceID reported ok=true on a context with no trace id")
	}
}
