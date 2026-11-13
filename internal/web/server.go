// Package web serves Fishplate's HTTP endpoints: health, readiness and
// metrics.
package web

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

// Server holds the HTTP handler and the readiness state.
type Server struct {
	logger  *slog.Logger
	metrics *Metrics
	ready   atomic.Bool
	check   func(context.Context) error
	handler http.Handler
}

// ReadyTimeout bounds one readiness check, so a probe gets an answer before
// a typical kubelet probe timeout even when the database lock is held.
const ReadyTimeout = 3 * time.Second

// Option configures a Server.
type Option func(*Server)

// WithReadinessCheck makes /readyz run check on every request and answer 503
// when it fails.
func WithReadinessCheck(check func(context.Context) error) Option {
	return func(s *Server) { s.check = check }
}

// New builds the handler tree. The server starts not ready; call SetReady
// once the listener is accepting connections.
func New(logger *slog.Logger, metrics *Metrics, opts ...Option) *Server {
	s := &Server{logger: logger, metrics: metrics}
	for _, o := range opts {
		o(s)
	}
	mux := http.NewServeMux()
	s.handle(mux, "/healthz", s.healthz, http.MethodGet, http.MethodHead)
	s.handle(mux, "/readyz", s.readyz, http.MethodGet, http.MethodHead)
	s.handle(mux, "/metrics", s.metricsHandler, http.MethodGet, http.MethodHead)
	mux.HandleFunc("/", s.notFound)
	s.handler = middleware(mux, logger, metrics)
	return s
}

// Handler returns the root handler including middleware.
func (s *Server) Handler() http.Handler { return s.handler }

// SetReady marks the process as serving (true) or draining (false).
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

// handle registers an exact path and answers other methods with a 405
// problem rather than ServeMux's plain-text default.
func (s *Server) handle(mux *http.ServeMux, path string, h http.HandlerFunc, methods ...string) {
	allowed := map[string]bool{}
	for _, m := range methods {
		allowed[m] = true
	}
	sorted := append([]string(nil), methods...)
	sort.Strings(sorted)
	allow := strings.Join(sorted, ", ")
	mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
		if !allowed[r.Method] {
			w.Header().Set("Allow", allow)
			writeProblem(w, r, Problem{
				Status: http.StatusMethodNotAllowed,
				Detail: "method " + r.Method + " is not allowed; use " + allow,
			})
			return
		}
		h(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// readyz reports whether the process should receive traffic: it must be
// serving, and the readiness check (in 0b, a committed write to the store)
// must succeed on this request. Nothing is cached; a database that becomes
// unwritable turns the next probe into a 503.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() {
		writeProblem(w, r, Problem{
			Status: http.StatusServiceUnavailable,
			Detail: "server is not accepting traffic",
		})
		return
	}
	if check := s.check; check != nil {
		ctx, cancel := context.WithTimeout(r.Context(), ReadyTimeout)
		defer cancel()
		if err := check(ctx); err != nil {
			s.logger.WarnContext(r.Context(), "readiness check failed",
				"request_id", requestIDFrom(r.Context()), "error", err)
			writeProblem(w, r, Problem{
				Status: http.StatusServiceUnavailable,
				Detail: "database is not writable: " + err.Error(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *Server) metricsHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", metricsContentType)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = s.metrics.WriteTo(w)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, Problem{
		Status: http.StatusNotFound,
		Detail: "no resource at " + r.URL.Path,
	})
}
