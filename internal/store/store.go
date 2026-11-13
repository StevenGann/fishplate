// Package store owns the SQLite database: opening it with the pragmas the
// design requires, applying embedded forward-only migrations, and proving the
// database is writable.
//
// Concurrency model: one writer. Each Store holds a database/sql pool capped
// at a single connection, and every transaction begins IMMEDIATE so the
// SQLite write lock is taken up front instead of on the first write, which is
// what makes a read-then-write sequence (such as the migration runner's
// "read the ledger, apply what is missing") atomic across processes.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" database/sql driver
)

// BusyTimeout is how long a connection waits for another connection's lock
// before failing with SQLITE_BUSY. It bounds how long a second process waits
// for a concurrent migration to finish.
const BusyTimeout = 30 * time.Second

// Options controls how the database is opened.
type Options struct {
	// ReadOnly opens the database without write access and without creating
	// it. Used by "migrate --dry-run", which must not change anything.
	ReadOnly bool
}

// Store is an open database.
type Store struct {
	db       *sql.DB
	path     string
	readOnly bool
	// file identifies the database file that was opened, so CheckWritable can
	// notice when the path no longer refers to it (volume unmounted, file
	// deleted or replaced underneath a running process).
	file os.FileInfo
}

// Open opens the database at path. It never creates parent directories: a
// missing directory means a missing volume, and that is an error, not
// something to paper over. In read-write mode the file is created if absent
// and the connection is put in WAL mode; Open fails if WAL cannot be enabled.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("store: database path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: resolve %s: %w", path, err)
	}
	dir := filepath.Dir(abs)
	if fi, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("store: database directory %s: %w", dir, err)
	} else if !fi.IsDir() {
		return nil, fmt.Errorf("store: database directory %s is not a directory", dir)
	}
	if opts.ReadOnly {
		if _, err := os.Stat(abs); err != nil {
			return nil, fmt.Errorf("store: open %s read-only: %w", abs, err)
		}
	}

	db, err := sql.Open("sqlite", dsn(abs, opts.ReadOnly))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", abs, err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)
	db.SetConnMaxIdleTime(0)

	s := &Store{db: db, path: abs, readOnly: opts.ReadOnly}
	if err := s.init(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// dsn builds a SQLite URI filename. The path is percent-encoded so that '?',
// '#' and '%' in a directory name cannot be mistaken for URI syntax.
//
// The DSN carries only per-connection settings that take no file lock.
// journal_mode is deliberately absent: switching to WAL needs an exclusive
// lock on the file, and when that runs inside the driver's connect path a
// SQLITE_BUSY surfaces as an opaque connect failure. init performs the switch
// explicitly, under openRetry, and verifies the result.
func dsn(abs string, readOnly bool) string {
	q := url.Values{}
	// The driver applies busy_timeout before any other pragma.
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", BusyTimeout.Milliseconds()))
	q.Add("_pragma", "foreign_keys(1)")
	if readOnly {
		q.Set("mode", "ro")
		q.Add("_pragma", "query_only(1)")
	} else {
		q.Add("_pragma", "synchronous(NORMAL)")
		q.Set("_txlock", "immediate")
	}
	return "file:" + (&url.URL{Path: filepath.ToSlash(abs)}).EscapedPath() + "?" + q.Encode()
}

// init forces the first connection open, switches a read-write database to
// WAL and checks that the setting took. Every step is retried on SQLITE_BUSY
// by openRetry: busy_timeout does not cover every case, because SQLite
// returns BUSY without calling the busy handler when waiting could deadlock
// (two connections both upgrading to change the journal mode of a fresh
// file) and while another connection is running WAL recovery.
func (s *Store) init(ctx context.Context) error {
	err := openRetry(ctx, OpenTimeout, func(ctx context.Context) error {
		if err := s.db.PingContext(ctx); err != nil {
			return fmt.Errorf("connect: %w", err)
		}
		if s.readOnly {
			return nil
		}
		var mode string
		if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&mode); err != nil {
			return fmt.Errorf("set journal_mode=wal: %w", err)
		}
		if !strings.EqualFold(mode, "wal") {
			return fmt.Errorf("set journal_mode=wal: SQLite kept journal_mode %q (the filesystem may not support the shared memory WAL needs)", mode)
		}
		// Read it back in a separate statement: the value returned by the
		// assignment is what SQLite intended, this is what the file is.
		if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
			return fmt.Errorf("read journal_mode: %w", err)
		}
		if !strings.EqualFold(mode, "wal") {
			return fmt.Errorf("journal_mode is %q after switching, want wal", mode)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: open %s: %w", s.path, err)
	}
	fi, err := os.Stat(s.path)
	if err != nil {
		return fmt.Errorf("store: stat %s after open: %w", s.path, err)
	}
	s.file = fi
	return nil
}

// Path returns the absolute path of the database file.
func (s *Store) Path() string { return s.path }

// DB returns the underlying pool. Callers must not raise its connection limit.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// CheckWritable proves the database accepts writes by committing an update
// to the readiness_probe row, then confirms the path still names the file
// that was opened. It is the readiness check: a read-only open (SQLite
// silently opens a write-protected file read-only), a read-only or full
// filesystem, a held lock past BusyTimeout, an unmigrated schema, or a
// volume that disappeared under the process all return an error.
func (s *Store) CheckWritable(ctx context.Context) error {
	if s.readOnly {
		return errors.New("store: opened read-only")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO readiness_probe (id, checked_at) VALUES (1, ?)
		 ON CONFLICT (id) DO UPDATE SET checked_at = excluded.checked_at`, now); err != nil {
		return fmt.Errorf("store: write probe to %s: %w", s.path, err)
	}
	fi, err := os.Stat(s.path)
	if err != nil {
		return fmt.Errorf("store: database file %s: %w", s.path, err)
	}
	if !os.SameFile(fi, s.file) {
		return fmt.Errorf("store: %s has been replaced since it was opened", s.path)
	}
	return nil
}
