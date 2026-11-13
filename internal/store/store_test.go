package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"
)

func ctx(t *testing.T) context.Context {
	t.Helper()
	c, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return c
}

func openRW(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(ctx(t), path, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type ledgerRow struct {
	Version int
	Name    string
}

func ledger(t *testing.T, db *sql.DB) []ledgerRow {
	t.Helper()
	rows, err := db.Query(`SELECT version, name FROM schema_migrations ORDER BY rowid`)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	defer rows.Close()
	var out []ledgerRow
	for rows.Next() {
		var r ledgerRow
		if err := rows.Scan(&r.Version, &r.Name); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func wantLedger(t *testing.T) []ledgerRow {
	t.Helper()
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	var out []ledgerRow
	for _, m := range all {
		out = append(out, ledgerRow{m.Version, m.Name})
	}
	return out
}

func equalLedger(a, b []ledgerRow) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEmbeddedMigrations(t *testing.T) {
	all, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if all[0].Name != "0001_init.sql" {
		t.Fatalf("first migration = %s", all[0].Name)
	}
	// 0001 creates the ledger and nothing else.
	db := openRW(t, filepath.Join(t.TempDir(), "x.db")).DB()
	if _, err := db.Exec(all[0].SQL); err != nil {
		t.Fatal(err)
	}
	var names []string
	rows, err := db.Query(`SELECT name FROM sqlite_schema WHERE name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	if len(names) != 1 || names[0] != "schema_migrations" {
		t.Fatalf("0001_init created %v, want only schema_migrations", names)
	}
}

func TestOpenSettings(t *testing.T) {
	s := openRW(t, filepath.Join(t.TempDir(), "fishplate.db"))
	var mode string
	var busy, fk int
	db := s.DB()
	db.QueryRow(`PRAGMA journal_mode`).Scan(&mode)
	db.QueryRow(`PRAGMA busy_timeout`).Scan(&busy)
	db.QueryRow(`PRAGMA foreign_keys`).Scan(&fk)
	if mode != "wal" || busy != int(BusyTimeout.Milliseconds()) || fk != 1 {
		t.Fatalf("journal_mode=%s busy_timeout=%d foreign_keys=%d", mode, busy, fk)
	}
	if got := db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("MaxOpenConnections = %d, want 1 (one writer)", got)
	}
}

func TestOpenPathWithURISyntax(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a?b#c%20d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "x.db")
	openRW(t, p)
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("database not created at the literal path: %v", err)
	}
}

func TestOpenRefusesMissingDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-volume")
	_, err := Open(ctx(t), filepath.Join(missing, "fishplate.db"), Options{})
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("err = %v, want error naming %s", err, missing)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Open created the missing directory")
	}
}

func TestMigrateAppliesOnceAndIsIdempotent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fishplate.db")
	s := openRW(t, p)
	all, _ := Migrations()
	latest := all[len(all)-1].Version

	res, err := s.Migrate(ctx(t))
	if err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if res.From != 0 || res.To != latest || len(res.Applied) != len(all) {
		t.Fatalf("first run = %+v", res)
	}
	for i := 0; i < 3; i++ {
		res, err = s.Migrate(ctx(t))
		if err != nil {
			t.Fatalf("repeat Migrate: %v", err)
		}
		if res.From != latest || res.To != latest || len(res.Applied) != 0 {
			t.Fatalf("repeat run applied something: %+v", res)
		}
	}
	s.Close()

	// A fresh process (new Store) sees the same ledger and applies nothing.
	s2 := openRW(t, p)
	res, err = s2.Migrate(ctx(t))
	if err != nil || len(res.Applied) != 0 {
		t.Fatalf("reopen Migrate = %+v, %v", res, err)
	}
	if got := ledger(t, s2.DB()); !equalLedger(got, wantLedger(t)) {
		t.Fatalf("ledger = %v", got)
	}
	if err := s2.CheckSchema(ctx(t)); err != nil {
		t.Fatalf("CheckSchema: %v", err)
	}
}

func TestMigrateAppliesOnlyPending(t *testing.T) {
	all, _ := Migrations()
	s := openRW(t, filepath.Join(t.TempDir(), "fishplate.db"))
	res, err := s.migrate(ctx(t), all[:1])
	if err != nil || res.To != 1 {
		t.Fatalf("partial migrate = %+v, %v", res, err)
	}
	if err := s.CheckSchema(ctx(t)); err == nil || !strings.Contains(err.Error(), "run fishplate migrate") {
		t.Fatalf("CheckSchema on partial schema = %v", err)
	}
	res, err = s.Migrate(ctx(t))
	if err != nil {
		t.Fatal(err)
	}
	if res.From != 1 || len(res.Applied) != len(all)-1 || res.Applied[0].Version != 2 {
		t.Fatalf("second migrate = %+v", res)
	}
}

func TestMigrateRefusesNewerSchema(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fishplate.db")
	s := openRW(t, p)
	if _, err := s.Migrate(ctx(t)); err != nil {
		t.Fatal(err)
	}
	all, _ := Migrations()
	binary := all[len(all)-1].Version
	future := binary + 7
	if _, err := s.DB().Exec(`INSERT INTO schema_migrations VALUES (?, 'from_the_future.sql', 'x', 'now')`, future); err != nil {
		t.Fatal(err)
	}

	check := func(name string, err error) {
		t.Helper()
		var tooNew *SchemaTooNewError
		if !errors.As(err, &tooNew) || !errors.Is(err, ErrSchemaTooNew) {
			t.Fatalf("%s: err = %v, want SchemaTooNewError", name, err)
		}
		if tooNew.Database != future || tooNew.Binary != binary {
			t.Fatalf("%s: versions = %+v", name, tooNew)
		}
		msg := err.Error()
		if !strings.Contains(msg, "version "+strconv.Itoa(future)) || !strings.Contains(msg, "version "+strconv.Itoa(binary)) {
			t.Fatalf("%s: message %q does not name both versions", name, msg)
		}
	}
	_, err := s.Migrate(ctx(t))
	check("Migrate", err)
	check("CheckSchema", s.CheckSchema(ctx(t)))
	_, err = DryRun(ctx(t), p)
	check("DryRun", err)

	// The refusal changed nothing.
	if n := len(ledger(t, s.DB())); n != len(all)+1 {
		t.Fatalf("ledger rows = %d", n)
	}
}

func TestMigrateRefusesEditedMigration(t *testing.T) {
	s := openRW(t, filepath.Join(t.TempDir(), "fishplate.db"))
	if _, err := s.Migrate(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB().Exec(`UPDATE schema_migrations SET checksum = 'deadbeef' WHERE version = 1`); err != nil {
		t.Fatal(err)
	}
	_, err := s.Migrate(ctx(t))
	if err == nil || !strings.Contains(err.Error(), "must never be edited") {
		t.Fatalf("err = %v", err)
	}
}

func TestFailedMigrationRollsBack(t *testing.T) {
	good, _ := Migrations()
	bad := append(append([]Migration(nil), good...), Migration{
		Version: len(good) + 1, Name: "9999_bad.sql", SQL: "CREATE TABLE ok_then (x); THIS IS NOT SQL;",
	})
	s := openRW(t, filepath.Join(t.TempDir(), "fishplate.db"))
	if _, err := s.migrate(ctx(t), bad); err == nil {
		t.Fatal("bad migration succeeded")
	}
	var n int
	s.DB().QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name IN ('schema_migrations', 'ok_then')`).Scan(&n)
	if n != 0 {
		t.Fatalf("failed run left %d objects behind; the whole run must roll back", n)
	}
}

func TestLoadMigrationsValidation(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"gap", map[string]string{"m/0001_a.sql": "", "m/0003_c.sql": ""}, "expected version 0002"},
		{"duplicate", map[string]string{"m/0001_a.sql": "", "m/0001_b.sql": ""}, "expected version 0002"},
		{"bad name", map[string]string{"m/1_a.sql": ""}, "does not match"},
		{"does not start at 1", map[string]string{"m/0002_a.sql": ""}, "expected version 0001"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.MapFS{}
			for k, v := range tt.files {
				fsys[k] = &fstest.MapFile{Data: []byte(v)}
			}
			_, err := loadMigrations(fsys, "m")
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestDryRun(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fishplate.db")
	all, _ := Migrations()

	plan, err := DryRun(ctx(t), p)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Exists || plan.Current != 0 || len(plan.Pending) != len(all) {
		t.Fatalf("missing-file plan = %+v", plan)
	}
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dry run created the database")
	}

	s := openRW(t, p)
	if _, err := s.migrate(ctx(t), all[:1]); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before, _ := os.ReadFile(p)
	plan, err = DryRun(ctx(t), p)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Exists || plan.Current != 1 || len(plan.Pending) != len(all)-1 {
		t.Fatalf("partial plan = %+v", plan)
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("dry run modified the database file")
	}
}

func TestCheckWritable(t *testing.T) {
	s := openRW(t, filepath.Join(t.TempDir(), "fishplate.db"))
	if err := s.CheckWritable(ctx(t)); err == nil {
		t.Fatal("CheckWritable passed on an unmigrated database")
	}
	if _, err := s.Migrate(ctx(t)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.CheckWritable(ctx(t)); err != nil {
			t.Fatalf("CheckWritable: %v", err)
		}
	}
	var n int
	s.DB().QueryRow(`SELECT count(*) FROM readiness_probe`).Scan(&n)
	if n != 1 {
		t.Fatalf("readiness_probe rows = %d, want 1", n)
	}
}

// skipIfRoot skips permission-based tests when running as root, which
// ignores file modes.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
}

func TestCheckWritableFailsOnReadOnlyFile(t *testing.T) {
	skipIfRoot(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "fishplate.db")
	s := openRW(t, p)
	if _, err := s.Migrate(ctx(t)); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Write-protect the file and the directory (so -wal/-shm cannot be made
	// either). SQLite will still open it, silently read-only; readiness must
	// not be fooled by that.
	for _, f := range []string{p + "-wal", p + "-shm"} {
		os.Remove(f)
	}
	if err := os.Chmod(p, 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755); os.Chmod(p, 0o644) })

	s2, err := Open(ctx(t), p, Options{})
	if err == nil {
		defer s2.Close()
		if err := s2.CheckWritable(ctx(t)); err == nil {
			t.Fatal("CheckWritable passed on a write-protected database")
		}
	}
}

func TestCheckWritableFailsWhenFileRemoved(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "fishplate.db")
	s := openRW(t, p)
	if _, err := s.Migrate(ctx(t)); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckWritable(ctx(t)); err != nil {
		t.Fatal(err)
	}
	// Simulate the volume going away: the open handle still writes happily to
	// an unlinked inode, which is exactly the case that must not report ready.
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckWritable(ctx(t)); err == nil {
		t.Fatal("CheckWritable passed after the database file was removed")
	}
}

func TestOpenFailsInUnwritableDirectory(t *testing.T) {
	skipIfRoot(t)
	dir := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o755) })
	if s, err := Open(ctx(t), filepath.Join(dir, "fishplate.db"), Options{}); err == nil {
		s.Close()
		t.Fatal("Open succeeded in a read-only directory")
	}
}

