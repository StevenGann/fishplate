package web

import (
	"encoding/json"
	"net/http"
)

// Problem is an RFC 9457 problem details object.
type Problem struct {
	Type      string `json:"type"`
	Title     string `json:"title"`
	Status    int    `json:"status"`
	Detail    string `json:"detail,omitempty"`
	Instance  string `json:"instance,omitempty"`
	RequestID string `json:"request_id,omitempty"` // extension member
}

// ProblemContentType is the media type defined by RFC 9457.
const ProblemContentType = "application/problem+json"

// writeProblem writes p with the problem+json media type. Type defaults to
// "about:blank" and Title to the HTTP status text, as RFC 9457 section 4.2.1
// prescribes for problems that carry no extra semantics.
func writeProblem(w http.ResponseWriter, r *http.Request, p Problem) {
	if p.Type == "" {
		p.Type = "about:blank"
	}
	if p.Title == "" {
		p.Title = http.StatusText(p.Status)
	}
	if p.Instance == "" && r != nil {
		p.Instance = r.URL.Path
	}
	if p.RequestID == "" && r != nil {
		p.RequestID = requestIDFrom(r.Context())
	}
	h := w.Header()
	h.Set("Content-Type", ProblemContentType)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
