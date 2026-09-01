package paths

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/d0lim/aaswap/internal/apperr"
)

// throwawayNames and throwawayPrefixes are entries any prior aaswap run may have
// left in the backup root without user data being present: logger output and
// the update-check / usage cache. A target holding only these counts as empty,
// because wiping them loses no real state.
var (
	throwawayNames    = []string{"cache"}
	throwawayPrefixes = []string{"aaswap.log"}
)

// MigrateLegacyBackupDir moves ~/.claude-swap-backup to target when the layout
// has changed underneath an existing install (the Linux/WSL move to XDG).
//
// The move is guarded by a <target>.migrating flag file. Touching the flag
// before the move and removing it after is what lets the next run tell an
// interrupted migration apart from a genuine collision:
//
//   - Flag present, legacy still there — a previous run died mid-move. Discard
//     whatever partial target exists and retry.
//   - Flag present, legacy gone — the move completed but the run died before
//     cleaning up. Just unlink the flag.
//   - No flag, both paths exist — a real collision. Refuse, unless the target
//     holds nothing but throwaway artifacts, which happens when a fresh box ran
//     aaswap once (laying down cache/ and a log) and the legacy directory then
//     arrived from another machine via file sync. In that case wipe and migrate.
//
// It reports whether a move actually ran.
func (r *Resolver) MigrateLegacyBackupDir(target string) (bool, error) {
	legacy := r.LegacyBackupRoot()
	if samePath(legacy, target) {
		return false, nil
	}

	flag := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".migrating")

	if !exists(legacy) {
		// A prior run succeeded but died before unlinking the flag.
		if err := os.Remove(flag); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, migrationErr(legacy, target, err)
		}
		return false, nil
	}

	switch {
	case exists(flag):
		// Interrupted before completion: the target may be half-written, and
		// the legacy directory is still the authoritative copy.
		if err := os.RemoveAll(target); err != nil {
			return false, migrationErr(legacy, target, err)
		}
	case exists(target):
		meaningful, err := hasMeaningfulData(target)
		if err != nil {
			return false, migrationErr(legacy, target, err)
		}
		if meaningful {
			return false, fmt.Errorf(
				"both legacy (%s) and new (%s) backup paths exist; refusing to merge "+
					"or overwrite — inspect both and remove the stale one manually "+
					"before re-running: %w",
				legacy, target, apperr.ErrMigration)
		}
		if err := wipeThrowawayArtifacts(target); err != nil {
			return false, migrationErr(legacy, target, err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return false, migrationErr(legacy, target, err)
	}
	if err := touch(flag); err != nil {
		return false, migrationErr(legacy, target, err)
	}
	if err := moveTree(legacy, target); err != nil {
		return false, migrationErr(legacy, target, err)
	}
	if err := os.Remove(flag); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false, migrationErr(legacy, target, err)
	}
	return true, nil
}

func migrationErr(legacy, target string, err error) error {
	return fmt.Errorf("migration of %s -> %s failed: %w: %w", legacy, target, apperr.ErrMigration, err)
}

// samePath compares two paths with symlinks resolved, falling back to a
// lexical comparison when either side cannot be resolved (it does not exist
// yet, which is the common case for the target).
func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return ra == rb
}

// hasMeaningfulData reports whether target holds anything beyond throwaway
// artifacts. A missing or non-directory target holds nothing.
func hasMeaningfulData(target string) (bool, error) {
	entries, err := os.ReadDir(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || isNotDir(err) {
			return false, nil
		}
		return false, err
	}
	for _, entry := range entries {
		if !isThrowaway(entry.Name()) {
			return true, nil
		}
	}
	return false, nil
}

func isThrowaway(name string) bool {
	if slices.Contains(throwawayNames, name) {
		return true
	}
	for _, p := range throwawayPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// wipeThrowawayArtifacts empties target and removes it, so the subsequent move
// can land on the name.
func wipeThrowawayArtifacts(target string) error {
	entries, err := os.ReadDir(target)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || isNotDir(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if err := os.RemoveAll(filepath.Join(target, entry.Name())); err != nil {
			return err
		}
	}
	return os.Remove(target)
}

func isNotDir(err error) bool {
	return errors.Is(err, os.ErrInvalid) || strings.Contains(err.Error(), "not a directory")
}

func touch(path string) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// moveTree renames src to dst, falling back to a recursive copy-then-remove
// when the two sit on different filesystems (the XDG target can easily land on
// a different mount than the home directory).
//
// This is shutil.move's contract, and the fallback preserves permission bits
// because the backup root holds 0600 credential files whose modes are part of
// the security posture.
func moveTree(src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	} else if !isCrossDevice(err) {
		return err
	}
	if err := copyTree(src, dst); err != nil {
		return err
	}
	return os.RemoveAll(src)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(target, info.Mode().Perm())
		case info.Mode()&fs.ModeSymlink != 0:
			dest, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(dest, target)
		default:
			return copyFile(path, target, info.Mode().Perm())
		}
	})
}

func copyFile(src, dst string, perm fs.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close() // best effort: the copy already failed
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	// OpenFile's perm is masked by umask; set the mode explicitly so a 0600
	// credential file does not widen to 0644 on a permissive umask.
	return os.Chmod(dst, perm)
}

// isCrossDevice reports whether a rename failed only because source and
// destination live on different filesystems, which is the one case worth
// retrying as a copy. POSIX reports EXDEV; Windows reports
// ERROR_NOT_SAME_DEVICE, which Go surfaces as the same errno value.
func isCrossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}

// AdoptStore moves a foreign store's directory tree into this resolver's backup
// root, and reports where it came from.
//
// Moved, not copied. Both trees would hold the same OAuth refresh tokens, and a
// refresh ROTATES the token — so whichever tool refreshed first would silently
// invalidate the other copy, and the other would then report a live account as
// dead. One store, one truth.
//
// Refuses when this resolver's root already holds a roster: merging two account
// tables is a decision about which slot wins, and nothing here is entitled to
// make it.
func (r *Resolver) AdoptStore(source string) error {
	target := r.BackupRoot()
	if samePath(source, target) {
		return fmt.Errorf("%w: the store is already aaswap's own", apperr.ErrConfig)
	}
	if exists(filepath.Join(target, RosterFileName)) {
		return fmt.Errorf("%w: aaswap already has accounts at %s; "+
			"remove or move that store first", apperr.ErrConfig, target)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return fmt.Errorf("%w: preparing %s: %w", apperr.ErrConfig, target, err)
	}
	// A target with nothing but a cache and a log is not a store — it is what a
	// single `aaswap list` leaves on a fresh machine, and it must not block an
	// import.
	if exists(target) {
		if err := wipeThrowawayArtifacts(target); err != nil {
			return fmt.Errorf("%w: clearing %s: %w", apperr.ErrConfig, target, err)
		}
		if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%w: %s is not empty: %w", apperr.ErrConfig, target, err)
		}
	}
	if err := moveTree(source, target); err != nil {
		return fmt.Errorf("%w: moving %s to %s: %w", apperr.ErrConfig, source, target, err)
	}
	return nil
}
