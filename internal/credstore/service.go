package credstore

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"

	"golang.org/x/text/unicode/norm"

	"github.com/realiti4/claude-swap/internal/paths"
)

// KeychainServiceName returns the Keychain service name Claude Code derives for
// a given config directory.
//
// Claude Code hashes the raw CLAUDE_CONFIG_DIR value — NFC-normalized and
// *unresolved* (claude src envUtils.ts / macOsKeychainHelpers.ts). Hash exactly
// the string that gets exported, never a resolved or realpath variant, and
// never a cleaned one: a trailing slash or a leading ./ changes the hash, and
// therefore the item.
//
// It takes a string rather than a path type for that reason — a path round trip
// would silently drop those characters and send the lookup to a different item.
func KeychainServiceName(configDir string) string {
	normalized := norm.NFC.String(configDir)
	sum := sha256.Sum256([]byte(normalized))
	return "Claude Code-credentials-" + hex.EncodeToString(sum[:])[:8]
}

// ActiveProfileIsDefault reports whether the active config home is the default
// profile's.
//
// CLAUDE_CONFIG_DIR pointed at the default profile is still the default
// profile, so this keys on where the path resolves rather than on the variable
// being set. Resolution also collapses symlinks, which is how a profile reached
// through one shows up as itself.
//
// An unresolvable path answers false: treating an unknown profile as the
// default is what would license reading another account's credential, and that
// is the failure this guards.
func ActiveProfileIsDefault(r *paths.Resolver) bool {
	active := resolveLenient(r.ClaudeConfigHome())
	def := resolveLenient(r.DefaultClaudeConfigHome())
	return active != "" && active == def
}

// resolveLenient resolves symlinks as far as the filesystem allows, then
// re-appends whatever does not exist yet.
//
// filepath.EvalSymlinks fails outright on a missing path, but the comparison
// above has to work on a fresh machine where ~/.claude does not exist yet — the
// two sides are then equal and the profile *is* the default. This mirrors the
// non-strict resolution the original relied on.
func resolveLenient(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		return filepath.Clean(path)
	}
	remainder := ""
	for current := abs; ; {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(resolved, remainder)
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Walked to the root without finding anything that exists.
			return abs
		}
		remainder = filepath.Join(filepath.Base(current), remainder)
		current = parent
	}
}

// ActiveOAuthKeychainServices returns the Keychain services that may hold the
// OAuth credential for the active environment, in the order they should be
// tried.
//
// Claude Code (2.1.220 getMacOsKeychainStorageServiceName) sources secure
// storage from CLAUDE_SECURESTORAGE_CONFIG_DIR when that is *defined*, else
// from CLAUDE_CONFIG_DIR. Defined-but-empty selects the default secure store,
// whose item is the unsuffixed one.
//
// More than one name is returned for exactly one case: an explicit
// CLAUDE_CONFIG_DIR naming the default profile. Claude Code hashes the exported
// string, so it would write a *suffixed* item there, but a user who has always
// used the default profile may only have the unsuffixed one — so both are
// tried.
//
// A defined CLAUDE_SECURESTORAGE_CONFIG_DIR gets no such fallback: it names the
// only store Claude Code will read for this environment, so a miss means Claude
// Code sees a logged-out profile, and reaching into another store would report a
// credential Claude Code is not using.
func ActiveOAuthKeychainServices(r *paths.Resolver) []string {
	if r.SecureStorageConfigDirSet {
		if r.SecureStorageConfigDir == "" {
			return []string{ClaudeOAuthService}
		}
		return []string{KeychainServiceName(r.SecureStorageConfigDir)}
	}
	if r.ConfigDir == "" {
		return []string{ClaudeOAuthService}
	}
	services := []string{KeychainServiceName(r.ConfigDir)}
	if ActiveProfileIsDefault(r) {
		services = append(services, ClaudeOAuthService)
	}
	return services
}
