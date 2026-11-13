package store

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// OpenTimeout bounds how long Open keeps retrying a database that reports
// SQLITE_BUSY while it is being opened. It matches BusyTimeout: a process
// that opens the database while another is migrating it waits as long as it
// would for the migration lock itself.
const OpenTimeout = BusyTimeout

// Backoff bounds for openRetry. The first retry waits about minBackoff; each
// later one doubles, capped at maxBackoff, and is jittered so processes that
// collided once do not collide again in lockstep.
const (
	minBackoff = 5 * time.Millisecond
	maxBackoff = 500 * time.Millisecond
)

// isBusy reports whether err is SQLITE_BUSY or one of its extended codes
// (BUSY_RECOVERY, BUSY_SNAPSHOT, BUSY_TIMEOUT).
func isBusy(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code()&0xff == sqlite3.SQLITE_BUSY
}

// sleep is replaced in tests.
var sleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// openRetry runs step until it succeeds, fails with an error other than
// SQLITE_BUSY, or limit (or ctx) expires. Retries back off exponentially with
// full jitter. On expiry the last BUSY error is returned with the number of
// attempts and the elapsed time, so the caller can say why it gave up.
func openRetry(ctx context.Context, limit time.Duration, step func(context.Context) error) error {
	start := time.Now()
	deadline := start.Add(limit)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	backoff := minBackoff
	for attempt := 1; ; attempt++ {
		err := step(ctx)
		if err == nil || !isBusy(err) {
			return err
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("database still locked after %d attempts over %s: %w",
				attempt, time.Since(start).Round(time.Millisecond), err)
		}
		// Full jitter in [backoff/2, backoff), never past the deadline.
		wait := backoff/2 + rand.N(backoff/2+1)
		if wait > remaining {
			wait = remaining
		}
		if serr := sleep(ctx, wait); serr != nil {
			return fmt.Errorf("gave up waiting for database lock after %d attempts: %w (last error: %v)", attempt, serr, err)
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}
