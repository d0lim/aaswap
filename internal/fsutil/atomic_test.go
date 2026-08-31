package fsutil

import (
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteJSONAtomicFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	data := map[string]any{
		"schemaVersion": 1,
		"autoswitch":    map[string]any{"threshold": 90, "strategy": "best"},
	}
	if err := WriteJSONAtomic(path, data); err != nil {
		t.Fatalf("WriteJSONAtomic: %v", err)
	}

	// Two-space indent and ": " separators, matching what the Python
	// implementation wrote, so the files stay diffable across the migration.
	want := `{
  "autoswitch": {
    "strategy": "best",
    "threshold": 90
  },
  "schemaVersion": 1
}`
	if got := readFileString(t, path); got != want {
		t.Errorf("written JSON =\n%s\nwant\n%s", got, want)
	}
}

// Randomized map order would reshuffle the file on every write, turning any
// diff of a settings or state file into noise.
func TestWriteJSONAtomicIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	data := map[string]any{"z": 1, "a": 2, "m": 3, "b": 4, "y": 5, "c": 6}

	var first string
	for i := range 5 {
		path := filepath.Join(dir, "state.json")
		if err := WriteJSONAtomic(path, data); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		got := readFileString(t, path)
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("write %d produced different bytes:\n%s\nfirst:\n%s", i, got, first)
		}
	}
}

func TestWriteJSONAtomicCreatesParents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "state.json")
	if err := WriteJSONAtomic(path, map[string]any{"ok": true}); err != nil {
		t.Fatalf("WriteJSONAtomic: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file was not created under missing parents: %v", err)
	}
}

// The backup root holds credentials; 0600/0700 is part of the security posture
// and must not depend on the caller's umask.
func TestWriteJSONAtomicModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not meaningful on Windows")
	}
	dir := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	if err := WriteJSONAtomic(path, map[string]any{"ok": true}); err != nil {
		t.Fatalf("WriteJSONAtomic: %v", err)
	}

	if got := statMode(t, path); got != 0o600 {
		t.Errorf("file mode = %#o, want 0600", got)
	}
	if got := statMode(t, dir); got != 0o700 {
		t.Errorf("directory mode = %#o, want 0700", got)
	}
}

func TestWriteJSONAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := WriteJSONAtomic(path, map[string]any{"ok": true}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "state.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory holds %v, want only state.json", names)
	}
}

// Renaming onto a symlink would detach it: the write succeeds, the content is
// right, and the link target silently stops receiving updates until a dotfiles
// deploy restores the link and takes every change with it.
func TestWriteJSONAtomicWritesThroughASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	linkDir := filepath.Join(dir, "link")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(linkDir, 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(realDir, "settings.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(linkDir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := WriteJSONAtomic(link, map[string]any{"wrote": "through"}); err != nil {
		t.Fatalf("WriteJSONAtomic: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a regular file — the link was detached")
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(readFileString(t, target)), &got); err != nil {
		t.Fatal(err)
	}
	if got["wrote"] != "through" {
		t.Errorf("link target = %v, want the write to have landed there", got)
	}
}

// A dangling link still names where the caller wants the write to go; linking
// a path is a request to write there.
func TestWriteJSONAtomicFollowsADanglingSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs elevation on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real", "settings.json")
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	if err := WriteJSONAtomic(link, map[string]any{"ok": true}); err != nil {
		t.Fatalf("WriteJSONAtomic on a dangling link: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("write did not land at the dangling link's target: %v", err)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return string(b)
}

func statMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	return info.Mode().Perm()
}
