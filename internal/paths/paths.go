// Package paths resolves where Claude Code keeps its config and credentials,
// and where claude-swap keeps its backups.
//
// The resolution rules mirror Claude Code's own so cswap reads and writes the
// very same files (from the claude-code source):
//
//   - Config home: CLAUDE_CONFIG_DIR if set, else ~/.claude.
//   - Global config: <config-home>/.config.json when that legacy file exists,
//     otherwise (CLAUDE_CONFIG_DIR || $HOME)/.claude.json. Note the asymmetry:
//     .claude.json sits at the home directory by default, not inside .claude/.
//   - Credentials: <config-home>/.credentials.json.
//
// The claude-swap backup root follows the XDG Base Directory Specification on
// Linux and WSL ($XDG_DATA_HOME/claude-swap) and keeps the legacy
// ~/.claude-swap-backup on macOS and Windows.
//
// # Why a Resolver instead of package functions
//
// The Python original read os.environ and Path.home() at every call, and its
// test suite kept the developer's real account store safe with a runtime audit
// hook. Go has no such hook, so the environment is read exactly once — in
// [FromEnv] — and the result is threaded through the program as a value. A test
// constructs a Resolver over a temp directory and cannot reach the real store
// even by accident, because nothing below this package consults the
// environment. [FromEnv] additionally refuses to hand back the developer's real
// store when it is called from a test binary; see guard.go.
//
// References:
//   - claude-code utils/env.ts getGlobalClaudeFile
//   - claude-code utils/secureStorage/plainTextStorage.ts getStoragePath
//   - https://specifications.freedesktop.org/basedir-spec/basedir-spec-latest.html
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/realiti4/claude-swap/internal/apperr"
	"github.com/realiti4/claude-swap/internal/platform"
)

// LegacyBackupDirName is the pre-XDG backup directory, still the layout used on
// macOS and Windows.
const LegacyBackupDirName = ".claude-swap-backup"

// Resolver answers every "where does that file live" question from a fixed set
// of inputs. The zero value is not usable; build one with [FromEnv] in
// production or [New] in tests.
type Resolver struct {
	// Home is the user's home directory ($HOME, or %USERPROFILE% on Windows).
	Home string
	// ConfigDir is CLAUDE_CONFIG_DIR, or "" when unset. When set it relocates
	// Claude Code's entire profile, which is how session mode isolates accounts.
	ConfigDir string
	// XDGDataHome is $XDG_DATA_HOME, or "" when unset. Consulted only on
	// Linux and WSL.
	XDGDataHome string
	// SecureStorageConfigDir is $CLAUDE_SECURESTORAGE_CONFIG_DIR, and
	// SecureStorageConfigDirSet records whether it was defined at all.
	//
	// The two are separate because defined-but-empty is a distinct case with a
	// distinct meaning: Claude Code reads it as "use the default secure store",
	// whose Keychain item is the unsuffixed one. Collapsing it into "unset"
	// would send the lookup to the wrong item.
	SecureStorageConfigDir    string
	SecureStorageConfigDirSet bool
	// Platform selects the backup layout. Held as a field rather than detected
	// on demand so tests can exercise every platform's layout on one host.
	Platform platform.Platform
}

// New builds a Resolver for a given home directory and platform, leaving both
// environment overrides unset. Intended for tests.
func New(home string, p platform.Platform) *Resolver {
	return &Resolver{Home: home, Platform: p}
}

// FromEnv builds a Resolver from the process environment. This is the only
// place in claude-swap that reads these variables.
//
// In a test binary it panics rather than return a Resolver pointing at the
// developer's real account store — see [guardRealStore].
func FromEnv() (*Resolver, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate home directory: %w: %w", apperr.ErrConfig, err)
	}
	secure, secureSet := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	r := &Resolver{
		Home:                      home,
		ConfigDir:                 os.Getenv("CLAUDE_CONFIG_DIR"),
		XDGDataHome:               os.Getenv("XDG_DATA_HOME"),
		SecureStorageConfigDir:    secure,
		SecureStorageConfigDirSet: secureSet,
		Platform:                  platform.Detect(),
	}
	guardRealStore(r)
	return r, nil
}

