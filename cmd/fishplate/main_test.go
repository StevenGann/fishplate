package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

func testEnv(envVars, files map[string]string) (env, *bytes.Buffer, *bytes.Buffer) {
	var out, errb bytes.Buffer
	return env{
		stdout: &out,
		stderr: &errb,
		lookupEnv: func(k string) (string, bool) {
			v, ok := envVars[k]
			return v, ok
		},
		readFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		},
		listen: net.Listen,
	}, &out, &errb
}

func TestVersion(t *testing.T) {
	e, out, _ := testEnv(nil, nil)
	if code := run(context.Background(), []string{"version"}, e); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	re := regexp.MustCompile(`^fishplate \S+ \(\S+, \S+, ` + regexp.QuoteMeta(runtime.Version()) + `\)\n$`)
	if !re.MatchString(out.String()) {
		t.Fatalf("version output %q does not match %s", out, re)
	}
}

func TestCommandErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want int
		msg  string
	}{
		{name: "no args", args: nil, want: exitUsage, msg: "usage"},
		{name: "unknown command", args: []string{"frobnicate"}, want: exitUsage, msg: "unknown command"},
		{name: "version extra args", args: []string{"version", "x"}, want: exitUsage},
		{name: "config without show", args: []string{"config"}, want: exitUsage},
		{name: "config show without --effective", args: []string{"config", "show"}, want: exitUsage, msg: "--effective is required"},
		{name: "unknown flag", args: []string{"serve", "--nope"}, want: exitUsage},
		{name: "secret flag does not exist", args: []string{"config", "show", "--effective", "--secrets-asana-token", "x"}, want: exitUsage},
		{name: "invalid config refuses", args: []string{"config", "show", "--effective"}, env: map[string]string{"FISHPLATE_LOG_LEVEL": "loud"}, want: exitError, msg: "log.level"},
		{name: "serve invalid config refuses", args: []string{"serve"}, env: map[string]string{"FISHPLATE_HTTP_ADDR": ""}, want: exitError, msg: "http.addr"},
		{name: "serve missing explicit config refuses", args: []string{"serve", "--config", "/missing.yaml"}, want: exitError, msg: "/missing.yaml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _, errb := testEnv(tt.env, nil)
			if code := run(context.Background(), tt.args, e); code != tt.want {
				t.Fatalf("exit %d, want %d; stderr %s", code, tt.want, errb)
			}
			if tt.msg != "" && !strings.Contains(errb.String(), tt.msg) {
				t.Fatalf("stderr %q missing %q", errb, tt.msg)
			}
		})
	}
}

func TestConfigShowEffective(t *testing.T) {
	const secret = "do-not-print-me"
	e, out, _ := testEnv(
		map[string]string{
			"FISHPLATE_HTTP_ADDR":               ":9099",
			"FISHPLATE_SECRETS_ASANA_TOKEN":     secret,
			"FISHPLATE_SECRETS_SLACK_APP_TOKEN": "",
		},
		map[string]string{"/etc/fishplate/config.yaml": "log:\n  level: debug\nsecrets:\n  slack_bot_token: " + secret + "\n"},
	)
	if code := run(context.Background(), []string{"config", "show", "--effective", "--db-path", "/tmp/x.db"}, e); code != exitOK {
		t.Fatalf("exit %d", code)
	}
	s := out.String()
	for _, want := range []string{
		"addr: :9099", "level: debug", "path: /tmp/x.db",
		"asana_token: <set>", "slack_bot_token: <set>", "slack_app_token: <unset>", "github_app_private_key: <unset>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	if strings.Contains(s, secret) {
		t.Fatalf("secret leaked:\n%s", s)
	}
}

func TestServeEndToEnd(t *testing.T) {
	e, _, logs := testEnv(map[string]string{"FISHPLATE_HTTP_ADDR": "127.0.0.1:0"}, nil)
	ready := make(chan net.Addr, 1)
	e.onReady = func(a net.Addr) { ready <- a }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"serve"}, e) }()

	var addr net.Addr
	select {
	case addr = <-ready:
	case code := <-done:
		t.Fatalf("serve exited early with %d: %s", code, logs)
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not become ready")
	}

	base := "http://" + addr.String()
	get := func(path string) (int, string) {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := get("/healthz"); code != 200 || strings.TrimSpace(body) != `{"status":"ok"}` {
		t.Errorf("healthz = %d %q", code, body)
	}
	if code, _ := get("/readyz"); code != 200 {
		t.Errorf("readyz = %d", code)
	}
	if code, body := get("/metrics"); code != 200 || !strings.Contains(body, "fishplate_build_info{") {
		t.Errorf("metrics = %d %q", code, body)
	}

	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Fatalf("serve exit %d: %s", code, logs)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("serve did not shut down")
	}

	// Every stderr line from serve must be JSON.
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("non-JSON log line: %s", line)
		}
	}
	if !strings.Contains(logs.String(), `"request_id"`) {
		t.Errorf("access logs lack request_id:\n%s", logs)
	}
}

func TestServeListenFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	e, _, logs := testEnv(map[string]string{"FISHPLATE_HTTP_ADDR": ln.Addr().String()}, nil)
	if code := run(context.Background(), []string{"serve"}, e); code != exitError {
		t.Fatalf("exit %d, want %d", code, exitError)
	}
	if !strings.Contains(logs.String(), "listen on") {
		t.Fatalf("missing reason: %s", logs)
	}
}