// TestConcurrentMigrateInProcess starts many runners at once, each with its
// own Store (its own SQLite connection, as a second process would have), and
// asserts that exactly one of them applied the migrations, in order, once.
func TestConcurrentMigrateInProcess(t *testing.T) {
	p := filepath.Join(t.TempDir(), "fishplate.db")
	const runners = 8
	all, _ := Migrations()

	var (
		wg      sync.WaitGroup
		start   = make(chan struct{})
		mu      sync.Mutex
		results []Result
		errs    []error
	)
	for i := 0; i < runners; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			s, err := Open(context.Background(), p, Options{})
			if err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				return
			}
			defer s.Close()
			res, err := s.Migrate(context.Background())
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, err)
				return
			}
			results = append(results, res)
		}()
	}
	close(start)
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("runner errors: %v", errs)
	}
	assertSingleApplier(t, results, len(all))
	if got := ledger(t, openRW(t, p).DB()); !equalLedger(got, wantLedger(t)) {
		t.Fatalf("ledger = %v, want %v", got, wantLedger(t))
	}
}

func assertSingleApplier(t *testing.T, results []Result, total int) {
	t.Helper()
	appliers := 0
	for _, r := range results {
		switch len(r.Applied) {
		case 0:
		case total:
			appliers++
			for i, m := range r.Applied {
				if m.Version != i+1 {
					t.Fatalf("applied out of order: %v", r.Applied)
				}
			}
		default:
			t.Fatalf("a runner applied a partial set (%d of %d)", len(r.Applied), total)
		}
	}
	if appliers != 1 {
		t.Fatalf("%d runners applied migrations, want exactly 1", appliers)
	}
}

