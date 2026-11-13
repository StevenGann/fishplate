package web

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/StevenGann/fishplate/internal/logging"
)

// RequestIDHeader carries the request id in both directions.
const RequestIDHeader = "X-Request-Id"

func requestIDFrom(ctx context.Context) string { return logging.RequestID(ctx) }

// newRequestID returns 16 random bytes, hex encoded.
func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand does not fail on supported platforms; if it does, the
		// process has bigger problems than a missing id.
		panic("crypto/rand: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}

// validInboundID accepts a caller-supplied id only if it is short and made of
// safe characters, so it can be echoed and logged without escaping concerns.
func validInboundID(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.':
		default:
			return false
		}
	}
	return true
}

// statusRecorder captures the status code and byte count of a response.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// middleware wraps next with, outermost first: request id assignment,
// access logging and metrics, and panic recovery.
func middleware(next http.Handler, logger *slog.Logger, m *Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		id := r.Header.Get(RequestIDHeader)
		if !validInboundID(id) {
			id = newRequestID()
		}
		w.Header().Set(RequestIDHeader, id)
		r = r.WithContext(logging.WithRequestID(r.Context(), id))

		rec := &statusRecorder{ResponseWriter: w}
		serveRecovered(rec, r, next, logger)

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		// ServeMux records the matched pattern on r; anything unmatched shares
		// one label so arbitrary paths cannot inflate metric cardinality.
		route := r.Pattern
		if route == "" {
			route = "unmatched"
		}
		m.observe(r.Method, route, status)

		logging.FromContext(r.Context(), logger).LogAttrs(r.Context(), slog.LevelInfo, "http request",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("route", route),
			slog.Int("status", status),
			slog.Int64("bytes", rec.bytes),
			slog.Float64("duration_ms", float64(time.Since(start).Microseconds())/1000),
			slog.String("remote_addr", r.RemoteAddr),
		)
	})
}

// serveRecovered runs next and turns a panic into a 500 problem response.
func serveRecovered(w *statusRecorder, r *http.Request, next http.Handler, logger *slog.Logger) {
	defer func() {
		v := recover()
		if v == nil {
			return
		}
		if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
			panic(v) // net/http's documented way to abort; let the server handle it
		}
		logging.FromContext(r.Context(), logger).Error("panic in handler",
			slog.Any("panic", v),
			slog.String("stack", string(debug.Stack())),
		)
		if w.status != 0 {
			// Headers are already on the wire; the best we can do is stop.
			// The access log still records the original status.
			return
		}
		writeProblem(w, r, Problem{
			Status: http.StatusInternalServerError,
			Detail: "the server encountered an internal error; quote the request id when reporting it",
		})
	}()
	next.ServeHTTP(w, r)
}
