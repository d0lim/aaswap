package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// realHome is the developer's actual home directory, captured during package
// initialization — which runs before TestMain, and therefore before any test
// can call t.Setenv("HOME", ...). It is the fixed reference the guard compares
// against, exactly as the Python suite's conftest snapshotted the real store
// roots exactly once at import time.
var realHome = func() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Clean(home)
}()

// guardRealStore panics when a test binary resolves paths that point at the
// developer's own Claude Code profile or aaswap backups.
//
// This is the Go stand-in for the Python suite's sys.addaudithook safety net.
// aaswap manipulates live login credentials, so a test that forgets to
// redirect HOME would not merely fail — it could overwrite or delete the
// developer's real accounts. Python could intercept that at the syscall level;
// Go cannot, so the check sits at the single choke point where the environment
// becomes paths, and it fails loudly rather than silently proceeding.
//
// It costs one branch in production, where testing.Testing() is false.
func guardRealStore(r *Resolver) {
	if !testing.Testing() || realHome == "" {
		return
	}
	for label, path := range map[string]string{
		"home directory":     r.Home,
		"Claude config home": r.ClaudeConfigHome(),
		"backup root":        r.BackupRoot(),
		"legacy backup root": r.LegacyBackupRoot(),
	} {
		if withinRealHome(path) {
			panic(fmt.Sprintf(
				"aaswap test safety net: resolved %s to %q, inside the real "+
					"home directory %q.\n"+
					"A test must never touch the developer's live account store. "+
					"Build a Resolver over t.TempDir() with paths.New instead of "+
					"calling paths.FromEnv, or redirect HOME with t.Setenv first.",
				label, path, realHome))
		}
	}
}

// withinRealHome reports whether path is the real home directory or sits
// underneath it. Temp directories handed out by t.TempDir() live outside the
// home directory on every platform aaswap supports, so this does not
// produce false positives for correctly isolated tests.
func withinRealHome(path string) bool {
	if path == "" {
		return false
	}
	clean := filepath.Clean(path)
	if clean == realHome {
		return true
	}
	return strings.HasPrefix(clean, realHome+string(filepath.Separator))
}
