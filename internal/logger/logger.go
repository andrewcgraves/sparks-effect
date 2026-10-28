package logger

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
)

type Logger struct {
	sl *slog.Logger
}

func New(out io.Writer, debug bool, also ...slog.Handler) *Logger {
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	var handler slog.Handler = slog.NewJSONHandler(out, &slog.HandlerOptions{Level: level})
	if copies := nonNil(also); len(copies) > 0 {
		// stderr stays the primary sink — Railway's log viewer reads it — and
		// an exporter is a copy of it, not a replacement. The level gate sits
		// on the tee because the OTLP bridge accepts every level itself.
		handler = teeHandler{level: level, handlers: append([]slog.Handler{handler}, copies...)}
	}
	return &Logger{sl: slog.New(traceHandler{Handler: handler})}
}

func Default(debug bool, also ...slog.Handler) *Logger {
	return New(os.Stderr, debug, also...)
}

func nonNil(handlers []slog.Handler) []slog.Handler {
	var out []slog.Handler
	for _, h := range handlers {
		if h != nil {
			out = append(out, h)
		}
	}
	return out
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

type teeHandler struct {
	level    slog.Level
	handlers []slog.Handler
}

func (h teeHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var errs []error
	for _, handler := range h.handlers {
		if handler.Enabled(ctx, r.Level) {
			errs = append(errs, handler.Handle(ctx, r))
		}
	}
	return errors.Join(errs...)
}

func (h teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		out[i] = handler.WithAttrs(attrs)
	}
	return teeHandler{level: h.level, handlers: out}
}

func (h teeHandler) WithGroup(name string) slog.Handler {
	out := make([]slog.Handler, len(h.handlers))
	for i, handler := range h.handlers {
		out[i] = handler.WithGroup(name)
	}
	return teeHandler{level: h.level, handlers: out}
}
