// Package lockfile provides the two kinds of exclusion aaswap needs.
//
// [FileLock] is aaswap's own cross-process lock over its account store: a
// conventional advisory lock on a file, held while the roster and credentials
// are mutated.
//
// [WithClaudeCredentials] and [WithClaudeConfig] are different animals — they
// hold *Claude Code's* locks, using Claude Code's protocol, so that a swap
// never interleaves with a running Claude Code's token refresh. See claude.go.
package lockfile

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/d0lim/aaswap/internal/apperr"
)

// DefaultTimeout bounds how long a caller waits for aaswap's own lock.
const DefaultTimeout = 10 * time.Second

// pollInterval is how often a waiter re-tries the lock. Advisory locks give no
// readiness signal, so waiting is a poll; 100ms is short enough to feel
// instant and long enough not to spin.
const pollInterval = 100 * time.Millisecond

// FileLock is an exclusive, cross-process lock over a single file.
//
// The lock is advisory and tied to the open file description, so the operating
// system drops it if the process dies — a crashed aaswap cannot leave the
// account store permanently locked.
type FileLock struct {
	path    string
	timeout time.Duration
	file    *os.File
}

// New returns an unheld lock over path. A zero or negative timeout means
// [DefaultTimeout].
func New(path string, timeout time.Duration) *FileLock {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &FileLock{path: path, timeout: timeout}
}

// Held reports whether this FileLock currently holds the lock.
//
// Nothing in production asks: a caller that acquired knows, and one that did
// not has the error. It exists so a test can assert the release actually
// released, which is the property the whole type is for.
func (l *FileLock) Held() bool { return l.file != nil }

// Acquire blocks until the lock is taken or the timeout expires.
//
// It reports whether the lock was acquired; a false with a nil error is a
// timeout, which callers that can proceed without exclusion may tolerate. A
// non-nil error means the lock file itself could not be opened.
func (l *FileLock) Acquire() (bool, error) {
	if l.file != nil {
		return true, nil
	}
	if err := os.MkdirAll(filepath.Dir(l.path), 0o700); err != nil {
		return false, fmt.Errorf("create lock directory: %w: %w", apperr.ErrLock, err)
	}
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false, fmt.Errorf("open lock file %s: %w: %w", l.path, apperr.ErrLock, err)
	}

	deadline := time.Now().Add(l.timeout)
	for {
		err := tryLock(f)
		if err == nil {
			l.file = f
			return true, nil
		}
		if !isLockContention(err) {
			_ = f.Close() // best effort: the lock was never taken
			return false, fmt.Errorf("lock %s: %w: %w", l.path, apperr.ErrLock, err)
		}
		if time.Now().After(deadline) {
			_ = f.Close() // best effort: the lock was never taken
			return false, nil
		}
		time.Sleep(pollInterval)
	}
}

// Release drops the lock. Releasing an unheld lock is a no-op, so a deferred
// Release is always safe.
func (l *FileLock) Release() error {
	if l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	// Unlock first, then close: closing also drops the lock, but doing it
	// explicitly keeps the failure attributable.
	unlockErr := unlock(f)
	closeErr := f.Close()
	if unlockErr != nil {
		return fmt.Errorf("unlock %s: %w: %w", l.path, apperr.ErrLock, unlockErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close lock file %s: %w: %w", l.path, apperr.ErrLock, closeErr)
	}
	return nil
}

// With runs fn while holding an exclusive lock over path, releasing it
// afterwards even if fn panics.
//
// A timeout is an error here rather than a silent skip: every caller in
// aaswap mutates the account store, and doing that unlocked is what the
// lock exists to prevent.
func With(path string, timeout time.Duration, fn func() error) (err error) {
	l := New(path, timeout)
	acquired, err := l.Acquire()
	if err != nil {
		return err
	}
	if !acquired {
		return fmt.Errorf(
			"could not acquire %s within %s — another aaswap may be running: %w",
			filepath.Base(path), l.timeout, apperr.ErrLock)
	}
	defer func() {
		// A failed release leaves the store locked for the next caller, so it
		// is reported — but never at the cost of masking the callback's own
		// error, which is what the user actually asked about.
		if releaseErr := l.Release(); releaseErr != nil && err == nil {
			err = releaseErr
		}
	}()
	return fn()
}
