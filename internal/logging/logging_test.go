package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name       string
		level      string
		format     string
		wantErr    bool
		debugShown bool
		infoShown  bool
	}{
		{name: "debug", level: "debug", format: "json", debugShown: true, infoShown: true},
		{name: "info", level: "info", format: "json", infoShown: true},
		{name: "error", level: "error", format: "json"},
		{name: "bad level", level: "loud", format: "json", wantErr: true},
		{name: "bad format", level: "info", format: "text", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			l, err := New(&buf, tt.level, tt.format)
			if tt.wantErr {
				if err == nil {
					t.Fatal("want error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			l.Debug("d")
			l.Info("i")
			out := buf.String()
			if got := strings.Contains(out, `"msg":"d"`); got != tt.debugShown {
				t.Errorf("debug shown = %v, want %v", got, tt.debugShown)
			}
			if got := strings.Contains(out, `"msg":"i"`); got != tt.infoShown {
				t.Errorf("info shown = %v, want %v", got, tt.infoShown)
			}
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				if line == "" {
					continue
				}
				if !json.Valid([]byte(line)) {
					t.Errorf("not JSON: %s", line)
				}
			}
		})
	}
}

func TestFromContext(t *testing.T) {
	var buf bytes.Buffer
	l, _ := New(&buf, "info", "json")
	FromContext(WithRequestID(context.Background(), "abc123"), l).Info("x")
	if !strings.Contains(buf.String(), `"request_id":"abc123"`) {
		t.Fatalf("request_id missing: %s", buf.String())
	}
	buf.Reset()
	FromContext(context.Background(), l).Info("y")
	if strings.Contains(buf.String(), "request_id") {
		t.Fatalf("unexpected request_id: %s", buf.String())
	}
}
