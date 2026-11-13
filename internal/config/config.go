// Package config resolves Fishplate's configuration.
//
// Precedence, highest first: command-line flags, FISHPLATE_* environment
// variables, the YAML config file, then built-in defaults. Unknown keys in the
// config file are an error. Values in the secrets section are of type Secret,
// which never renders its value through fmt, slog, JSON or YAML.
package config

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultPath is the config file read when neither --config nor
// FISHPLATE_CONFIG is given. A missing file at this path is not an error; a
// missing file at an explicitly named path is.
const DefaultPath = "/etc/fishplate/config.yaml"

// EnvPrefix is the prefix for every environment variable Fishplate reads.
const EnvPrefix = "FISHPLATE_"

// Config is the resolved configuration.
type Config struct {
	HTTP    HTTP    `yaml:"http"`
	Log     Log     `yaml:"log"`
	DB      DB      `yaml:"db"`
	Secrets Secrets `yaml:"secrets"`
}

// HTTP configures the HTTP listener.
type HTTP struct {
	Addr string `yaml:"addr"`
}

// Log configures structured logging.
type Log struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// DB configures the store. Phase 0a does not open the database; the path is
// resolved and validated so that it appears in the effective configuration.
type DB struct {
	Path string `yaml:"path"`
}

// Secrets holds credentials. They may come from the environment or the config
// file, never from flags (flags are visible in process listings).
type Secrets struct {
	GitHubAppPrivateKey Secret `yaml:"github_app_private_key"`
	AsanaToken          Secret `yaml:"asana_token"`
	SlackBotToken       Secret `yaml:"slack_bot_token"`
	SlackAppToken       Secret `yaml:"slack_app_token"`
}

// Defaults returns the built-in defaults.
func Defaults() Config {
	return Config{
		HTTP: HTTP{Addr: ":8080"},
		Log:  Log{Level: "info", Format: "json"},
		DB:   DB{Path: "/data/fishplate.db"},
	}
}

// field describes one configurable setting. Every setting is listed here once
// so that the environment, flag and validation paths cannot drift apart.
type field struct {
	key    string // dotted YAML path, e.g. "http.addr"
	usage  string
	secret bool
	ptr    func(*Config) *string
}

var fields = []field{
	{key: "http.addr", usage: "HTTP listen address", ptr: func(c *Config) *string { return &c.HTTP.Addr }},
	{key: "log.level", usage: "log level: debug, info, warn, error", ptr: func(c *Config) *string { return &c.Log.Level }},
	{key: "log.format", usage: "log format: json", ptr: func(c *Config) *string { return &c.Log.Format }},
	{key: "db.path", usage: "SQLite database path", ptr: func(c *Config) *string { return &c.DB.Path }},
	{key: "secrets.github_app_private_key", secret: true, ptr: func(c *Config) *string { return (*string)(&c.Secrets.GitHubAppPrivateKey) }},
	{key: "secrets.asana_token", secret: true, ptr: func(c *Config) *string { return (*string)(&c.Secrets.AsanaToken) }},
	{key: "secrets.slack_bot_token", secret: true, ptr: func(c *Config) *string { return (*string)(&c.Secrets.SlackBotToken) }},
	{key: "secrets.slack_app_token", secret: true, ptr: func(c *Config) *string { return (*string)(&c.Secrets.SlackAppToken) }},
}

// EnvName returns the environment variable for a dotted key.
func EnvName(key string) string {
	return EnvPrefix + strings.ToUpper(strings.ReplaceAll(key, ".", "_"))
}

// FlagName returns the command-line flag for a dotted key.
func FlagName(key string) string {
	return strings.ReplaceAll(strings.ReplaceAll(key, ".", "-"), "_", "-")
}

// Flags holds the values of the configuration flags registered on a FlagSet.
type Flags struct {
	fs         *flag.FlagSet
	configPath string
	values     map[string]*string // key -> flag value
}

// RegisterFlags adds --config and one flag per non-secret setting to fs.
// Flag defaults are left empty: only flags the user actually sets take part
// in resolution, so the defaults shown come from Defaults().
func RegisterFlags(fs *flag.FlagSet) *Flags {
	f := &Flags{fs: fs, values: map[string]*string{}}
	fs.StringVar(&f.configPath, "config", "", "config file path (default "+DefaultPath+", or $"+EnvPrefix+"CONFIG)")
	d := Defaults()
	for _, fl := range fields {
		if fl.secret {
			continue
		}
		usage := fmt.Sprintf("%s (default %q, or $%s)", fl.usage, *fl.ptr(&d), EnvName(fl.key))
		f.values[fl.key] = fs.String(FlagName(fl.key), "", usage)
	}
	return f
}

