//go:build !windows

package testutil

import (
	"os"
	"testing"
)

// On POSIX a zero mode is the whole story, for files and directories alike —
// except for root, which ignores permission bits entirely.
func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root, where permission bits deny nothing")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	restore := info.Mode().Perm()
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatal(err)
	}
	// Registered after t.TempDir's own cleanup, so it runs BEFORE the removal
	// that would otherwise trip over an unsearchable directory.
	t.Cleanup(func() { _ = os.Chmod(path, restore) })
}
