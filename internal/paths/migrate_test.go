package paths

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/realiti4/claude-swap/internal/apperr"
	"github.com/realiti4/claude-swap/internal/platform"
)

// migrateFixture builds a Linux resolver over a temp home and returns it
// alongside the legacy and XDG-target paths the migration moves between.
func migrateFixture(t *testing.T) (r *Resolver, legacy, target string) {
	t.Helper()
	home := t.TempDir()
	r = New(home, platform.Linux)
	return r, r.LegacyBackupRoot(), r.BackupRoot()
}

func flagPath(target string) string {
	return filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".migrating")
}

func TestMigrateNoLegacyIsNoop(t *testing.T) {
	r, _, target := migrateFixture(t)

	moved, err := r.MigrateLegacyBackupDir(target)
	if err != nil {
		t.Fatalf("MigrateLegacyBackupDir: %v", err)
	}
	if moved {
		t.Error("reported a move with no legacy directory present")
	}
	if exists_(target) {
		t.Error("target was created despite there being nothing to migrate")
	}
}

// On macOS and Windows the backup root *is* the legacy path. Migration must
// leave it completely alone.
func TestMigrateTargetEqualsLegacyIsNoop(t *testing.T) {
	home := t.TempDir()
	r := New(home, platform.MacOS)
	legacy := r.LegacyBackupRoot()
	writeFile(t, filepath.Join(legacy, "marker"), "keep me")

	moved, err := r.MigrateLegacyBackupDir(r.BackupRoot())
	if err != nil {
		t.Fatalf("MigrateLegacyBackupDir: %v", err)
	}
	if moved {
		t.Error("reported a move when target == legacy")
	}
	if got := readFile(t, filepath.Join(legacy, "marker")); got != "keep me" {
		t.Errorf("marker = %q, want %q", got, "keep me")
	}
}

func TestMigrateMovesLegacyToTarget(t *testing.T) {
	r, legacy, target := migrateFixture(t)
	writeFile(t, filepath.Join(legacy, "sequence.json"), `{"k": 1}`)
	writeFile(t, filepath.Join(legacy, "configs", "x.json"), "{}")

	moved, err := r.MigrateLegacyBackupDir(target)
	if err != nil {
		t.Fatalf("MigrateLegacyBackupDir: %v", err)
	}
	if !moved {
		t.Error("did not report a move")
	}
	if exists_(legacy) {
		t.Error("legacy directory survived the move")
	}
	if got := readFile(t, filepath.Join(target, "sequence.json")); got != `{"k": 1}` {
		t.Errorf("sequence.json = %q", got)
	}
	if got := readFile(t, filepath.Join(target, "configs", "x.json")); got != "{}" {
		t.Errorf("nested configs/x.json = %q", got)
	}
}

func TestMigrateCollisionRefuses(t *testing.T) {
	tests := []struct {
		name        string
		targetFiles map[string]string
	}{
		{
			name:        "plain collision",
			targetFiles: map[string]string{"sequence.json": `{"src": "target"}`},
		},
		{
			// Throwaway artifacts sitting next to real data is still a real
			// collision — only an all-throwaway target may be wiped.
			name: "real data alongside throwaway artifacts",
			targetFiles: map[string]string{
				"sequence.json":   `{"src": "target"}`,
				"claude-swap.log": "noise",
				"cache/probe":     "{}",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r, legacy, target := migrateFixture(t)
			writeFile(t, filepath.Join(legacy, "sequence.json"), `{"src": "legacy"}`)
			for name, content := range tt.targetFiles {
				writeFile(t, filepath.Join(target, filepath.FromSlash(name)), content)
			}

			_, err := r.MigrateLegacyBackupDir(target)
			if !errors.Is(err, apperr.ErrMigration) {
				t.Fatalf("error = %v, want it to wrap apperr.ErrMigration", err)
			}

			// Neither side may be touched on a refusal.
			if got := readFile(t, filepath.Join(legacy, "sequence.json")); got != `{"src": "legacy"}` {
				t.Errorf("legacy sequence.json = %q, want it untouched", got)
			}
			if got := readFile(t, filepath.Join(target, "sequence.json")); got != `{"src": "target"}` {
				t.Errorf("target sequence.json = %q, want it untouched", got)
			}
		})
	}
}

// Regression: any prior cswap run lays down cache/ and a log in the XDG path
// even with no real data, so a legacy directory arriving later (file sync from
// another machine) used to be reported as a collision.
func TestMigrateWipesThrowawayOnlyTarget(t *testing.T) {
	r, legacy, target := migrateFixture(t)
	writeFile(t, filepath.Join(legacy, "sequence.json"), `{"src": "legacy"}`)
	writeFile(t, filepath.Join(target, "cache", "update_check.json"), "{}")
	writeFile(t, filepath.Join(target, "claude-swap.log"), "noise")
	writeFile(t, filepath.Join(target, "claude-swap.log.1"), "rotated")

	moved, err := r.MigrateLegacyBackupDir(target)
	if err != nil {
		t.Fatalf("MigrateLegacyBackupDir: %v", err)
	}
	if !moved {
		t.Error("did not report a move")
	}
	if exists_(legacy) {
		t.Error("legacy directory survived the move")
	}
	if got := readFile(t, filepath.Join(target, "sequence.json")); got != `{"src": "legacy"}` {
		t.Errorf("sequence.json = %q, want the legacy contents", got)
	}
	for _, gone := range []string{"cache", "claude-swap.log", "claude-swap.log.1"} {
		if exists_(filepath.Join(target, gone)) {
			t.Errorf("throwaway artifact %q survived", gone)
		}
	}
}