// helperEnv makes the test binary act as a migration process for
// TestConcurrentMigrateProcesses.
const helperEnv = "FISHPLATE_STORE_TEST_MIGRATE_HELPER"

func TestMain(m *testing.M) {
	if p := os.Getenv(helperEnv); p != "" {
		os.Exit(migrateHelper(p))
	}
	os.Exit(m.Run())
}

func migrateHelper(p string) int {
	s, err := Open(context.Background(), p, Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer s.Close()
	res, err := s.Migrate(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(len(res.Applied))
	return 0
}

// TestConcurrentMigrateProcesses is the cross-process version: separate OS
// processes race on one fresh database file. Exactly one applies.
func TestConcurrentMigrateProcesses(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "fishplate.db")
	const procs = 4
	all, _ := Migrations()

	cmds := make([]*exec.Cmd, procs)
	outs := make([]*strings.Builder, procs)
	errb := make([]*strings.Builder, procs)
	for i := range cmds {
		cmd := exec.Command(exe, "-test.run=^$")
		cmd.Env = append(os.Environ(), helperEnv+"="+p)
		outs[i], errb[i] = &strings.Builder{}, &strings.Builder{}
		cmd.Stdout, cmd.Stderr = outs[i], errb[i]
		cmds[i] = cmd
	}
	for _, c := range cmds {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
	}
	var results []Result
	for i, c := range cmds {
		if err := c.Wait(); err != nil {
			t.Fatalf("process %d: %v: %s", i, err, errb[i])
		}
		n, err := strconv.Atoi(strings.TrimSpace(outs[i].String()))
		if err != nil {
			t.Fatalf("process %d output %q", i, outs[i])
		}
		results = append(results, Result{Applied: make([]Migration, n)})
	}
	// Order is verified from the ledger below; here only the counts matter.
	appliers := 0
	for _, r := range results {
		switch len(r.Applied) {
		case 0:
		case len(all):
			appliers++
		default:
			t.Fatalf("a process applied a partial set: %d", len(r.Applied))
		}
	}
	if appliers != 1 {
		t.Fatalf("%d processes applied migrations, want exactly 1", appliers)
	}
	if got := ledger(t, openRW(t, p).DB()); !equalLedger(got, wantLedger(t)) {
		t.Fatalf("ledger = %v, want %v", got, wantLedger(t))
	}
}
