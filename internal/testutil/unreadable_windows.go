package testutil

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// Windows has no mode that denies a read: os.Chmod only toggles the read-only
// attribute, and a read-only file reads back fine. os.Geteuid also returns -1
// here, so the root guard the POSIX side uses never fires — a chmod-based test
// does not skip on Windows, it FAILS.
//
// The condition Windows actually produces is a sharing violation: another
// process holds the file with a share mode that excludes us. That is the real
// cause of an unreadable credential here — a virus scanner or a second Claude
// Code mid-write — and fsutil already knows the errno, so holding the handle
// exercises the production path rather than a stand-in for it.
func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.IsDir() {
		// A directory needs its ACL rewritten to become unlistable; an
		// exclusive handle does not stop enumeration. Not worth an icacls
		// dependency for one assertion — the file cases below carry the rule.
		t.Skip("making a directory unlistable on Windows needs an ACL rewrite")
	}

	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// dwShareMode 0: no other opener may read, write, or delete.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("could not take an exclusive handle on %s: %v", path, err)
	}
	// Registered after t.TempDir's own cleanup, so it runs BEFORE the removal
	// that an open handle would otherwise block.
	t.Cleanup(func() { _ = windows.CloseHandle(handle) })
}
