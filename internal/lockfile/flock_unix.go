//go:build !windows

package lockfile

import (
	"errors"
	"os"
	"syscall"
)

// tryLock takes a non-blocking exclusive advisory lock on the whole file.
//
// flock locks are owned by the open file description, so they are released
// automatically when the process exits — including on a crash, which is what
// keeps a killed aaswap from wedging the account store.
func tryLock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

func unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// isLockContention reports whether err means "another holder has it", as
// opposed to a real failure. flock signals contention with EWOULDBLOCK
// (== EAGAIN on every platform aaswap supports).
func isLockContention(err error) bool {
	return errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)
}
