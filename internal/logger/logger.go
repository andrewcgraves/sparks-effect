// Package logger provides structured JSON logging for the routing worker.
//
// It wraps the standard library's log/slog with a JSON handler so every line
// this worker writes is a self-describing JSON object: consistent field
// names, timestamps, and levels that a log shipper (Grafana Alloy, eventually
// — see SPA-193) can parse without a custom grammar. Debug-level detail is
// gated behind a level set at construction time, driven by the LOG_LEVEL
// environment variable (see internal/config), so it can be switched on in a
// running environment without a code change.
//
// Every logging call takes a context.Context. That is not decoration: a
// trace_id carried on the context (see WithTraceID) is attached to the
// record automatically, so a job's whole log trail — from the moment it is
// pulled off the queue, through every routing decision, to the outcome
// recorded on the delivery — can be filtered on one value without every call
// site threading it through by hand.
package logger

import (
	"context"
	"io"
	"log/slog"
	"os"
)

// Logger writes structured JSON log lines, gating debug-level detail behind
// the level it was constructed with.
type Logger struct {
	sl *slog.Logger
}

// New creates a Logger that writes JSON to out. debug=true admits Debug
// records; debug=false discards them, matching the LOG_LEVEL=debug /
// LOG_LEVEL=info distinction operators set at the environment level.
func New(out io.Writer, debug bool) *Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	handler := traceHandler{Handler: slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level})}
	return &Logger{sl: slog.New(handler)}
}

// Default creates a Logger writing JSON to stderr, which is where a
// containerised worker's logs are collected from.
func Default(debug bool) *Logger {
	return New(os.Stderr, debug)
}

// Discard returns a Logger that writes nothing, for tests and for any caller
// that is wired for logging but has nothing useful to say it to.
func Discard() *Logger {
	return New(io.Discard, false)
}

// Debug records detail useful for debugging a specific job or decision —
// cache hits, node counts, per-candidate routing detail. Emitted only when
// the logger was built with debug=true; otherwise the call costs a level
// check and nothing more, so call sites do not need to guard it themselves.
//
// args are alternating key-value pairs, per slog convention, e.g.:
//
//	log.Debug(ctx, "chain: egress cache", "hits", hits, "misses", misses)
func (lg *Logger) Debug(ctx context.Context, msg string, args ...any) {
	lg.sl.Log(ctx, slog.LevelDebug, msg, args...)
}

// Info records a routing decision or lifecycle event worth keeping regardless
// of level: a job's outcome, a fallback taken, a retry, a shutdown. Always
// emitted.
func (lg *Logger) Info(ctx context.Context, msg string, args ...any) {
	lg.sl.Log(ctx, slog.LevelInfo, msg, args...)
}

// Warn records a condition that degrades the worker without failing the
// current job outright — a cache that could not be read, an optional call
// that came back empty.
func (lg *Logger) Warn(ctx context.Context, msg string, args ...any) {
	lg.sl.Log(ctx, slog.LevelWarn, msg, args...)
}

// Error records a failure: a job that could not be completed, a write that
// did not take, a connection that dropped.
func (lg *Logger) Error(ctx context.Context, msg string, args ...any) {
	lg.sl.Log(ctx, slog.LevelError, msg, args...)
}

// traceIDKey is an unexported type so WithTraceID's value cannot collide with
// a key set by another package sharing the same context.
type traceIDKey struct{}

// WithTraceID returns a context carrying traceID for every log record written
// through it. An empty traceID returns ctx unchanged rather than storing an
// empty string, so a job that arrived with no trace attribute — an older
// publisher, a message sent by hand — simply omits the field instead of
// logging trace_id="".
func WithTraceID(ctx context.Context, traceID string) context.Context {
	if traceID == "" {
		return ctx
	}
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

// TraceID reads back the trace id a context was tagged with by WithTraceID,
// for the rare caller that needs the value itself rather than just wanting it
// logged.
func TraceID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(traceIDKey{}).(string)
	return id, ok
}

// traceHandler wraps an slog.Handler and adds a trace_id attribute to every
// record whose context carries one, so trace propagation is a property of the
// context alone — no call site has to remember to pass the id explicitly.
type traceHandler struct {
	slog.Handler
}

func (h traceHandler) Handle(ctx context.Context, r slog.Record) error {
	if id, ok := TraceID(ctx); ok {
		r.AddAttrs(slog.String("trace_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceHandler{Handler: h.Handler.WithAttrs(attrs)}
}

func (h traceHandler) WithGroup(name string) slog.Handler {
	return traceHandler{Handler: h.Handler.WithGroup(name)}
}
