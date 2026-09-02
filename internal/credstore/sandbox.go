package credstore

import (
	"errors"
	"io/fs"
	"log/slog"
	"os"

	"github.com/d0lim/aaswap/internal/keychain"
	"github.com/d0lim/aaswap/internal/paths"
	"github.com/d0lim/aaswap/internal/platform"
)

// At is this store reading its LIVE credential through another resolver —
// one whose provider home is a login sandbox — while filing backups exactly
// where this store files them.
//
// The backup side is shared on purpose: what a sandboxed login produces is
// stored as an ordinary account, in the same vault, under the same provider.
func (s *Store) At(r *paths.Resolver, layout Layout) *Store {
	return NewForProvider(r, s.backupRoot, s.kc, s.provider, layout)
}

// DiscardActive removes the live credential this store reads: the Keychain
// item Claude Code derived for its config directory, and the credential file.
//
// For a sandbox, and nothing else. A login sandbox is a home the tool was
// pointed at for one login, and once the credential has been filed against an
// account the sandbox's copy is a second, unmanaged copy of a live token — in
// a directory about to be deleted, and on macOS in a Keychain item nothing
// would ever find again. Best effort: a down Keychain leaves the item, which
// is the documented residual.
func (s *Store) DiscardActive() {
	if s.layout.Keychain && s.platform == platform.MacOS {
		for _, service := range ActiveOAuthKeychainServices(s.paths) {
			if err := s.kc.Delete(service, keychain.AccountName()); err != nil {
				slog.Debug("could not remove a sandbox Keychain item", "service", service, "error", err)
			}
		}
	}
	if err := os.Remove(s.livePath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Debug("could not remove a sandbox credential file", "path", s.livePath(), "error", err)
	}
}