// Flag present + legacy still there means a previous run died mid-move: discard
// the partial target and retry.
func TestMigrateResumesAfterInterruptedMove(t *testing.T) {
	r, legacy, target := migrateFixture(t)
	writeFile(t, filepath.Join(legacy, "sequence.json"), `{"src": "legacy"}`)
	writeFile(t, filepath.Join(target, "stale-partial.json"), "garbage")
	writeFile(t, flagPath(target), "")

	moved, err := r.MigrateLegacyBackupDir(target)
	if err != nil {
		t.Fatalf("MigrateLegacyBackupDir: %v", err)
	}
	if !moved {
		t.Error("did not report a move")
	}
	if exists_(legacy) {
		t.Error("legacy directory survived the move")
	}
	if exists_(flagPath(target)) {
		t.Error("migrating flag survived a successful move")
	}
	if exists_(filepath.Join(target, "stale-partial.json")) {
		t.Error("partial target contents survived the retry")
	}
	if got := readFile(t, filepath.Join(target, "sequence.json")); got != `{"src": "legacy"}` {
		t.Errorf("sequence.json = %q, want the legacy contents", got)
	}
}

// Flag present + legacy gone means the move completed but the run died before
// unlinking the flag. Clean the flag and leave the complete target alone.
func TestMigrateCleansStaleFlagAfterCompletedMove(t *testing.T) {
	r, _, target := migrateFixture(t)
	writeFile(t, filepath.Join(target, "sequence.json"), `{"complete": true}`)
	writeFile(t, flagPath(target), "")

	moved, err := r.MigrateLegacyBackupDir(target)
	if err != nil {
		t.Fatalf("MigrateLegacyBackupDir: %v", err)
	}
	if moved {
		t.Error("reported a move when there was nothing left to migrate")
	}
	if exists_(flagPath(target)) {
		t.Error("stale migrating flag was not cleaned up")
	}
	if got := readFile(t, filepath.Join(target, "sequence.json")); got != `{"complete": true}` {
		t.Errorf("sequence.json = %q, want the target untouched", got)
	}
}

// Filesystem failures must surface as apperr.ErrMigration rather than a raw
// syscall error, so the CLI prints a directed message instead of a traceback.
// A regular file standing where the target's parent directory belongs makes
// MkdirAll fail deterministically on every platform.
func TestMigrateWrapsFilesystemErrors(t *testing.T) {
	r, legacy, target := migrateFixture(t)
	writeFile(t, filepath.Join(legacy, "sequence.json"), "{}")

	blocker := filepath.Dir(target)
	if err := os.RemoveAll(blocker); err != nil {
		t.Fatalf("RemoveAll(%q): %v", blocker, err)
	}
	writeFile(t, blocker, "not a directory")

	_, err := r.MigrateLegacyBackupDir(target)
	if !errors.Is(err, apperr.ErrMigration) {
		t.Fatalf("error = %v, want it to wrap apperr.ErrMigration", err)
	}
	if got := readFile(t, filepath.Join(legacy, "sequence.json")); got != "{}" {
		t.Errorf("legacy was disturbed by the failed migration: %q", got)
	}
}

// The backup root holds 0600 credential files; their modes are part of the
// security posture and must survive the move.
func TestMigratePreservesFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not meaningful on Windows")
	}
	r, legacy, target := migrateFixture(t)
	credDir := filepath.Join(legacy, "credentials")
	cred := filepath.Join(credDir, ".creds-1-user@example.com.enc")
	writeFile(t, cred, "data")
	chmod(t, cred, 0o600)
	chmod(t, credDir, 0o700)

	if _, err := r.MigrateLegacyBackupDir(target); err != nil {
		t.Fatalf("MigrateLegacyBackupDir: %v", err)
	}

	moved := filepath.Join(target, "credentials", ".creds-1-user@example.com.enc")
	if got := mode(t, moved); got != 0o600 {
		t.Errorf("credential file mode = %#o, want 0600", got)
	}
	if got := mode(t, filepath.Join(target, "credentials")); got != 0o700 {
		t.Errorf("credentials dir mode = %#o, want 0700", got)
	}
}

// moveTree's cross-filesystem fallback is a separate code path from the plain
// rename, and it is the one that has to reproduce modes by hand.
func TestCopyTreePreservesModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not meaningful on Windows")
	}
	src, dst := filepath.Join(t.TempDir(), "src"), filepath.Join(t.TempDir(), "dst")
	nested := filepath.Join(src, "credentials")
	cred := filepath.Join(nested, "creds.enc")
	writeFile(t, cred, "secret")
	chmod(t, cred, 0o600)
	chmod(t, nested, 0o700)

	if err := copyTree(src, dst); err != nil {
		t.Fatalf("copyTree: %v", err)
	}
	if got := readFile(t, filepath.Join(dst, "credentials", "creds.enc")); got != "secret" {
		t.Errorf("copied content = %q", got)
	}
	if got := mode(t, filepath.Join(dst, "credentials", "creds.enc")); got != 0o600 {
		t.Errorf("copied file mode = %#o, want 0600", got)
	}
	if got := mode(t, filepath.Join(dst, "credentials")); got != 0o700 {
		t.Errorf("copied dir mode = %#o, want 0700", got)
	}
}

func TestIsThrowaway(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"cache", true},
		{"claude-swap.log", true},
		{"claude-swap.log.1", true},
		{"sequence.json", false},
		{"settings.json", false},
		{"credentials", false},
		{"caches", false}, // prefix match must not extend to unrelated names
	}
	for _, tt := range tests {
		if got := isThrowaway(tt.name); got != tt.want {
			t.Errorf("isThrowaway(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func chmod(t *testing.T, path string, perm os.FileMode) {
	t.Helper()
	if err := os.Chmod(path, perm); err != nil {
		t.Fatalf("Chmod(%q): %v", path, err)
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	return info.Mode().Perm()
}