// set returns the keys of flags the user set explicitly.
func (f *Flags) set() map[string]bool {
	out := map[string]bool{}
	if f == nil || f.fs == nil {
		return out
	}
	f.fs.Visit(func(fl *flag.Flag) { out[fl.Name] = true })
	return out
}

// Source records where the config file came from, for operator output.
type Source struct {
	Path     string // file path considered
	Loaded   bool   // true if the file was read
	Explicit bool   // true if named by --config or FISHPLATE_CONFIG
}

// Load resolves the configuration. lookupEnv is normally os.LookupEnv and
// readFile normally os.ReadFile; both are parameters so tests stay hermetic.
func Load(flags *Flags, lookupEnv func(string) (string, bool), readFile func(string) ([]byte, error)) (Config, Source, error) {
	cfg := Defaults()
	setFlags := flags.set()

	// Locate the config file: --config, then FISHPLATE_CONFIG, then the default.
	src := Source{Path: DefaultPath}
	if setFlags["config"] {
		src.Path, src.Explicit = flags.configPath, true
	} else if v, ok := lookupEnv(EnvPrefix + "CONFIG"); ok {
		src.Path, src.Explicit = v, true
	}
	if src.Explicit && src.Path == "" {
		return Config{}, src, errors.New("config file path is set but empty")
	}

	data, err := readFile(src.Path)
	switch {
	case err == nil:
		if err := decode(data, &cfg); err != nil {
			return Config{}, src, fmt.Errorf("config file %s: %w", src.Path, err)
		}
		src.Loaded = true
	case errors.Is(err, fs.ErrNotExist) && !src.Explicit:
		// The default path is optional; defaults, environment and flags apply.
	default:
		return Config{}, src, fmt.Errorf("config file %s: %w", src.Path, err)
	}

	for _, fl := range fields {
		if v, ok := lookupEnv(EnvName(fl.key)); ok {
			*fl.ptr(&cfg) = v
		}
	}

	for _, fl := range fields {
		if fl.secret {
			continue
		}
		if setFlags[FlagName(fl.key)] {
			*fl.ptr(&cfg) = *flags.values[fl.key]
		}
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, src, err
	}
	return cfg, src, nil
}

// decode parses a YAML document into cfg, rejecting unknown keys and
// multi-document files.
func decode(data []byte, cfg *Config) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		if errors.Is(err, io.EOF) {
			return nil // empty file
		}
		return err
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return errors.New("multiple YAML documents are not supported")
	}
	return nil
}

var (
	validLevels  = []string{"debug", "info", "warn", "error"}
	validFormats = []string{"json"}
)

// Validate checks that every required setting is present and well-formed.
func (c Config) Validate() error {
	var errs []error
	if strings.TrimSpace(c.HTTP.Addr) == "" {
		errs = append(errs, errors.New("http.addr must not be empty"))
	}
	if !contains(validLevels, c.Log.Level) {
		errs = append(errs, fmt.Errorf("log.level %q is not one of %s", c.Log.Level, strings.Join(validLevels, ", ")))
	}
	if !contains(validFormats, c.Log.Format) {
		errs = append(errs, fmt.Errorf("log.format %q is not one of %s", c.Log.Format, strings.Join(validFormats, ", ")))
	}
	if strings.TrimSpace(c.DB.Path) == "" {
		errs = append(errs, errors.New("db.path must not be empty"))
	}
	return errors.Join(errs...)
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

// WriteEffective writes the resolved configuration as YAML. Secrets render as
// "<set>" or "<unset>"; their values are never written.
func WriteEffective(w io.Writer, c Config, src Source) error {
	switch {
	case src.Loaded:
		fmt.Fprintf(w, "# config file: %s\n", src.Path)
	default:
		fmt.Fprintf(w, "# config file: none (%s not found)\n", src.Path)
	}
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return err
	}
	return enc.Close()
}

// LoadFromOS is Load with the real environment and filesystem.
func LoadFromOS(flags *Flags) (Config, Source, error) {
	return Load(flags, os.LookupEnv, os.ReadFile)
}
