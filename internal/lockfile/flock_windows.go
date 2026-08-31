//go:build windows

package lockfile

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLock takes a non-blocking exclusive lock on the first byte of the file.
//
// Windows has no flock; LockFileEx is the equivalent, and locking a single byte
// (rather than the whole range) matches what the Python implementation did
// through msvcrt.locking, so a Go and a Python cswap on the same machine still
// exclude each other during the migration period.
func tryLock(f *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.LockFileEx(
		windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, overlapped,
	)
}

func unlock(f *os.File) error {
	overlapped := new(windows.Overlapped)
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, overlapped)
}

// isLockContention reports whether err means another holder has the lock.
// LOCKFILE_FAIL_IMMEDIATELY surfaces contention as ERROR_LOCK_VIOLATION.
func isLockContention(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
