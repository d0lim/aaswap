package fsutil

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// jsonIndent matches what the Python implementation wrote (json.dumps with
// indent=2), so files stay diffable across the migration. Object *member
// order* does differ — Python emitted insertion order, this emits sorted keys
// (see Deterministic below) — which JSON treats as insignificant and both
// implementations parse identically.
const jsonIndent = "  "

// WriteJSONAtomic writes data as indented JSON, atomically, with the backup
// directory's 0600/0700 modes.
//
// Shared by settings.json, the auto-switch state file, the stash manifest, the
// usage store and session config — every machine-local JSON artifact aaswap
// owns.
//
// # It writes THROUGH a symlink, never over it
//
// A rename swaps a directory *entry* and does not follow links, so renaming
// onto a symlinked path detaches the link: the write succeeds, the content is
// right, and the link target silently stops receiving updates — until
// something restores the link (a dotfiles deploy), taking every change written
// since with it. Three consequences, each deliberate:
//
//   - A dangling link still writes where it points. Linking a path is a request
//     to write there.
//   - The temp file is created beside the *resolved* target, so the rename stays
//     on one filesystem and remains atomic. Beside the link it would hit EXDEV
//     whenever the target lives on another mount.
//   - The 0700 hardening stays on the directory aaswap owns — the link's parent,
//     not the resolved one. Applying it to the resolved parent would narrow a
//     directory belonging to something else, and fail outright when that parent
//     is not ours to chmod. The written file still gets 0600, and the temp file
//     is created 0600 to begin with, so the secret is never exposed.
func WriteJSONAtomic(path string, data any) error {
	// Deterministic sorts map keys. Without it json/v2 emits Go's randomized
	// map order, so every write would reshuffle the file and turn any diff of
	// a settings or state file into noise.
	encoded, err := json.Marshal(data, jsontext.WithIndent(jsonIndent), json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode JSON for %s: %w", path, err)
	}
	return WriteFileAtomic(path, encoded)
}

// WriteFileAtomic writes data to path, atomically, with the backup directory's
// 0600/0700 modes.
//
// It carries the same symlink and permission contract as [WriteJSONAtomic]; see
// that doc comment for why writing through a link matters and why the directory
// hardening lands on the link's parent rather than the resolved one.
func WriteFileAtomic(path string, data []byte) error {
	return writeAtomic(path, data, true)
}

// WriteForeignFileAtomic writes a file aaswap does not own — Claude Code's
// ~/.claude.json and ~/.claude/.credentials.json — atomically, at 0600, WITHOUT
// touching the containing directory's mode.
//
// The distinction is load-bearing, not stylistic. [WriteFileAtomic] narrows the
// containing directory to 0700, which is right for the backup root but wrong
// here: the parent of ~/.claude.json is the user's HOME DIRECTORY, and
// hardening it would silently change the permissions of everything the user
// keeps there. The file itself still gets 0600, so the secret is no less
// protected.
func WriteForeignFileAtomic(path string, data []byte) error {
	return writeAtomic(path, data, false)
}

func writeAtomic(path string, data []byte, hardenDir bool) error {
	target, err := resolveThroughSymlink(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("create directory for %s: %w", target, err)
	}
	if hardenDir && runtime.GOOS != "windows" {
		// The link's parent, not the target's — see WriteJSONAtomic.
		if err := os.Chmod(filepath.Dir(path), 0o700); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("harden directory %s: %w", filepath.Dir(path), err)
		}
	}

	tmp, err := os.CreateTemp(filepath.Dir(target), ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file beside %s: %w", target, err)
	}
	tmpName := tmp.Name()
	// From here on any failure must take the temp file with it, or a crashed
	// write would litter the backup root with partial content.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}
	if err := ReplaceFile(tmpName, target); err != nil {
		return fmt.Errorf("publish %s: %w", target, err)
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(target, 0o600); err != nil {
			return fmt.Errorf("restrict %s: %w", target, err)
		}
	}
	return nil
}

// resolveThroughSymlink returns the path a write should land on: the link's
// target when path is a symlink, and path itself otherwise.
func resolveThroughSymlink(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return path, nil
		}
		return "", fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&fs.ModeSymlink == 0 {
		return path, nil
	}
	// EvalSymlinks fails on a dangling link, but a dangling link still names
	// where the caller wants the write to go, so fall back to reading it.
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved, nil
	}
	dest, err := os.Readlink(path)
	if err != nil {
		return "", fmt.Errorf("read link %s: %w", path, err)
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(filepath.Dir(path), dest)
	}
	return filepath.Clean(dest), nil
}