// ClaudeConfigHome returns Claude Code's config home: CLAUDE_CONFIG_DIR when
// set, else ~/.claude.
func (r *Resolver) ClaudeConfigHome() string {
	if r.ConfigDir != "" {
		return r.ConfigDir
	}
	return filepath.Join(r.Home, ".claude")
}

// DefaultClaudeConfigHome returns the *default* profile's config home, ignoring
// CLAUDE_CONFIG_DIR.
//
// Credential capture needs to tell an env var naming the default profile from
// one naming a session profile, because only the former's credential belongs to
// the active store.
func (r *Resolver) DefaultClaudeConfigHome() string {
	return filepath.Join(r.Home, ".claude")
}

// GlobalConfigPath returns the global Claude config file, preferring the legacy
// <config-home>/.config.json when it exists.
func (r *Resolver) GlobalConfigPath() string {
	if legacy := filepath.Join(r.ClaudeConfigHome(), ".config.json"); exists(legacy) {
		return legacy
	}
	base := r.ConfigDir
	if base == "" {
		base = r.Home
	}
	return filepath.Join(base, ".claude.json")
}

// DefaultGlobalConfigPath returns the global config path of the default
// profile, with the same legacy fallback as [Resolver.GlobalConfigPath] but
// deliberately ignoring CLAUDE_CONFIG_DIR.
//
// Callers that mirror the user's real profile — session sharing — must not
// source from another session when they are themselves invoked from inside one.
func (r *Resolver) DefaultGlobalConfigPath() string {
	if legacy := filepath.Join(r.DefaultClaudeConfigHome(), ".config.json"); exists(legacy) {
		return legacy
	}
	return filepath.Join(r.Home, ".claude.json")
}

// CredentialsPath returns Claude Code's plaintext credential file. It is the
// live store on Linux, WSL and Windows, and a transient staging file on macOS,
// where the Keychain is authoritative.
func (r *Resolver) CredentialsPath() string {
	return filepath.Join(r.ClaudeConfigHome(), ".credentials.json")
}

// LegacyBackupRoot returns the pre-XDG backup root, ~/.claude-swap-backup.
func (r *Resolver) LegacyBackupRoot() string {
	return filepath.Join(r.Home, LegacyBackupDirName)
}

// BackupRoot returns claude-swap's backup root for this platform.
//
// Per the XDG spec, XDG_DATA_HOME is ignored when unset, empty, or not
// absolute. A leading ~ is expanded so values like "~/data" — set through
// systemd units or Dockerfiles, which get no shell expansion — still work.
func (r *Resolver) BackupRoot() string {
	if r.Platform.UsesXDG() {
		if xdg := r.expandTilde(r.XDGDataHome); filepath.IsAbs(xdg) {
			return filepath.Join(xdg, "claude-swap")
		}
		return filepath.Join(r.Home, ".local", "share", "claude-swap")
	}
	return r.LegacyBackupRoot()
}

// CacheDir is where cswap keeps regenerable state: the usage table and the
// update-check stamp.
//
// Everything under it is throwaway by construction. Deleting it costs at most
// one round of re-fetching, which is why the backup-root migration treats a
// target holding only this directory as empty.
func (r *Resolver) CacheDir() string {
	return filepath.Join(r.BackupRoot(), "cache")
}

// expandTilde resolves a leading ~ against this Resolver's home directory.
// An empty or plain-"~"-less value is returned unchanged.
func (r *Resolver) expandTilde(p string) string {
	if p == "" {
		return ""
	}
	if p == "~" {
		return r.Home
	}
	if rest, ok := strings.CutPrefix(p, "~"+string(os.PathSeparator)); ok {
		return filepath.Join(r.Home, rest)
	}
	// "~/" is accepted on every platform: the values that reach this code come
	// from config files and unit files written with forward slashes.
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(r.Home, rest)
	}
	return p
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
