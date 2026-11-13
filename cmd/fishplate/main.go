// Command fishplate is the Fishplate server and CLI.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/StevenGann/fishplate/internal/config"
	"github.com/StevenGann/fishplate/internal/logging"
	"github.com/StevenGann/fishplate/internal/store"
	"github.com/StevenGann/fishplate/internal/version"
	"github.com/StevenGann/fishplate/internal/web"
)

const usage = `usage: fishplate <command> [flags]

commands:
  version                       print build information
  serve [flags]                 apply pending migrations, then run the HTTP server
  migrate [--dry-run] [flags]   apply pending database migrations, or list them
  config show --effective [flags]
                                print the resolved configuration, secrets redacted

Run "fishplate serve -h" to list configuration flags.
`

// Exit codes.
const (
	exitOK    = 0
	exitError = 1
	exitUsage = 2
)

// env abstracts the process environment so main is testable end to end.
type env struct {
	stdout, stderr io.Writer
	lookupEnv      func(string) (string, bool)
	readFile       func(string) ([]byte, error)
	// listen is net.Listen in production; tests substitute a port-0 listener.
	listen func(network, addr string) (net.Listener, error)
	// onReady is called with the bound address once serve is accepting.
	onReady func(addr net.Addr)
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	e := env{
		stdout:    os.Stdout,
		stderr:    os.Stderr,
		lookupEnv: os.LookupEnv,
		readFile:  os.ReadFile,
		listen:    net.Listen,
	}
	os.Exit(run(ctx, os.Args[1:], e))
}

func run(ctx context.Context, args []string, e env) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, usage)
		return exitUsage
	}
	switch args[0] {
	case "version", "--version", "-version":
		if len(args) > 1 {
			fmt.Fprintf(e.stderr, "fishplate version: unexpected arguments %q\n", args[1:])
			return exitUsage
		}
		fmt.Fprintln(e.stdout, version.String())
		return exitOK
	case "serve":
		return cmdServe(ctx, args[1:], e)
	case "migrate":
		return cmdMigrate(ctx, args[1:], e)
	case "config":
		return cmdConfig(args[1:], e)
	case "help", "-h", "--help":
		fmt.Fprint(e.stdout, usage)
		return exitOK
	default:
		fmt.Fprintf(e.stderr, "fishplate: unknown command %q\n\n%s", args[0], usage)
		return exitUsage
	}
}

// parseConfig parses config flags for a subcommand and resolves the
// configuration. extra registers subcommand-specific flags.
func parseConfig(name string, args []string, e env, extra func(*flag.FlagSet)) (config.Config, config.Source, int, bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	if extra != nil {
		extra(fs)
	}
	flags := config.RegisterFlags(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return config.Config{}, config.Source{}, exitOK, false
		}
		return config.Config{}, config.Source{}, exitUsage, false
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(e.stderr, "%s: unexpected arguments %q\n", name, fs.Args())
		return config.Config{}, config.Source{}, exitUsage, false
	}
	cfg, src, err := config.Load(flags, e.lookupEnv, e.readFile)
	if err != nil {
		fmt.Fprintf(e.stderr, "%s: invalid configuration: %v\n", name, err)
		return config.Config{}, config.Source{}, exitError, false
	}
	return cfg, src, exitOK, true
}

func cmdConfig(args []string, e env) int {
	if len(args) == 0 || args[0] != "show" {
		fmt.Fprint(e.stderr, "usage: fishplate config show --effective [flags]\n")
		return exitUsage
	}
	var effective bool
	cfg, src, code, ok := parseConfig("config show", args[1:], e, func(fs *flag.FlagSet) {
		fs.BoolVar(&effective, "effective", false, "print the resolved configuration (required)")
	})
	if !ok {
		return code
	}
	if !effective {
		// Only the resolved view exists; refuse rather than guess what was meant.
		fmt.Fprint(e.stderr, "config show: --effective is required\n")
		return exitUsage
	}
	if err := config.WriteEffective(e.stdout, cfg, src); err != nil {
		fmt.Fprintf(e.stderr, "config show: %v\n", err)
		return exitError
	}
	return exitOK
}

