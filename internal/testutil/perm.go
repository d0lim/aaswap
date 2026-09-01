package testutil

import (
	"io/fs"
	"os"
	"runtime"
	"testing"
)

// AssertPerm checks that a path carries exactly the POSIX permission bits
// given.
//
// It is a no-op on Windows. Go does not read a DACL to build os.FileMode there:
// it synthesizes one from the read-only attribute alone, so every file reports
// 0666 (0444 when read-only) and every directory 0777, whatever the code did.
// A literal comparison against 0600 fails on Windows no matter how the file was
// created, which makes the assertion noise rather than a check.
//
// The confidentiality being asserted is still real on Windows — it comes from
// the DACL a file inherits from its parent, and a per-user profile directory is
// already owner-only. That property just is not expressible as a mode, so it is
// verified on the platforms whose modes mean something and left to the
// filesystem on the one whose do not.
func AssertPerm(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s has mode %o, want %o", path, got, want)
	}
}

// AssertPermInfo is AssertPerm for a caller that already holds the FileInfo —
// a directory walk, say, where re-stating would be a second syscall and a
// second chance for the entry to change underneath.
func AssertPermInfo(t *testing.T, name string, info fs.FileInfo, want fs.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s has mode %o, want %o", name, got, want)
	}
}
