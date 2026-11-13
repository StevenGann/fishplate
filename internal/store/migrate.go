package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"time"
)

//go:embed migrations/*.sql
var embedded embed.FS

// Migration is one forward-only schema change.
type Migration struct {
	Version  int
	Name     string // file name, e.g. "0001_init.sql"
	SQL      string
	Checksum string // hex SHA-256 of SQL
}

// ErrSchemaTooNew is returned when the database was migrated by a newer
// binary. Running an older binary against it could corrupt data written under
// rules it does not know, so it refuses.
var ErrSchemaTooNew = errors.New("database schema is newer than this binary")

// SchemaTooNewError names both versions.
type SchemaTooNewError struct {
	Database int // highest version recorded in the database
	Binary   int // highest version embedded in this binary
}

func (e *SchemaTooNewError) Error() string {
	return fmt.Sprintf("database schema version %d is newer than this binary supports (version %d); run a fishplate release that includes migration %d",
		e.Database, e.Binary, e.Database)
}

func (e *SchemaTooNewError) Unwrap() error { return ErrSchemaTooNew }

var migrationName = regexp.MustCompile(`^([0-9]{4})_[a-z0-9_]+\.sql$`)

// Migrations returns the embedded migrations in version order.
func Migrations() ([]Migration, error) { return loadMigrations(embedded, "migrations") }

