package credstore

import (
	"encoding/base64"
	"errors"
	"io/fs"
	"log/slog"
	"os"

	"github.com/realiti4/claude-swap/internal/fsutil"
	"github.com/realiti4/claude-swap/internal/platform"
)

// The retained previous generation.
//
// Overwriting a slot's backup replaces the only stored copy of a refresh token.
// When the write turns out to have been the wrong call — a misclassified
// switch-time backup, say — the displaced generation is the only way back, so
// one generation is kept beside the current backup.
//
// It is a cushion, never a contract: the write it protects is NEVER conditional
// on the retention succeeding.

func (s *Store) prevBackupPath(accountNum, email string) string {
	return s.backupEncPath(accountNum, email) + ".prev"
}

func prevBackupUsername(accountNum, email string) string {
	return backupUsername(accountNum, email) + ".prev"
}

// retainPreviousBackup keeps a slot's current backup as the previous generation
// before it is replaced.
//
// When the current generation cannot be READ — a locked Keychain, an unreadable
// file — nothing is retained, and that is the deliberate answer rather than a
// gap waiting to be filled. Checkpointing the INCOMING bytes instead was tried
// and withdrawn twice: once it overwrote a genuine previous generation with a
// duplicate of the incoming ones, and once, guarded, it still fired on a locked
// Keychain and wrote a plaintext copy that then WON over the real Keychain copy
// by the .enc-wins rule and shadowed it even after the lock cleared.
//
// The honest position is that the true previous generation is unrecoverable
// here. A cushion that can silently outrank a real one is worse than the
// absence it was meant to fill.
func (s *Store) retainPreviousBackup(accountNum, email, incoming string) {
	current, unreadable := s.ReadAccount(accountNum, email)
	if unreadable {
		slog.Warn("could not retain the previous credential generation: the current "+
			"backup exists but could not be read (not absent), so no recovery copy "+
			"will exist for this write", "account", accountNum)
		return
	}
	if current == "" || current == incoming {
		return
	}

	if !s.usesFileBackupBackend() {
		if _, err := s.cap.observe(func() (struct{}, error) {
			return struct{}{}, s.kc.Set(BackupService, prevBackupUsername(accountNum, email), current)
		}); err != nil {
			slog.Warn("failed to retain the previous credential generation",
				"account", accountNum, "error", err)
		}
		return
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(current))
	if err := fsutil.WriteFileAtomic(s.prevBackupPath(accountNum, email), []byte(encoded)); err != nil {
		slog.Warn("failed to retain the previous credential generation",
			"account", accountNum, "error", err)
	}
}

// ReadPreviousBackup reads a slot's retained previous generation, returning
// empty when there is none or it is unusable.
//
// File-wins, like the main backup read: a copy written while the Keychain was
// unusable beats a possibly-stale Keychain one.
func (s *Store) ReadPreviousBackup(accountNum, email string) string {
	if encoded, err := fsutil.ReadText(s.prevBackupPath(accountNum, email)); err == nil {
		if decoded, err := decodeBackup(encoded); err == nil {
			return decoded
		}
		slog.Warn("the retained previous credential generation is corrupt", "account", accountNum)
	}
	if s.usesFileBackupBackend() {
		return ""
	}
	value, err := s.cap.observe(func() (string, error) {
		v, found, err := s.kc.Get(BackupService, prevBackupUsername(accountNum, email))
		if err != nil || !found {
			return "", err
		}
		return v, nil
	})
	if err != nil {
		return ""
	}
	return value
}

// DeletePreviousBackup drops a slot's retained generation.
//
// Called after a relocation, where the retained copy holds the DISPLACED
// material — another account's credential, or a stale one — which recovery must
// never resurrect onto the key's new owner.
func (s *Store) DeletePreviousBackup(accountNum, email string) {
	if err := os.Remove(s.prevBackupPath(accountNum, email)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("failed to drop the retained previous credential generation",
			"account", accountNum, "error", err)
	}
	if s.platform == platform.MacOS {
		if err := s.kc.Delete(BackupService, prevBackupUsername(accountNum, email)); err != nil {
			slog.Warn("failed to drop the retained previous Keychain generation",
				"account", accountNum, "error", err)
		}
	}
}
