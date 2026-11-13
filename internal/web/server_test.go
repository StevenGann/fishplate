package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) (*Server, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	s := New(logger, NewMetrics("v1.2.3", "abc1234"))
	s.SetReady(true)
	return s, &logs
}

func do(h http.Handler, method, path string, hdr ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestEndpoints(t *testing.T) {
	tests := []struct {
		name        string
		method      string
		path        string
		ready       bool
		wantStatus  int
		wantType    string
		wantBody    string // exact body (trimmed) when non-empty
		wantContain []string
	}{
		{name: "healthz", method: "GET", path: "/healthz", ready: true, wantStatus: 200, wantType: "application/json", wantBody: `{"status":"ok"}`},
		{name: "healthz when not ready", method: "GET", path: "/healthz", ready: false, wantStatus: 200, wantBody: `{"status":"ok"}`},
		{name: "healthz HEAD", method: "HEAD", path: "/healthz", ready: true, wantStatus: 200},
		{name: "readyz ready", method: "GET", path: "/readyz", ready: true, wantStatus: 200, wantType: "application/json", wantBody: `{"status":"ready"}`},
		{name: "readyz not ready", method: "GET", path: "/readyz", ready: false, wantStatus: 503, wantType: ProblemContentType, wantContain: []string{`"status":503`}},
		{name: "metrics", method: "GET", path: "/metrics", ready: true, wantStatus: 200, wantType: "text/plain; version=0.0.4",
			wantContain: []string{
				"# TYPE fishplate_build_info gauge",
				`fishplate_build_info{version="v1.2.3",commit="abc1234"} 1`,
				"# TYPE fishplate_http_requests_total counter",
			}},
		{name: "wrong method", method: "POST", path: "/healthz", ready: true, wantStatus: 405, wantType: ProblemContentType, wantContain: []string{`"title":"Method Not Allowed"`}},
		{name: "not found", method: "GET", path: "/nope", ready: true, wantStatus: 404, wantType: ProblemContentType, wantContain: []string{`"type":"about:blank"`, `"instance":"/nope"`}},
		{name: "healthz subpath is not healthz", method: "GET", path: "/healthz/x", ready: true, wantStatus: 404, wantType: ProblemContentType},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newTestServer(t)
			s.SetReady(tt.ready)
			rec := do(s.Handler(), tt.method, tt.path)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tt.wantStatus, rec.Body)
			}
			if tt.wantType != "" && !strings.HasPrefix(rec.Header().Get("Content-Type"), tt.wantType) {
				t.Errorf("Content-Type = %q, want prefix %q", rec.Header().Get("Content-Type"), tt.wantType)
			}
			body := rec.Body.String()
			if tt.wantBody != "" && strings.TrimSpace(body) != tt.wantBody {
				t.Errorf("body = %q, want %q", body, tt.wantBody)
			}
			for _, c := range tt.wantContain {
				if !strings.Contains(body, c) {
					t.Errorf("body missing %q:\n%s", c, body)
				}
			}
			if rec.Header().Get(RequestIDHeader) == "" {
				t.Error("missing X-Request-Id")
			}
		})
	}
}

func TestReadinessCheck(t *testing.T) {
	var logs bytes.Buffer
	var fail error
	calls := 0
	s := New(slog.New(slog.NewJSONHandler(&logs, nil)), NewMetrics("v", "c"),
		WithReadinessCheck(func(ctx context.Context) error {
			calls++
			if _, ok := ctx.Deadline(); !ok {
				t.Error("readiness check called without a deadline")
			}
			return fail
		}))
	s.SetReady(true)

	if rec := do(s.Handler(), "GET", "/readyz"); rec.Code != 200 {
		t.Fatalf("healthy check: status %d", rec.Code)
	}
	fail = errors.New("attempt to write a readonly database")
	rec := do(s.Handler(), "GET", "/readyz")
	if rec.Code != 503 || !strings.HasPrefix(rec.Header().Get("Content-Type"), ProblemContentType) ||
		!strings.Contains(rec.Body.String(), "readonly database") {
		t.Fatalf("failing check: %d %s %s", rec.Code, rec.Header().Get("Content-Type"), rec.Body)
	}
	if !strings.Contains(logs.String(), "readiness check failed") {
		t.Errorf("failure not logged: %s", logs.String())
	}
	fail = nil
	if rec := do(s.Handler(), "GET", "/readyz"); rec.Code != 200 {
		t.Fatalf("recovered check: status %d", rec.Code)
	}
	if calls != 3 {
		t.Fatalf("check ran %d times, want once per request (3)", calls)
	}

	s.SetReady(false)
	if rec := do(s.Handler(), "GET", "/readyz"); rec.Code != 503 || calls != 3 {
		t.Fatalf("not serving: status %d, calls %d", rec.Code, calls)
	}
}

