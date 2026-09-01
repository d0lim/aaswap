package credstore

import (
	"path/filepath"

	"github.com/d0lim/aaswap/internal/keychain"
	"github.com/d0lim/aaswap/internal/paths"
	"github.com/d0lim/aaswap/internal/platform"
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
	// provider scopes this store to one auth domain. Empty is the unscoped
	// layout every store written before providers existed uses, and the
	// upgrade reads through a store built that way.
	provider string
	kc       *keychain.Keychain
	cap      *capability

	// lastActiveBackend records where the most recent active-credential write
	// landed, "keychain" or "file", for the post-switch follow-up message.
	lastActiveBackend string
}

// New builds a Store over a resolver and a backup root.
//
// The Keychain handle is injectable so tests drive every branch with a fake
// runner; production passes keychain.New().
func New(r *paths.Resolver, backupRoot string, kc *keychain.Keychain) *Store {
	return NewForProvider(r, backupRoot, kc, "")
}

// NewForProvider builds a Store scoped to one auth domain.
//
// Two providers can hold the same person's account under the same name — one
// address, one handle, two tools — so the place their credentials are filed has
// to differ. A directory per provider rather than a longer filename, because a
// person opening the store should be able to see which tool an account belongs
// to.
//
// An empty provider selects the unscoped layout. That is not a default anyone
// should choose: it is what a store written before providers existed looks
// like, and it exists so the upgrade can read one.
func NewForProvider(r *paths.Resolver, backupRoot string, kc *keychain.Keychain, provider string) *Store {
	dir := filepath.Join(backupRoot, "credentials")
	if provider != "" {
		dir = filepath.Join(dir, provider)
	}
	return &Store{
		paths:          r,
		platform:       r.Platform,
		credentialsDir: dir,
		provider:       provider,
		kc:             kc,
		cap:            newCapability(r.Platform),
	}
}

// Unscoped returns a view of the same store in the pre-provider layout.
//
// Only the upgrade should want this: it reads material a version 1 store filed
// before providers existed, and writes it back through the scoped store. A
// fresh capability cache because the two views probe the Keychain
// independently, and a failure reading the old layout says nothing about the
// new one.
func (s *Store) Unscoped() *Store {
	if s.provider == "" {
		return s
	}
	return NewForProvider(s.paths, filepath.Dir(s.credentialsDir), s.kc, "")
}

// CredentialsDir is where per-account .enc backups live.
func (s *Store) CredentialsDir() string { return s.credentialsDir }

// LastActiveBackend reports where the most recent active-credential write
// landed: "keychain", "file", or "" when nothing has been written yet.
func (s *Store) LastActiveBackend() string { return s.lastActiveBackend }

// KeychainUnreadable reports that the Keychain could not be asked, so an empty
// read proves nothing. See capability.unreadable for the full argument.
func (s *Store) KeychainUnreadable() bool { return s.cap.unreadable() }
