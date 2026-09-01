package credstore

import (
	"path/filepath"

	"github.com/d0lim/ccswap/internal/keychain"
	"github.com/d0lim/ccswap/internal/paths"
	"github.com/d0lim/ccswap/internal/platform"
)

// Store owns the active credential store and the per-account backup store.
//
// One Store per process-level owner: the Keychain capability cache it holds is
// learned from real security(1) calls and is deliberately per-process, so a
// fresh process re-evaluates from scratch.
type Store struct {
	paths          *paths.Resolver
	platform       platform.Platform
	credentialsDir string
	kc             *keychain.Keychain
	cap            *capability

	// lastActiveBackend records where the most recent active-credential write
	// landed, "keychain" or "file", for the post-switch follow-up message.
	lastActiveBackend string
}

// New builds a Store over a resolver and a backup root.
//
// The Keychain handle is injectable so tests drive every branch with a fake
// runner; production passes keychain.New().
func New(r *paths.Resolver, backupRoot string, kc *keychain.Keychain) *Store {
	return &Store{
		paths:          r,
		platform:       r.Platform,
		credentialsDir: filepath.Join(backupRoot, "credentials"),
		kc:             kc,
		cap:            newCapability(r.Platform),
	}
}

// CredentialsDir is where per-account .enc backups live.
func (s *Store) CredentialsDir() string { return s.credentialsDir }

// LastActiveBackend reports where the most recent active-credential write
// landed: "keychain", "file", or "" when nothing has been written yet.
func (s *Store) LastActiveBackend() string { return s.lastActiveBackend }

// KeychainUnreadable reports that the Keychain could not be asked, so an empty
// read proves nothing. See capability.unreadable for the full argument.
func (s *Store) KeychainUnreadable() bool { return s.cap.unreadable() }
