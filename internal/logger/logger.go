package logger

import (
	"context"
	"io"
	"log/slog"
	"os"
)

type Logger struct {
	sl *slog.Logger
}

func New(out io.Writer, debug bool) *Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	handler := traceHandler{Handler: slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level})}
	return &Logger{sl: slog.New(handler)}
}

func Default(debug bool) *Logger {
	return New(os.Stderr, debug)
}

func Discard() *Logger {
	return New(io.Discard, false)
}

func (lg *Logger) Debug(ctx context.Context, msg string, args ...any) {
	lg.sl.Log(ctx, slog.LevelDebug, msg, args...)
}

func (lg *Logger) Info(ctx context.Context, msg string, args ...any) {
	lg.sl.Log(ctx, slog.LevelInfo, msg, args...)
}

func (lg *Logger) Warn(ctx context.Context, msg string, args ...any) {
	lg.sl.Log(ctx, slog.LevelWarn, msg, args...)
}

func (lg *Logger) Error(ctx context.Context, msg string, args ...any) {
	lg.sl.Log(ctx, slog.LevelError, msg, args...)
}

type traceIDKey struct{}

func WithTraceID(ctx context.Context, traceID string) context.Context {
	if traceID == "" {
		return ctx
	}
	return context.WithValue(ctx, traceIDKey{}, traceID)
}

func TraceID(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(traceIDKey{}).(string)
	return id, ok
}

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
