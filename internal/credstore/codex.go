package credstore

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/fsutil"
)

// Codex keeps its live credential in ONE plaintext file, and that is the whole
// store.
//
// Everything the Claude path does — the Keychain, the bounded retry, the
// capability cache, the managed-key axis, the .enc reconciliation — exists
// because Claude Code has two places a credential can be and they can disagree.
// Codex has one. So this is not a smaller version of that code; it is the
// absence of the problem that code solves.
//
// The two fields that carry that absence: Degraded is never set, because
// degraded means "these bytes may be a superseded generation, since a fresher
// store could not be asked" and there is no fresher store. KeychainUnavailable
// is never set, because there is no Keychain to be unavailable.

// readCodexActive reads Codex's live credential.
func (s *Store) readCodexActive() ActiveCredentials {
	path := s.paths.CodexAuthPath()
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		return ActiveCredentials{Value: string(data)}
	case os.IsNotExist(err):
		// A fresh machine has never logged in. Empty, not failed.
		return ActiveCredentials{}
	}
	// Present and unreachable: "nothing is stored" and "something is stored
	// but cannot be read" call for different advice.
	return ActiveCredentials{FileReadFailed: true}
}

// writeCodexActive replaces Codex's live credential.
func (s *Store) writeCodexActive(credentials string) error {
	path := s.paths.CodexAuthPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("%w: creating %s: %w", apperr.ErrCredentialWrite,
			filepath.Dir(path), err)
	}
	// Atomic and owner-only: this file holds a refresh token, and a torn write
	// would leave the tool unable to start.
	if err := fsutil.WriteFileAtomic(path, []byte(credentials)); err != nil {
		return fmt.Errorf("%w: writing %s: %w", apperr.ErrCredentialWrite, path, err)
	}
	return nil
}
