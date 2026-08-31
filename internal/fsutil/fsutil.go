// Package fsutil holds filesystem primitives with no other claude-swap
// dependencies.
//
// Deliberately a leaf package: settings, credentials, mappings and session all
// need these helpers, and they sit on both sides of every other import edge.
//
// # Why reads and renames retry on Windows
//
// POSIX rename is genuinely atomic and never fails from contention. On Windows,
// antivirus (Defender) and the search indexer open freshly created files
// opportunistically, so a replace onto a just-written target fails with
// ERROR_ACCESS_DENIED or ERROR_SHARING_VIOLATION for a few milliseconds — it
// was measured at ~44% of replaces into a scanned temp directory, which made
// credential and usage-store writes fail intermittently for real users.
//
// The read side needs identical treatment: the account roster is published by
// renaming onto sequence.json, so the file is freshly modified exactly when the
// scanner grabs it, and a reader arriving in that window gets the same error. A
// failed roster read is worse than a failed write, because it turns a
// recoverable switch failure into a failed rollback.
//
// Only the contention codes are retried. Other errors (missing source,
// cross-device link) surface on the first attempt. A *persistent* access denial
// — ACLs, a read-only target — matches the retried codes and so surfaces only
// after the attempt budget, costing about 0.75s in the worst case.
package fsutil

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"time"
)

// Windows error codes that usually mean "someone else has this file open right
// now". AV and indexer contention clears within milliseconds;
// ERROR_ACCESS_DENIED can also be a persistent condition (ACLs, the read-only
// attribute), which still surfaces once the bounded retries are exhausted.
const (
	errorAccessDenied     syscall.Errno = 5
	errorSharingViolation syscall.Errno = 32
	errorLockViolation    syscall.Errno = 33
)

const (
	defaultAttempts     = 10
	defaultInitialDelay = 2 * time.Millisecond
	defaultMaxDelay     = 250 * time.Millisecond
)

// policy holds the retry knobs. Production code uses defaultPolicy; tests build
// one with a fake clock and their own transient predicate, so the loop is
// exercised on every host rather than only on Windows.
type policy struct {
	attempts     int
	initialDelay time.Duration
	maxDelay     time.Duration
	sleep        func(time.Duration)
	transient    func(error) bool
}

func defaultPolicy() policy {
	return policy{
		attempts:     defaultAttempts,
		initialDelay: defaultInitialDelay,
		maxDelay:     defaultMaxDelay,
		sleep:        time.Sleep,
		transient:    isTransientContention,
	}
}

// do runs op until it succeeds, until the error is not transient, or until the
// attempt budget is spent, backing off exponentially in between.
func (p policy) do(op func() error) error {
	if p.attempts < 1 {
		// A zero budget would skip the operation entirely and report success,
		// which for a credential write means silently losing the write.
		return fmt.Errorf("fsutil: attempts must be >= 1, got %d", p.attempts)
	}
	delay := p.initialDelay
	for attempt := range p.attempts {
		err := op()
		if err == nil {
			return nil
		}
		if attempt == p.attempts-1 || !p.transient(err) {
			return err
		}
		p.sleep(delay)
		delay = min(delay*2, p.maxDelay)
	}
	panic("unreachable: the loop returns on the final attempt")
}

// ReadText reads a UTF-8 file, retrying past transient Windows sharing
// failures.
func ReadText(path string) (string, error) {
	return readText(path, defaultPolicy())
}

func readText(path string, p policy) (string, error) {
	var out string
	err := p.do(func() error {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out = string(b)
		return nil
	})
	return out, err
}

// ReplaceFile renames src over dst, retrying past transient Windows sharing
// failures. This is the publish step of every atomic write in claude-swap.
func ReplaceFile(src, dst string) error {
	return replaceFile(src, dst, defaultPolicy())
}

func replaceFile(src, dst string, p policy) error {
	return p.do(func() error { return os.Rename(src, dst) })
}

// isTransientContention reports whether err is a Windows sharing/locking
// failure worth retrying. It is always false off Windows, where an EACCES is a
// genuine, persistent permission problem that must surface at once rather than
// after a ~0.75s stall.
func isTransientContention(err error) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	errno, ok := errors.AsType[syscall.Errno](err)
	if !ok {
		return false
	}
	switch errno {
	case errorAccessDenied, errorSharingViolation, errorLockViolation:
		return true
	}
	return false
}
