package session

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/d0lim/ccswap/internal/apperr"
)

// InvalidateForSlot re-points a slot's session profile at a credential that has
// just been replaced.
//
// A profile is seeded from the slot's backup at bootstrap and keeps its own
// copy. When the backup changes — a re-login, an import, a relocation, a
// consumed refresh grant — that copy becomes a PREDECESSOR generation. It still
// satisfies the local reuse check, because that check asks whether a credential
// is well-formed and unexpired, not whether the server has since rotated it
// out. So `ccswap run` would keep launching against a token that fails on its
// first refresh.
//
// Two outcomes, decided by whether anything is running against the profile:
//
//   - Quiet: drop the credential material so the next launch re-bootstraps from
//     the fresh backup. History, projects and settings are left alone — only the
//     credential is stale.
//   - Live: leave the running process's copy strictly alone and write a stale
//     marker instead. Claude Code manages that file while it runs, and pulling
//     credentials out from under it would be worse than the drift. The marker is
//     what makes the next quiet launch re-bootstrap.
//
// Reports what it did, so a caller can say so. Never fails the operation that
// caused it: a credential write is the thing the user asked for, and it must
// not be reported as failed because a profile could not be tidied.
func (m *Manager) InvalidateForSlot(accountNum, email string) (Outcome, error) {
	sessionDir := m.Dir(accountNum, email)
	// Absent OR unreachable: either way there is no profile this can act on,
	// and a credential write must not fail because a directory could not be
	// stat'ed.
	if !dirExists(sessionDir) {
		return NoProfile, nil
	}

	if len(m.LivePIDs(accountNum, email)) > 0 {
		if err := MarkStale(sessionDir); err != nil {
			return MarkFailed, err
		}
		return Marked, nil
	}

	if !m.MayHaveCredentialMaterial(sessionDir) {
		return AlreadyClear, nil
	}
	if err := m.dropCredentialMaterial(sessionDir); err != nil {
		// The drop failed, so the profile still holds the superseded
		// generation. The marker is the fallback that still forces a
		// re-bootstrap; without it the reuse check would hand the stale
		// credential straight back.
		if markErr := MarkStale(sessionDir); markErr != nil {
			return MarkFailed, fmt.Errorf("%w: dropping the profile credential: %w "+
				"(and it could not be marked stale: %w)", apperr.ErrSession, err, markErr)
		}
		slog.Warn("could not clear a session profile's credential; marked it stale instead",
			"account", accountNum, "error", err)
		return Marked, nil
	}
	return Cleared, nil
}

// Outcome says what an invalidation did.
type Outcome int

const (
	// NoProfile means the slot has no session profile at all.
	NoProfile Outcome = iota
	// AlreadyClear means the profile held no credential material.
	AlreadyClear
	// Cleared means the stale credential was dropped.
	Cleared
	// Marked means a live profile was flagged for re-bootstrap instead.
	Marked
	// MarkFailed means neither the drop nor the marker worked, so the profile
	// may keep serving the superseded generation.
	MarkFailed
)

// dirExists reports whether a path is there and readable.
func dirExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// dropCredentialMaterial removes a profile's credential, leaving everything
// else. Both stores, because macOS keeps one in the Keychain and a plaintext
// file can shadow it.
func (m *Manager) dropCredentialMaterial(sessionDir string) error {
	m.DeleteKeychainEntry(sessionDir)
	path := filepath.Join(sessionDir, ".credentials.json")
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
