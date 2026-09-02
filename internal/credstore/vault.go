package credstore

import (
	"fmt"
	"os"
	"path/filepath"
)

// VaultDirName is the directory holding one directory per stored account.
//
// Separate from the working directory the stash manifest and the consume locks
// live in, because those are not account files: putting them under a name that
// a walk treats as "one account per entry" would make the stash look like an
// account with no credential.
const VaultDirName = "vault"

// VaultDir is where this provider's account directories live.
func (s *Store) VaultDir() string {
	return filepath.Join(s.backupRoot, VaultDirName, s.providerSegment())
}

// AccountDir is one account's own directory.
//
// Named by BOTH the account name and the address, matching the flat layout this
// replaced. Either alone would merge two distinct accounts: one address can be
// held under two names after a rename, and one name can be reused for a
// different address after a removal.
func (s *Store) AccountDir(name, email string) string {
	return filepath.Join(s.VaultDir(), accountSegment(name, email))
}

// accountSegment is an account's directory name.
//
// The name and address verbatim, exactly as the flat filename carried them.
// Sanitising them here would silently merge two accounts whose sanitised forms
// collide, and the layer above has already refused a name that is not
// [a-z0-9_.-] — see swap.NormalizeName.
func accountSegment(name, email string) string {
	return fmt.Sprintf("%s-%s", name, email)
}

// providerSegment keys the vault by provider.
//
// Never empty: an unscoped store does not use the vault at all — see
// Store.legacy — so reaching here without a provider is a caller that has one
// and did not pass it, and collapsing to the vault root would file its accounts
// where a walk reads them as some other provider's.
func (s *Store) providerSegment() string {
	if s.provider == "" {
		return unattributedSegment
	}
	return s.provider
}

// unattributedSegment holds accounts filed by a store with no provider, which
// should not happen. Named rather than empty so the mistake is visible on disk
// instead of silently merging into another provider's directory.
const unattributedSegment = "_unattributed"

// ensureAccountDir creates an account's directory, owner-only.
func (s *Store) ensureAccountDir(name, email string) error {
	dir := s.AccountDir(name, email)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create the account directory %s: %w", dir, err)
	}
	// MkdirAll honours the umask, so a permissive one would leave the directory
	// group- or world-readable with a refresh token inside it.
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("restrict the account directory %s: %w", dir, err)
	}
	return nil
}

// removeAccountDir deletes an account's directory and everything in it.
//
// Everything, because the directory holds only files aaswap put there for this
// account. A directory left behind is a credential nothing names any more.
func (s *Store) removeAccountDir(name, email string) error {
	return os.RemoveAll(s.AccountDir(name, email))
}

// BackupPath is the file holding an account's stored credential.
//
// Exported because "where did aaswap put this" is a question diagnostics and
// tests have to answer without reconstructing the layout — reconstructing it is
// how a test comes to assert against a path the store stopped using.
func (s *Store) BackupPath(name, email string) string {
	return s.backupEncPath(name, email)
}
