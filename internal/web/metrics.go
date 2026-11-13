package web

import (
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Metrics is a minimal Prometheus registry for Phase 0a: build info and a
// request counter, rendered in the text exposition format without a client
// library.
type Metrics struct {
	version, commit string

	mu       sync.Mutex
	requests map[requestKey]uint64
}

type requestKey struct {
	method, route string
	code          int
}

// NewMetrics returns a registry labelled with the given build metadata.
func NewMetrics(version, commit string) *Metrics {
	return &Metrics{version: version, commit: commit, requests: map[requestKey]uint64{}}
}

// knownMethods bounds the cardinality of the method label.
var knownMethods = map[string]bool{
	http.MethodGet: true, http.MethodHead: true, http.MethodPost: true, http.MethodPut: true,
	http.MethodPatch: true, http.MethodDelete: true, http.MethodOptions: true,
}

// observe counts one completed request. route must be a registered pattern
// or a fixed placeholder, never the raw path, so the label set stays bounded.
func (m *Metrics) observe(method, route string, code int) {
	if !knownMethods[method] {
		method = "OTHER"
	}
	m.mu.Lock()
	m.requests[requestKey{method, route, code}]++
	m.mu.Unlock()
}

// WriteTo renders the registry in Prometheus text format.
func (m *Metrics) WriteTo(w io.Writer) (int64, error) {
	var b strings.Builder
	b.WriteString("# HELP fishplate_build_info Build metadata; the value is always 1.\n")
	b.WriteString("# TYPE fishplate_build_info gauge\n")
	fmt.Fprintf(&b, "fishplate_build_info{version=\"%s\",commit=\"%s\"} 1\n", escapeLabel(m.version), escapeLabel(m.commit))

	b.WriteString("# HELP fishplate_http_requests_total HTTP requests served, by method, route and status code.\n")
	b.WriteString("# TYPE fishplate_http_requests_total counter\n")
	m.mu.Lock()
	keys := make([]requestKey, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	counts := make(map[requestKey]uint64, len(keys))
	for _, k := range keys {
		counts[k] = m.requests[k]
	}
	m.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool {
		a, c := keys[i], keys[j]
		if a.route != c.route {
			return a.route < c.route
		}
		if a.method != c.method {
			return a.method < c.method
		}
		return a.code < c.code
	})
	for _, k := range keys {
		fmt.Fprintf(&b, "fishplate_http_requests_total{method=\"%s\",route=\"%s\",code=\"%s\"} %d\n",
			escapeLabel(k.method), escapeLabel(k.route), strconv.Itoa(k.code), counts[k])
	}
	n, err := io.WriteString(w, b.String())
	return int64(n), err
}

// escapeLabel escapes a label value per the Prometheus text format.
func escapeLabel(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// ContentType is the Prometheus text exposition media type.
const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"
