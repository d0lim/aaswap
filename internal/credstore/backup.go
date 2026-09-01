package credstore

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/d0lim/ccswap/internal/apperr"
	"github.com/d0lim/ccswap/internal/fsutil"
	"github.com/d0lim/ccswap/internal/keychain"
	"github.com/d0lim/ccswap/internal/platform"
)

// Per-account backups have two backends: base64 .enc files under the
// credentials directory, and the macOS Keychain under BackupService.
//
// # Reads are .enc-wins
//
// On macOS a fallback .enc — written while the Keychain was unusable — is
// authoritative over a possibly-stale Keychain copy, so a Keychain that
// recovers cannot shadow a newer file. A successful Keychain write therefore
// has to reconcile the .enc away, which is correctness-critical rather than
// best-effort.
//
// Linux, WSL and Windows use .enc files only. Windows moved off the Credential
// Manager because it rejects entries over roughly 2,500 bytes (#45).

// backupEncPath is where a slot's fallback credential file lives.
func (s *Store) backupEncPath(accountNum, email string) string {
	return filepath.Join(s.credentialsDir, fmt.Sprintf(".creds-%s-%s.enc", accountNum, email))
}

// backupUsername is the Keychain account name for a slot's backup item.
func backupUsername(accountNum, email string) string {
	return fmt.Sprintf("account-%s-%s", accountNum, email)
}

// usesFileBackupBackend reports whether backup *writes* go to files rather than
// the Keychain.
//
// Linux, WSL, Windows and unknown platforms always use files. macOS uses the
// Keychain while it is usable and falls back to files when it is not (headless,
// SSH, a locked login keychain). Backup *reads* are .enc-wins regardless.
func (s *Store) usesFileBackupBackend() bool {
	return !s.cap.useKeychain()
}

// ReadAccount returns a slot's backup credential.
//
// The second return distinguishes "this read failed" from "there is nothing
// here". It is true when the read that produced an empty value actually FAILED:
// the .enc exists but could not be read (permissions, a mid-unmount — on every
// platform), or the .enc had nothing and the macOS Keychain read itself errored
// (locked, denied, timed out). A genuinely absent backup — no .enc, and the
// Keychain answering "not found" — reports false.
//
// Callers use the distinction to say "keychain unavailable, retry from a GUI
// session" instead of nudging the user into an unnecessary re-add. The .enc is
// the only backend off macOS, so its own read failure has to reach this verdict
// there too.
//
// The verdict is a return value rather than store state on purpose. As an
// instance flag it was shared by every thread, with a window spanning a 10-50ms
// security(1) subprocess: measured with one sibling reader against a genuinely
// denied slot, 2 of 60 reads came back "readable", and the consume gate then
// POSTed a spent grant.
func (s *Store) ReadAccount(accountNum, email string) (value string, unreadable bool) {
	encPath := s.backupEncPath(accountNum, email)
	failed := false

	// Stat, not an existence helper that swallows errors: an unsearchable
	// credentials directory must not be byte-identical to a genuinely absent
	// backup. That distinction is the whole point of the verdict.
	encPresent := true
	if _, err := os.Stat(encPath); err != nil {
		encPresent = false
		if !errors.Is(err, fs.ErrNotExist) {
			// The directory itself could not be searched — a real read failure,
			// the same as the arm below that fires once the file is known to
			// exist.
			failed = true
			slog.Warn("failed to read backup credentials file", "path", encPath, "error", err)
		}
	}

	if encPresent {
		encoded, err := fsutil.ReadText(encPath)
		if err != nil {
			// The .enc EXISTS but could not be read. This is the only backend
			// off macOS and it wins over the Keychain on macOS, so masking it
			// must not read as "absent" on any platform.
			failed = true
			slog.Warn("failed to read backup credentials file", "path", encPath, "error", err)
		} else if decoded, err := decodeBackup(encoded); err != nil {
			// Corrupt or garbled .enc: on macOS fall through to the Keychain
			// copy, which is the documented recovery. Content-level, not a read
			// failure, so it does NOT mark the verdict.
			slog.Warn("failed to decode backup credentials file", "path", encPath, "error", err)
		} else if decoded != "" {
			return decoded, false
		}
		// An empty or whitespace-only .enc is not a real backup: try the Keychain.
	}

	if s.platform == platform.MacOS {
		creds, err := s.readBackupKeychain(accountNum, email)
		if err != nil {
			failed = true
			slog.Warn("failed to read backup credentials from the Keychain",
				"account", accountNum, "error", err)
		} else if creds != "" {
			return creds, false
		}
	}
	return "", failed
}