func TestMethodNotAllowedSetsAllow(t *testing.T) {
	s, _ := newTestServer(t)
	rec := do(s.Handler(), "DELETE", "/metrics")
	if got := rec.Header().Get("Allow"); got != "GET, HEAD" {
		t.Fatalf("Allow = %q", got)
	}
}

func TestRequestCounter(t *testing.T) {
	s, _ := newTestServer(t)
	h := s.Handler()
	for i := 0; i < 3; i++ {
		do(h, "GET", "/healthz")
	}
	do(h, "GET", "/random/one")
	do(h, "GET", "/random/two")
	body := do(h, "GET", "/metrics").Body.String()
	for _, want := range []string{
		`fishplate_http_requests_total{method="GET",route="/healthz",code="200"} 3`,
		`fishplate_http_requests_total{method="GET",route="/",code="404"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "/random") {
		t.Errorf("raw path leaked into labels:\n%s", body)
	}
}

func TestMetricsLabelEscaping(t *testing.T) {
	var b bytes.Buffer
	if _, err := NewMetrics(`v"1\`, "c\nx").WriteTo(&b); err != nil {
		t.Fatal(err)
	}
	if want := `fishplate_build_info{version="v\"1\\",commit="c\nx"} 1`; !strings.Contains(b.String(), want) {
		t.Fatalf("got:\n%s\nwant line %s", b.String(), want)
	}
}

func TestPanicRecovery(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	m := NewMetrics("v", "c")
	mux := http.NewServeMux()
	mux.HandleFunc("/boom", func(http.ResponseWriter, *http.Request) { panic("kaboom") })
	h := middleware(mux, logger, m)

	rec := do(h, "GET", "/boom", RequestIDHeader, "client-id-1")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != ProblemContentType {
		t.Fatalf("Content-Type = %q", ct)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("body is not JSON: %v: %s", err, rec.Body)
	}
	want := Problem{Type: "about:blank", Title: "Internal Server Error", Status: 500, Instance: "/boom", RequestID: "client-id-1"}
	p.Detail = ""
	if p != want {
		t.Fatalf("problem = %+v, want %+v", p, want)
	}
	if strings.Contains(rec.Body.String(), "kaboom") {
		t.Error("panic value leaked to client")
	}

	out := logs.String()
	if !strings.Contains(out, `"msg":"panic in handler"`) || !strings.Contains(out, "kaboom") {
		t.Errorf("panic not logged:\n%s", out)
	}
	if !strings.Contains(out, `"status":500`) {
		t.Errorf("access log does not record 500:\n%s", out)
	}

	var b bytes.Buffer
	m.WriteTo(&b)
	if !strings.Contains(b.String(), `route="/boom",code="500"} 1`) {
		t.Errorf("panic not counted:\n%s", b.String())
	}
}

func TestPanicAfterHeadersWritten(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	h := middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
		panic("late")
	}), logger, NewMetrics("v", "c"))
	rec := do(h, "GET", "/")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want original 202", rec.Code)
	}
}

func TestAbortHandlerPropagates(t *testing.T) {
	logger := slog.New(slog.NewJSONHandler(&bytes.Buffer{}, nil))
	h := middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	}), logger, NewMetrics("v", "c"))
	defer func() {
		if recover() != http.ErrAbortHandler {
			t.Fatal("ErrAbortHandler was swallowed")
		}
	}()
	do(h, "GET", "/")
}

func TestRequestIDAndAccessLog(t *testing.T) {
	tests := []struct {
		name     string
		inbound  string
		wantEcho bool
	}{
		{name: "generated", inbound: ""},
		{name: "valid inbound is kept", inbound: "abc-123_x.y", wantEcho: true},
		{name: "unsafe inbound is replaced", inbound: "bad id\"}"},
		{name: "oversized inbound is replaced", inbound: strings.Repeat("a", 129)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, logs := newTestServer(t)
			var hdr []string
			if tt.inbound != "" {
				hdr = []string{RequestIDHeader, tt.inbound}
			}
			rec := do(s.Handler(), "GET", "/healthz", hdr...)
			id := rec.Header().Get(RequestIDHeader)
			if tt.wantEcho && id != tt.inbound {
				t.Fatalf("id = %q, want %q", id, tt.inbound)
			}
			if !tt.wantEcho && (id == tt.inbound || len(id) != 32) {
				t.Fatalf("id = %q, want fresh 32-char id", id)
			}

			lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
			if len(lines) != 1 {
				t.Fatalf("want one log line per request, got %d:\n%s", len(lines), logs)
			}
			var entry map[string]any
			if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
				t.Fatalf("log line is not JSON: %v", err)
			}
			for k, want := range map[string]any{"request_id": id, "method": "GET", "path": "/healthz", "route": "/healthz", "status": float64(200)} {
				if entry[k] != want {
					t.Errorf("log %s = %v, want %v", k, entry[k], want)
				}
			}
		})
	}
}