// loadMigrations reads and validates a migration directory: names must be
// NNNN_snake_case.sql, versions must start at 1 and have no gaps or
// duplicates.
func loadMigrations(fsys fs.FS, dir string) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("store: read migrations: %w", err)
	}
	var out []Migration
	for _, e := range entries {
		if e.IsDir() {
			return nil, fmt.Errorf("store: migrations: unexpected directory %s", e.Name())
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("store: migrations: %s does not match NNNN_name.sql", e.Name())
		}
		v, _ := strconv.Atoi(m[1])
		body, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("store: read migration %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		out = append(out, Migration{Version: v, Name: e.Name(), SQL: string(body), Checksum: hex.EncodeToString(sum[:])})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	for i, m := range out {
		if m.Version != i+1 {
			return nil, fmt.Errorf("store: migrations: expected version %04d, found %s (versions must start at 1 with no gaps or duplicates)", i+1, m.Name)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("store: no migrations embedded")
	}
	return out, nil
}

// Plan describes the state of a database relative to a set of migrations.
type Plan struct {
	Current int         // highest applied version, 0 for an empty database
	Latest  int         // highest version available
	Pending []Migration // migrations that would be applied, in order
	Exists  bool        // false when the database file does not exist yet
}

// appliedRow is one row of schema_migrations.
type appliedRow struct {
	version  int
	name     string
	checksum string
}

// querier is satisfied by *sql.DB, *sql.Conn and *sql.Tx.
type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// readApplied returns the ledger. A database without schema_migrations has
// applied nothing.
func readApplied(ctx context.Context, q querier) ([]appliedRow, error) {
	var n int
	if err := q.QueryRowContext(ctx,
		`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = 'schema_migrations'`).Scan(&n); err != nil {
		return nil, fmt.Errorf("store: inspect schema: %w", err)
	}
	if n == 0 {
		return nil, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("store: read schema_migrations: %w", err)
	}
	defer rows.Close()
	var out []appliedRow
	for rows.Next() {
		var r appliedRow
		if err := rows.Scan(&r.version, &r.name, &r.checksum); err != nil {
			return nil, fmt.Errorf("store: read schema_migrations: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// plan compares the ledger with the available migrations. It refuses a
// database that is ahead of the binary, one whose ledger has gaps, and one
// where an applied migration's checksum or name differs from the embedded
// copy (an applied migration was edited, which the rules forbid).
func plan(applied []appliedRow, all []Migration) (Plan, error) {
	latest := all[len(all)-1].Version
	p := Plan{Latest: latest, Exists: true}
	if len(applied) > 0 {
		if top := applied[len(applied)-1].version; top > latest {
			return Plan{}, &SchemaTooNewError{Database: top, Binary: latest}
		}
	}
	for i, r := range applied {
		m := all[i]
		if r.version != m.Version {
			return Plan{}, fmt.Errorf("store: schema_migrations is inconsistent: expected version %d, found %d", m.Version, r.version)
		}
		if r.checksum != m.Checksum || r.name != m.Name {
			return Plan{}, fmt.Errorf("store: migration %d (%s) was applied with checksum %s but this binary has %s (%s); applied migrations must never be edited",
				r.version, r.name, r.checksum, m.Checksum, m.Name)
		}
		p.Current = r.version
	}
	p.Pending = append([]Migration(nil), all[len(applied):]...)
	return p, nil
}

// Result reports what Migrate did.
type Result struct {
	From, To int
	Applied  []Migration // applied by this call; empty if another process got there first
}

// Migrate applies every pending embedded migration. See migrate.
func (s *Store) Migrate(ctx context.Context) (Result, error) {
	all, err := Migrations()
	if err != nil {
		return Result{}, err
	}
	return s.migrate(ctx, all)
}

// CheckSchema verifies the database is exactly at the binary's latest
// version without changing anything.
func (s *Store) CheckSchema(ctx context.Context) error {
	all, err := Migrations()
	if err != nil {
		return err
	}
	applied, err := readApplied(ctx, s.db)
	if err != nil {
		return err
	}
	p, err := plan(applied, all)
	if err != nil {
		return err
	}
	if len(p.Pending) > 0 {
		return fmt.Errorf("store: database is at schema version %d, binary expects %d; run fishplate migrate", p.Current, p.Latest)
	}
	return nil
}

// migrate applies pending migrations inside one transaction that begins
// IMMEDIATE (the DSN sets _txlock=immediate). That takes SQLite's RESERVED
// lock before the ledger is read, which is the advisory lock: a second
// process, or a second Store in this process, blocks in BEGIN for up to
// BusyTimeout, then reads a ledger that already contains the first runner's
// work and applies nothing. SQLite DDL is transactional, so a failing
// migration rolls back with everything before it in the same run; migrations
// therefore must not contain statements that cannot run in a transaction
// (PRAGMA journal_mode, VACUUM).
func (s *Store) migrate(ctx context.Context, all []Migration) (Result, error) {
	if s.readOnly {
		return Result{}, errors.New("store: cannot migrate a read-only database")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("store: acquire migration lock on %s: %w", s.path, err)
	}
	defer tx.Rollback()

	applied, err := readApplied(ctx, tx)
	if err != nil {
		return Result{}, err
	}
	p, err := plan(applied, all)
	if err != nil {
		return Result{}, err
	}
	res := Result{From: p.Current, To: p.Current}
	for _, m := range p.Pending {
		if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
			return Result{}, fmt.Errorf("store: apply migration %s: %w", m.Name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
			m.Version, m.Name, m.Checksum, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return Result{}, fmt.Errorf("store: record migration %s: %w", m.Name, err)
		}
		res.Applied = append(res.Applied, m)
		res.To = m.Version
	}
	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("store: commit migrations: %w", err)
	}
	return res, nil
}

// DryRun reports what Migrate would do to the database at path without
// creating, locking or writing it. A missing file is reported as an empty
// database with every migration pending.
func DryRun(ctx context.Context, dbPath string) (Plan, error) {
	all, err := Migrations()
	if err != nil {
		return Plan{}, err
	}
	if _, err := os.Stat(dbPath); errors.Is(err, fs.ErrNotExist) {
		p, err := plan(nil, all)
		p.Exists = false
		return p, err
	} else if err != nil {
		return Plan{}, fmt.Errorf("store: %s: %w", dbPath, err)
	}
	s, err := Open(ctx, dbPath, Options{ReadOnly: true})
	if err != nil {
		return Plan{}, err
	}
	defer s.Close()
	applied, err := readApplied(ctx, s.db)
	if err != nil {
		return Plan{}, err
	}
	return plan(applied, all)
}