// decodeBackup decodes a base64 backup payload.
//
// Strict decoding on purpose: a lenient decoder silently discards non-alphabet
// junk such as "!!!!" and yields empty bytes, which would let a corrupt .enc
// shadow a valid Keychain copy instead of falling through to it.
func decodeBackup(encoded string) (string, error) {
	trimmed := strings.TrimSpace(encoded)
	if trimmed == "" {
		return "", nil
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(trimmed)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// readBackupKeychain reads a slot's backup from the Keychain only, with no file
// fallback. An absent item is ("", nil); a Keychain failure is an error so the
// caller decides what it means.
func (s *Store) readBackupKeychain(accountNum, email string) (string, error) {
	return s.cap.observe(func() (string, error) {
		value, _, err := s.kc.Get(BackupService, backupUsername(accountNum, email))
		return value, err
	})
}

// WriteAccount stores a slot's backup credential.
//
// macOS writes the Keychain when it is usable and then reconciles the .enc
// away; when the Keychain is unusable, or the write fails, it falls back to the
// .enc file. Every other platform writes the file.
func (s *Store) WriteAccount(accountNum, email, credentials string) error {
	// Best effort, and never a precondition: the displaced generation is a
	// recovery cushion for a write that must happen either way.
	s.retainPreviousBackup(accountNum, email, credentials)

	if !s.usesFileBackupBackend() {
		_, err := s.cap.observe(func() (struct{}, error) {
			return struct{}{}, s.kc.Set(BackupService, backupUsername(accountNum, email), credentials)
		})
		if err == nil {
			return s.reconcileEncAfterKeychainWrite(accountNum, email, credentials)
		}
		if !errors.Is(err, keychain.ErrUnavailable) {
			return err // a defect, not a fallback condition
		}
		// observe has already flipped routing to file mode; fall through.
		slog.Warn("backup Keychain write failed, falling back to a file",
			"account", accountNum, "error", err)
	}
	return s.writeBackupEnc(accountNum, email, credentials)
}

// writeBackupEnc atomically writes a slot's base64 .enc backup at 0600.
func (s *Store) writeBackupEnc(accountNum, email, credentials string) error {
	encoded := base64.StdEncoding.EncodeToString([]byte(credentials))
	path := s.backupEncPath(accountNum, email)
	if err := fsutil.WriteFileAtomic(path, []byte(encoded)); err != nil {
		return fmt.Errorf("write backup credentials for account %s: %w: %w",
			accountNum, apperr.ErrCredentialWrite, err)
	}
	return nil
}

// reconcileEncAfterKeychainWrite stops a leftover .enc from shadowing a
// just-written Keychain backup.
//
// Reads are .enc-wins, which makes this correctness-critical rather than
// best-effort: delete the .enc; if the delete fails, atomically rewrite it with
// the same fresh credentials; if that also fails, return the error so the
// inconsistency surfaces rather than the store quietly serving stale bytes.
func (s *Store) reconcileEncAfterKeychainWrite(accountNum, email, credentials string) error {
	path := s.backupEncPath(accountNum, email)
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := os.Remove(path); err == nil {
		return nil
	} else {
		slog.Warn("could not delete the .enc after a Keychain backup write; "+
			"rewriting it with the fresh credentials to keep both consistent",
			"path", path, "error", err)
	}
	return s.writeBackupEnc(accountNum, email, credentials)
}

// DeleteAccount removes a slot's backup from both backends.
//
// Both are cleared regardless of which one the current platform writes to: a
// slot may hold an .enc from a period when the Keychain was unusable, and
// leaving either behind would let a removed account come back.
func (s *Store) DeleteAccount(accountNum, email string) error {
	var errs []error

	if err := os.Remove(s.backupEncPath(accountNum, email)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		errs = append(errs, fmt.Errorf("remove backup file: %w", err))
	}
	// The retained generation goes with the slot: leaving it would let a future
	// account landing on this number recover a credential that was never its.
	s.DeletePreviousBackup(accountNum, email)
	if s.platform == platform.MacOS {
		if _, err := s.cap.observe(func() (struct{}, error) {
			return struct{}{}, s.kc.Delete(BackupService, backupUsername(accountNum, email))
		}); err != nil {
			errs = append(errs, fmt.Errorf("remove backup Keychain item: %w", err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("delete backup for account %s: %w: %w",
			accountNum, apperr.ErrCredential, errors.Join(errs...))
	}
	return nil
}

// The three methods below are Keychain-only views of the backup store, for the
// one-time migration that relocates legacy items into the current service.
//
// Deliberately not routed through ReadAccount/WriteAccount: those are .enc-wins
// and would let a fallback .enc be mistaken for "already migrated", or divert
// the write away from the very service the migration exists to populate.

// ReadKeychainBackup reads a slot's backup from the Keychain only. An absent
// item is ("", nil); a Keychain failure is an error.
func (s *Store) ReadKeychainBackup(accountNum, email string) (string, error) {
	return s.readBackupKeychain(accountNum, email)
}

// WriteKeychainBackup writes a slot's backup to the Keychain only.
func (s *Store) WriteKeychainBackup(accountNum, email, credentials string) error {
	_, err := s.cap.observe(func() (struct{}, error) {
		return struct{}{}, s.kc.Set(BackupService, backupUsername(accountNum, email), credentials)
	})
	return err
}

// DeleteKeychainBackup removes a slot's backup Keychain item, best effort.
func (s *Store) DeleteKeychainBackup(accountNum, email string) {
	if err := s.kc.Delete(BackupService, backupUsername(accountNum, email)); err != nil {
		slog.Warn("failed to delete backup credentials from the Keychain",
			"account", accountNum, "error", err)
	}
}
