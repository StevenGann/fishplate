package config

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"testing"
)

// fakeEnv returns a lookupEnv backed by a map.
func fakeEnv(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

// fakeFS returns a readFile backed by a map; missing paths yield fs.ErrNotExist.
func fakeFS(m map[string]string) func(string) ([]byte, error) {
	return func(p string) ([]byte, error) {
		v, ok := m[p]
		if !ok {
			return nil, &fs.PathError{Op: "open", Path: p, Err: fs.ErrNotExist}
		}
		return []byte(v), nil
	}
}

func parseFlags(t *testing.T, args ...string) *Flags {
	t.Helper()
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	f := RegisterFlags(set)
	if err := set.Parse(args); err != nil {
		t.Fatalf("parse flags: %v", err)
	}
	return f
}

func TestPrecedence(t *testing.T) {
	const custom = "/srv/custom.yaml"
	fileAll := "http:\n  addr: \":7000\"\nlog:\n  level: warn\ndb:\n  path: /file/db\n"

	tests := []struct {
		name  string
		args  []string
		env   map[string]string
		files map[string]string
		want  Config
	}{
		{
			name: "defaults only",
			want: Defaults(),
		},
		{
			name:  "file over defaults",
			files: map[string]string{DefaultPath: fileAll},
			want:  Config{HTTP: HTTP{Addr: ":7000"}, Log: Log{Level: "warn", Format: "json"}, DB: DB{Path: "/file/db"}},
		},
		{
			name:  "env over file",
			files: map[string]string{DefaultPath: fileAll},
			env:   map[string]string{"FISHPLATE_HTTP_ADDR": ":9099", "FISHPLATE_LOG_LEVEL": "debug"},
			want:  Config{HTTP: HTTP{Addr: ":9099"}, Log: Log{Level: "debug", Format: "json"}, DB: DB{Path: "/file/db"}},
		},
		{
			name:  "flag over env over file",
			args:  []string{"--http-addr", ":1234"},
			files: map[string]string{DefaultPath: fileAll},
			env:   map[string]string{"FISHPLATE_HTTP_ADDR": ":9099", "FISHPLATE_DB_PATH": "/env/db"},
			want:  Config{HTTP: HTTP{Addr: ":1234"}, Log: Log{Level: "warn", Format: "json"}, DB: DB{Path: "/env/db"}},
		},
		{
			name: "flag over defaults without file",
			args: []string{"--log-level=error", "--db-path=/flag/db"},
			want: Config{HTTP: HTTP{Addr: ":8080"}, Log: Log{Level: "error", Format: "json"}, DB: DB{Path: "/flag/db"}},
		},
		{
			name:  "--config selects file",
			args:  []string{"--config", custom},
			files: map[string]string{custom: "http:\n  addr: \":5555\"\n", DefaultPath: fileAll},
			want:  Config{HTTP: HTTP{Addr: ":5555"}, Log: Log{Level: "info", Format: "json"}, DB: DB{Path: "/data/fishplate.db"}},
		},
		{
			name:  "FISHPLATE_CONFIG selects file",
			env:   map[string]string{"FISHPLATE_CONFIG": custom},
			files: map[string]string{custom: "db:\n  path: /custom/db\n"},
			want:  Config{HTTP: HTTP{Addr: ":8080"}, Log: Log{Level: "info", Format: "json"}, DB: DB{Path: "/custom/db"}},
		},
		{
			name:  "--config over FISHPLATE_CONFIG",
			args:  []string{"--config", custom},
			env:   map[string]string{"FISHPLATE_CONFIG": "/elsewhere.yaml"},
			files: map[string]string{custom: "db:\n  path: /flag-file/db\n"},
			want:  Config{HTTP: HTTP{Addr: ":8080"}, Log: Log{Level: "info", Format: "json"}, DB: DB{Path: "/flag-file/db"}},
		},
		{
			name:  "secret env over file",
			files: map[string]string{DefaultPath: "secrets:\n  asana_token: from-file\n  slack_bot_token: file-bot\n"},
			env:   map[string]string{"FISHPLATE_SECRETS_ASANA_TOKEN": "from-env"},
			want: Config{HTTP: HTTP{Addr: ":8080"}, Log: Log{Level: "info", Format: "json"}, DB: DB{Path: "/data/fishplate.db"},
				Secrets: Secrets{AsanaToken: "from-env", SlackBotToken: "file-bot"}},
		},
		{
			name:  "empty file keeps defaults",
			files: map[string]string{DefaultPath: ""},
			want:  Defaults(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _, err := Load(parseFlags(t, tt.args...), fakeEnv(tt.env), fakeFS(tt.files))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got  %#v\nwant %#v", got, tt.want)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		env     map[string]string
		files   map[string]string
		wantErr string
	}{
		{
			name:    "unknown top-level key",
			files:   map[string]string{DefaultPath: "http:\n  addr: \":1\"\nbogus: true\n"},
			wantErr: "field bogus not found",
		},
		{
			name:    "unknown nested key",
			files:   map[string]string{DefaultPath: "http:\n  adr: \":1\"\n"},
			wantErr: "field adr not found",
		},
		{
			name:    "unknown secret key",
			files:   map[string]string{DefaultPath: "secrets:\n  github_token: x\n"},
			wantErr: "field github_token not found",
		},
		{
			name:    "multiple documents",
			files:   map[string]string{DefaultPath: "http:\n  addr: \":1\"\n---\nhttp:\n  addr: \":2\"\n"},
			wantErr: "multiple YAML documents",
		},
		{
			name:    "malformed yaml",
			files:   map[string]string{DefaultPath: "http: [\n"},
			wantErr: "config file",
		},
		{
			name:    "explicit file missing",
			args:    []string{"--config", "/nope.yaml"},
			wantErr: "/nope.yaml",
		},
		{
			name:    "explicit env file missing",
			env:     map[string]string{"FISHPLATE_CONFIG": "/nope.yaml"},
			wantErr: "/nope.yaml",
		},
		{
			name:    "empty config path",
			env:     map[string]string{"FISHPLATE_CONFIG": ""},
			wantErr: "empty",
		},
		{
			name:    "invalid log level",
			env:     map[string]string{"FISHPLATE_LOG_LEVEL": "loud"},
			wantErr: "log.level",
		},
		{
			name:    "unsupported log format",
			args:    []string{"--log-format", "text"},
			wantErr: "log.format",
		},
		{
			name:    "empty http addr",
			env:     map[string]string{"FISHPLATE_HTTP_ADDR": ""},
			wantErr: "http.addr",
		},
		{
			name:    "empty db path",
			files:   map[string]string{DefaultPath: "db:\n  path: \"\"\n"},
			wantErr: "db.path",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := Load(parseFlags(t, tt.args...), fakeEnv(tt.env), fakeFS(tt.files))
			if err == nil {
				t.Fatalf("Load succeeded, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestSecretsHaveNoFlags(t *testing.T) {
	set := flag.NewFlagSet("test", flag.ContinueOnError)
	RegisterFlags(set)
	set.VisitAll(func(f *flag.Flag) {
		if strings.Contains(f.Name, "secret") || strings.Contains(f.Name, "token") || strings.Contains(f.Name, "key") {
			t.Errorf("secret exposed as flag --%s", f.Name)
		}
	})
}

func TestRedaction(t *testing.T) {
	const sentinel = "sentinel-value-must-not-leak"

	tests := []struct {
		name    string
		secrets Secrets
		want    map[string]string // yaml key -> rendered marker
	}{
		{
			name: "all unset",
			want: map[string]string{
				"github_app_private_key": RedactedUnset, "asana_token": RedactedUnset,
				"slack_bot_token": RedactedUnset, "slack_app_token": RedactedUnset,
			},
		},
		{
			name:    "some set",
			secrets: Secrets{AsanaToken: sentinel, SlackAppToken: sentinel},
			want: map[string]string{
				"github_app_private_key": RedactedUnset, "asana_token": RedactedSet,
				"slack_bot_token": RedactedUnset, "slack_app_token": RedactedSet,
			},
		},
		{
			name: "all set",
			secrets: Secrets{GitHubAppPrivateKey: "-----BEGIN " + sentinel, AsanaToken: sentinel,
				SlackBotToken: sentinel, SlackAppToken: sentinel},
			want: map[string]string{
				"github_app_private_key": RedactedSet, "asana_token": RedactedSet,
				"slack_bot_token": RedactedSet, "slack_app_token": RedactedSet,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Secrets = tt.secrets

			var effective bytes.Buffer
			if err := WriteEffective(&effective, cfg, Source{Path: DefaultPath}); err != nil {
				t.Fatalf("WriteEffective: %v", err)
			}
			for k, marker := range tt.want {
				line := fmt.Sprintf("%s: %s", k, marker)
				if !strings.Contains(effective.String(), line) {
					t.Errorf("effective output missing %q:\n%s", line, effective.String())
				}
			}

			jsonOut, err := json.Marshal(cfg)
			if err != nil {
				t.Fatalf("json: %v", err)
			}
			var logOut bytes.Buffer
			slog.New(slog.NewJSONHandler(&logOut, nil)).Info("cfg", "secrets", cfg.Secrets, "token", cfg.Secrets.AsanaToken)

			renderings := map[string]string{
				"effective yaml": effective.String(),
				"json":           string(jsonOut),
				"fmt %v":         fmt.Sprintf("%v", cfg),
				"fmt %+v":        fmt.Sprintf("%+v", cfg),
				"fmt %#v":        fmt.Sprintf("%#v", cfg),
				"fmt %s":         fmt.Sprintf("%s", cfg.Secrets.AsanaToken),
				"slog":           logOut.String(),
			}
			for name, out := range renderings {
				if strings.Contains(out, sentinel) {
					t.Errorf("%s leaked secret value: %s", name, out)
				}
			}
		})
	}
}

func TestSecretRevealAndDecode(t *testing.T) {
	cfg, _, err := Load(parseFlags(t), fakeEnv(nil), fakeFS(map[string]string{DefaultPath: "secrets:\n  slack_bot_token: abc\n"}))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Secrets.SlackBotToken.Reveal(); got != "abc" {
		t.Fatalf("Reveal = %q, want abc", got)
	}
	if !cfg.Secrets.SlackBotToken.IsSet() || cfg.Secrets.AsanaToken.IsSet() {
		t.Fatal("IsSet wrong")
	}
}

func TestWriteEffectiveRoundTrips(t *testing.T) {
	cfg := Defaults()
	cfg.HTTP.Addr = ":9099"
	var buf bytes.Buffer
	if err := WriteEffective(&buf, cfg, Source{Path: "/x.yaml", Loaded: true}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"# config file: /x.yaml", "addr: :9099", "level: info", "format: json", "path: /data/fishplate.db"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	// The output, minus redaction markers, must be accepted by the strict decoder.
	got := Defaults()
	if err := decode(buf.Bytes(), &got); err != nil {
		t.Fatalf("effective output is not valid config: %v", err)
	}
}

func TestEnvAndFlagNames(t *testing.T) {
	tests := []struct{ key, env, flag string }{
		{"http.addr", "FISHPLATE_HTTP_ADDR", "http-addr"},
		{"log.level", "FISHPLATE_LOG_LEVEL", "log-level"},
		{"db.path", "FISHPLATE_DB_PATH", "db-path"},
		{"secrets.asana_token", "FISHPLATE_SECRETS_ASANA_TOKEN", "secrets-asana-token"},
	}
	for _, tt := range tests {
		if got := EnvName(tt.key); got != tt.env {
			t.Errorf("EnvName(%q) = %q, want %q", tt.key, got, tt.env)
		}
		if got := FlagName(tt.key); got != tt.flag {
			t.Errorf("FlagName(%q) = %q, want %q", tt.key, got, tt.flag)
		}
	}
}
