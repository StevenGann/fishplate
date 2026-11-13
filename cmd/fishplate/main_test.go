package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/StevenGann/fishplate/internal/store"
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
	e, _, logs := testEnv(map[string]string{"FISHPLATE_HTTP_ADDR": "127.0.0.1:0", "FISHPLATE_DB_PATH": tempDB(t)}, nil)
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
	e, _, logs := testEnv(map[string]string{"FISHPLATE_HTTP_ADDR": ln.Addr().String(), "FISHPLATE_DB_PATH": tempDB(t)}, nil)
	if code := run(context.Background(), []string{"serve"}, e); code != exitError {
		t.Fatalf("exit %d, want %d", code, exitError)
	}
	if !strings.Contains(logs.String(), "listen on") {
		t.Fatalf("missing reason: %s", logs)
	}
}

func tempDB(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "fishplate.db")
}

func TestMigrateCommand(t *testing.T) {
	p := tempDB(t)
	all, err := store.Migrations()
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"FISHPLATE_DB_PATH": p}

	// Dry run on a missing database creates nothing.
	e, out, errb := testEnv(env, nil)
	if code := run(context.Background(), []string{"migrate", "--dry-run"}, e); code != exitOK {
		t.Fatalf("dry-run exit %d: %s", code, errb)
	}
	if !strings.Contains(out.String(), "does not exist") || !strings.Contains(out.String(), "would apply "+all[0].Name) {
		t.Fatalf("dry-run output:\n%s", out)
	}
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("dry-run created the database: %v", err)
	}

	// Apply.
	e, out, errb = testEnv(env, nil)
	if code := run(context.Background(), []string{"migrate"}, e); code != exitOK {
		t.Fatalf("migrate exit %d: %s", code, errb)
	}
	want := fmt.Sprintf("schema version %d (was 0), %d applied", len(all), len(all))
	if !strings.Contains(out.String(), want) {
		t.Fatalf("migrate output %q, want %q", out, want)
	}

	// Again: idempotent.
	e, out, _ = testEnv(env, nil)
	if code := run(context.Background(), []string{"migrate"}, e); code != exitOK {
		t.Fatalf("second migrate exit %d", code)
	}
	if !strings.Contains(out.String(), "0 applied") {
		t.Fatalf("second migrate output %q", out)
	}

	// Dry run now has nothing pending.
	e, out, _ = testEnv(env, nil)
	if code := run(context.Background(), []string{"migrate", "--dry-run"}, e); code != exitOK || !strings.Contains(out.String(), "0 pending") {
		t.Fatalf("dry-run after migrate: exit %d, %q", code, out)
	}
}

func TestMigrateCommandMissingVolume(t *testing.T) {
	p := filepath.Join(t.TempDir(), "absent", "fishplate.db")
	e, _, errb := testEnv(map[string]string{"FISHPLATE_DB_PATH": p}, nil)
	if code := run(context.Background(), []string{"migrate"}, e); code != exitError {
		t.Fatalf("exit %d, want %d", code, exitError)
	}
	if !strings.Contains(errb.String(), filepath.Dir(p)) {
		t.Fatalf("stderr does not name the directory: %s", errb)
	}
}

// TestServeRefusesNewerSchema: a database migrated by a newer binary stops
// serve before it listens.
func TestServeRefusesNewerSchema(t *testing.T) {
	p := tempDB(t)
	st, err := store.Open(context.Background(), p, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`INSERT INTO schema_migrations VALUES (9999, '9999_future.sql', 'x', 'now')`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	e, _, logs := testEnv(map[string]string{"FISHPLATE_HTTP_ADDR": "127.0.0.1:0", "FISHPLATE_DB_PATH": p}, nil)
	e.listen = func(string, string) (net.Listener, error) {
		t.Error("serve listened despite a newer schema")
		return nil, errors.New("unexpected listen")
	}
	if code := run(context.Background(), []string{"serve"}, e); code != exitError {
		t.Fatalf("exit %d, want %d", code, exitError)
	}
	if !strings.Contains(logs.String(), "newer than this binary") {
		t.Fatalf("missing reason: %s", logs)
	}
}

// TestServeReadyzFailsWhenDatabaseUnwritable: serve migrates and reports
// ready, then the database file disappears and /readyz turns 503.
func TestServeReadyzFailsWhenDatabaseUnwritable(t *testing.T) {
	p := tempDB(t)
	e, _, logs := testEnv(map[string]string{"FISHPLATE_HTTP_ADDR": "127.0.0.1:0", "FISHPLATE_DB_PATH": p}, nil)
	ready := make(chan net.Addr, 1)
	e.onReady = func(a net.Addr) { ready <- a }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"serve"}, e) }()
	defer func() { cancel(); <-done }()

	var addr net.Addr
	select {
	case addr = <-ready:
	case code := <-done:
		t.Fatalf("serve exited early with %d: %s", code, logs)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not become ready")
	}
	status := func() int {
		resp, err := http.Get("http://" + addr.String() + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if got := status(); got != http.StatusOK {
		t.Fatalf("readyz = %d before removal", got)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if got := status(); got != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d after the database was removed, want 503", got)
	}
}
