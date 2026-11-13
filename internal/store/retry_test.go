package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// realBusy returns a genuine SQLITE_BUSY error from the driver: one
// connection holds an exclusive lock and a second, with no busy timeout,
// tries to read.
func realBusy(t *testing.T) error {
	t.Helper()
	p := filepath.Join(t.TempDir(), "busy.db")
	holder, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { holder.Close() })
	holder.SetMaxOpenConns(1)
	if _, err := holder.Exec(`CREATE TABLE t (x)`); err != nil {
		t.Fatal(err)
	}
	conn, err := holder.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := conn.ExecContext(context.Background(), `BEGIN EXCLUSIVE`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.ExecContext(context.Background(), `ROLLBACK`) })

	other, err := sql.Open("sqlite", "file:"+p+"?_pragma=busy_timeout(0)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	var n int
	err = other.QueryRow(`SELECT count(*) FROM t`).Scan(&n)
	if !isBusy(err) {
		t.Fatalf("expected SQLITE_BUSY, got %v", err)
	}
	return err
}

// noSleep records requested waits instead of sleeping.
func noSleep(t *testing.T) *[]time.Duration {
	t.Helper()
	var waits []time.Duration
	orig := sleep
	sleep = func(ctx context.Context, d time.Duration) error {
		waits = append(waits, d)
		return ctx.Err()
	}
	t.Cleanup(func() { sleep = orig })
	return &waits
}

func TestOpenRetryRetriesBusyThenSucceeds(t *testing.T) {
	busy := realBusy(t)
	waits := noSleep(t)
	calls := 0
	err := openRetry(context.Background(), time.Minute, func(context.Context) error {
		calls++
		if calls < 4 {
			return busy
		}
		return nil
	})
	if err != nil {
		t.Fatalf("openRetry: %v", err)
	}
	if calls != 4 || len(*waits) != 3 {
		t.Fatalf("calls=%d waits=%v, want 4 calls and 3 waits", calls, *waits)
	}
	for i, w := range *waits {
		hi := minBackoff << i
		if w < hi/2 || w > hi {
			t.Fatalf("wait %d = %s, want within [%s, %s]", i, w, hi/2, hi)
		}
	}
}

func TestOpenRetryBackoffIsCapped(t *testing.T) {
	busy := realBusy(t)
	waits := noSleep(t)
	calls := 0
	_ = openRetry(context.Background(), time.Minute, func(context.Context) error {
		if calls++; calls < 20 {
			return busy
		}
		return nil
	})
	for _, w := range *waits {
		if w > maxBackoff {
			t.Fatalf("wait %s exceeds cap %s", w, maxBackoff)
		}
	}
}

func TestOpenRetryDoesNotRetryOtherErrors(t *testing.T) {
	noSleep(t)
	other := errors.New("disk on fire")
	calls := 0
	err := openRetry(context.Background(), time.Minute, func(context.Context) error {
		calls++
		return other
	})
	if !errors.Is(err, other) || calls != 1 {
		t.Fatalf("err=%v calls=%d, want the error returned after 1 call", err, calls)
	}
}

func TestOpenRetryGivesUpAtDeadline(t *testing.T) {
	busy := realBusy(t)
	start := time.Now()
	calls := 0
	err := openRetry(context.Background(), 150*time.Millisecond, func(context.Context) error {
		calls++
		return busy
	})
	elapsed := time.Since(start)
	if err == nil || !isBusy(err) {
		t.Fatalf("err = %v, want wrapped SQLITE_BUSY", err)
	}
	if !strings.Contains(err.Error(), "still locked after") {
		t.Fatalf("err = %q, want a message saying the database stayed locked", err)
	}
	if calls < 2 {
		t.Fatalf("calls = %d, want retries before giving up", calls)
	}
	if elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("gave up after %s, want about 150ms", elapsed)
	}
}

func TestOpenRetryHonoursContext(t *testing.T) {
	busy := realBusy(t)
	c, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := openRetry(c, time.Hour, func(context.Context) error { return busy })
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("err=%v after %s, want failure near the context deadline", err, time.Since(start))
	}
}

// TestOpenStress opens many Stores on fresh files at the same instant, many
// times over. Before the WAL switch was retried this failed with
// SQLITE_BUSY from Open within a few rounds.
func TestOpenStress(t *testing.T) {
	const rounds, openers = 20, 8
	for r := 0; r < rounds; r++ {
		p := filepath.Join(t.TempDir(), "fishplate.db")
		start := make(chan struct{})
		errc := make(chan error, openers)
		for i := 0; i < openers; i++ {
			go func() {
				<-start
				s, err := Open(context.Background(), p, Options{})
				if err != nil {
					errc <- err
					return
				}
				var mode string
				err = s.DB().QueryRow(`PRAGMA journal_mode`).Scan(&mode)
				s.Close()
				if err == nil && mode != "wal" {
					err = errors.New("journal_mode " + mode)
				}
				errc <- err
			}()
		}
		close(start)
		for i := 0; i < openers; i++ {
			if err := <-errc; err != nil {
				t.Fatalf("round %d: %v", r, err)
			}
		}
	}
}
