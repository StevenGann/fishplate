// Package web serves Fishplate's HTTP endpoints. Phase 0a exposes health,
// readiness and metrics only.
package web

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
)

// Server holds the HTTP handler and the readiness state.
type Server struct {
	logger  *slog.Logger
	metrics *Metrics
	ready   atomic.Bool
	handler http.Handler
}

// New builds the handler tree. The server starts not ready; call SetReady
// once the listener is accepting connections.
func New(logger *slog.Logger, metrics *Metrics) *Server {
	s := &Server{logger: logger, metrics: metrics}
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

// readyz reports whether the process is serving. Phase 0a has no database or
// connectors to check; 0b adds a real write to the store.
func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if !s.ready.Load() {
		writeProblem(w, r, Problem{
			Status: http.StatusServiceUnavailable,
			Detail: "server is not accepting traffic",
		})
		return
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