func cmdMigrate(ctx context.Context, args []string, e env) int {
	var dryRun bool
	cfg, _, code, ok := parseConfig("migrate", args, e, func(fs *flag.FlagSet) {
		fs.BoolVar(&dryRun, "dry-run", false, "report pending migrations without changing the database")
	})
	if !ok {
		return code
	}
	if dryRun {
		p, err := store.DryRun(ctx, cfg.DB.Path)
		if err != nil {
			fmt.Fprintf(e.stderr, "migrate --dry-run: %v\n", err)
			return exitError
		}
		if !p.Exists {
			fmt.Fprintf(e.stdout, "database %s does not exist; it would be created\n", cfg.DB.Path)
		}
		fmt.Fprintf(e.stdout, "schema version %d, binary version %d, %d pending\n", p.Current, p.Latest, len(p.Pending))
		for _, m := range p.Pending {
			fmt.Fprintf(e.stdout, "would apply %s\n", m.Name)
		}
		return exitOK
	}
	st, err := store.Open(ctx, cfg.DB.Path, store.Options{})
	if err != nil {
		fmt.Fprintf(e.stderr, "migrate: %v\n", err)
		return exitError
	}
	defer st.Close()
	res, err := st.Migrate(ctx)
	if err != nil {
		fmt.Fprintf(e.stderr, "migrate: %v\n", err)
		return exitError
	}
	for _, m := range res.Applied {
		fmt.Fprintf(e.stdout, "applied %s\n", m.Name)
	}
	fmt.Fprintf(e.stdout, "schema version %d (was %d), %d applied\n", res.To, res.From, len(res.Applied))
	return exitOK
}

func cmdServe(ctx context.Context, args []string, e env) int {
	cfg, src, code, ok := parseConfig("serve", args, e, nil)
	if !ok {
		return code
	}
	logger, err := logging.New(e.stderr, cfg.Log.Level, cfg.Log.Format)
	if err != nil {
		fmt.Fprintf(e.stderr, "serve: %v\n", err)
		return exitError
	}
	if err := serve(ctx, cfg, src, logger, e); err != nil {
		logger.Error("server stopped", "error", err)
		return exitError
	}
	return exitOK
}

func serve(ctx context.Context, cfg config.Config, src config.Source, logger *slog.Logger, e env) error {
	logger.Info("starting",
		"version", version.Version, "commit", version.Commit, "date", version.Date,
		"config_file", src.Path, "config_file_loaded", src.Loaded,
		"log_level", cfg.Log.Level, "db_path", cfg.DB.Path)

	// The database comes first: a process that cannot open or migrate it
	// refuses to start rather than serve without a store.
	st, err := store.Open(ctx, cfg.DB.Path, store.Options{})
	if err != nil {
		return err
	}
	defer st.Close()
	res, err := st.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	logger.Info("schema ready", "db_path", st.Path(), "from_version", res.From, "to_version", res.To,
		"applied", len(res.Applied))

	srv := web.New(logger, web.NewMetrics(version.Version, version.Commit),
		web.WithReadinessCheck(st.CheckWritable))

	ln, err := e.listen("tcp", cfg.HTTP.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", cfg.HTTP.Addr, err)
	}

	hs := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	logger.Info("listening", "addr", ln.Addr().String())

	errc := make(chan error, 1)
	go func() { errc <- hs.Serve(ln) }()
	srv.SetReady(true)
	if e.onReady != nil {
		e.onReady(ln.Addr())
	}

	select {
	case err := <-errc:
		return fmt.Errorf("serve: %w", err)
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	srv.SetReady(false)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := hs.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve: %w", err)
	}
	logger.Info("stopped")
	return nil
}
