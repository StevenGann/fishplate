// Package logging builds the process logger: JSON lines on stderr via log/slog.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// ParseLevel maps a config level name to a slog.Level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %q", s)
}

// New returns a JSON logger writing to w at the given level. Only the "json"
// format exists; any other value is an error rather than a silent fallback.
func New(w io.Writer, level, format string) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	if format != "json" {
		return nil, fmt.Errorf("unsupported log format %q", format)
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})), nil
}

type ctxKey struct{}

// WithRequestID returns a context carrying a request id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// RequestID returns the request id in ctx, or "".
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)
	return id
}

// FromContext returns logger with request_id attached when ctx carries one.
func FromContext(ctx context.Context, logger *slog.Logger) *slog.Logger {
	if id := RequestID(ctx); id != "" {
		return logger.With("request_id", id)
	}
	return logger
}
