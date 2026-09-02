// Package paths resolves where Claude Code keeps its config and credentials,
// and where aaswap keeps its backups.
//
// The resolution rules mirror Claude Code's own so aaswap reads and writes the
// very same files (from the claude-code source):
//
//   - Config home: CLAUDE_CONFIG_DIR if set, else ~/.claude.
//   - Global config: <config-home>/.config.json when that legacy file exists,
//     otherwise (CLAUDE_CONFIG_DIR || $HOME)/.claude.json. Note the asymmetry:
//     .claude.json sits at the home directory by default, not inside .claude/.
//   - Credentials: <config-home>/.credentials.json.
//
// The aaswap backup root follows the XDG Base Directory Specification on
// Linux and WSL ($XDG_DATA_HOME/aaswap) and uses ~/.aaswap-backup on macOS and
// Windows.
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
	"maps"
	"os"
	"path/filepath"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/platform"
)

// Backup directory names.
//
// aaswap keeps its own store, separate from the claude-swap project it was
// forked from. That is not cosmetic. Both projects stamp the same schema
// version numbers into settings.json, usage.json and the roster, and the
// version is how each decides whether a file is one it understands — the
// Python implementation discards a usage table whose version it does not
// recognise. Two independently evolving projects cannot both own that number,
// so the first one to bump it would silently wipe the other's state. Sharing a
// backup root only looks like compatibility.
//
// A predecessor's store is therefore FOREIGN. See [Resolver.Predecessors] and
// the `aaswap account adopt` command, which moves one over on request.
const (
	// BackupDirName is the XDG data directory, used on Linux and WSL.
	BackupDirName = "aaswap"
	// RosterFileName is the account table's name inside a backup root. Named
	// here because "is there a store at this path" is a question about
	// locations, and both aaswap's own roots and a foreign one answer it.
	RosterFileName = "sequence.json"
	// LegacyBackupDirName is the pre-XDG backup directory, still the layout
	// used on macOS and Windows.
	LegacyBackupDirName = ".aaswap-backup"
)

// Resolver answers every "where does that file live" question from a fixed set
// of inputs. The zero value is not usable; build one with [FromEnv] in
// production or [New] in tests.
type Resolver struct {
	// Home is the user's home directory ($HOME, or %USERPROFILE% on Windows).
	Home string
	// CodexHomeDir is CODEX_HOME, or "" when unset. Codex's counterpart to
	// CLAUDE_CONFIG_DIR, and the same warning applies: it relocates a live
	// tool's whole directory.
	CodexHomeDir string

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
	// HomeOverrides holds the home-relocating environment variable of every
	// provider this build offers, keyed by variable name and omitting the ones
	// that were unset.
	//
	// Generic because the set is not fixed: a provider is added by declaring
	// it, and its home variable has to be honoured without editing this struct.
	// ConfigDir and CodexHomeDir remain as named fields because the layers that
	// read them predate providers entirely.
	HomeOverrides map[string]string

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
// place in aaswap that reads these variables.
//
// In a test binary it panics rather than return a Resolver pointing at the
// developer's real account store — see [guardRealStore].
func FromEnv(homeEnvs ...string) (*Resolver, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("locate home directory: %w: %w", apperr.ErrConfig, err)
	}
	secure, secureSet := os.LookupEnv("CLAUDE_SECURESTORAGE_CONFIG_DIR")
	// Every provider's home variable, so one that this build learned about by
	// declaration alone is still honoured.
	overrides := map[string]string{}
	for _, name := range homeEnvs {
		if value := os.Getenv(name); value != "" {
			overrides[name] = value
		}
	}
	r := &Resolver{
		Home:                      home,
		ConfigDir:                 os.Getenv(ClaudeConfigDirEnv),
		CodexHomeDir:              os.Getenv(CodexHomeEnv),
		HomeOverrides:             overrides,
		XDGDataHome:               os.Getenv("XDG_DATA_HOME"),
		SecureStorageConfigDir:    secure,
		SecureStorageConfigDirSet: secureSet,
		Platform:                  platform.Detect(),
	}
	guardRealStore(r)
	return r, nil
}

// ClaudeConfigDirEnv relocates Claude Code's entire profile, which is how
// session mode isolates accounts.
const ClaudeConfigDirEnv = "CLAUDE_CONFIG_DIR"

// ClaudeConfigHome returns Claude Code's config home: CLAUDE_CONFIG_DIR when
// set, else ~/.claude.
func (r *Resolver) ClaudeConfigHome() string {
	if r.ConfigDir != "" {
		return r.ConfigDir
	}
	return filepath.Join(r.Home, ".claude")
}

// ProviderHome is where a provider's tool keeps everything.
//
// env is the variable that relocates it and defaultDir is the fallback,
// relative to the user's home — both taken from the provider's declaration
// rather than known here, so this resolves a provider whose name this package
// has never seen.
//
// The two named fields win over the generic map because a Resolver built by
// New — every test — sets those and not the map.
func (r *Resolver) ProviderHome(env, defaultDir string) string {
	switch {
	case env == ClaudeConfigDirEnv && r.ConfigDir != "":
		return r.ConfigDir
	case env == CodexHomeEnv && r.CodexHomeDir != "":
		return r.CodexHomeDir
	case env != "" && r.HomeOverrides[env] != "":
		return r.HomeOverrides[env]
	}
	return filepath.Join(r.Home, defaultDir)
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

// LegacyBackupRoot returns the pre-XDG backup root, ~/.aaswap-backup.
func (r *Resolver) LegacyBackupRoot() string {
	return filepath.Join(r.Home, LegacyBackupDirName)
}

// BackupRoot returns aaswap's backup root for this platform.
//
// Per the XDG spec, XDG_DATA_HOME is ignored when unset, empty, or not
// absolute. A leading ~ is expanded so values like "~/data" — set through
// systemd units or Dockerfiles, which get no shell expansion — still work.
func (r *Resolver) BackupRoot() string {
	if r.Platform.UsesXDG() {
		if xdg := r.expandTilde(r.XDGDataHome); filepath.IsAbs(xdg) {
			return filepath.Join(xdg, BackupDirName)
		}
		return filepath.Join(r.Home, ".local", "share", BackupDirName)
	}
	return r.LegacyBackupRoot()
}

// CacheDir is where aaswap keeps regenerable state: the usage table and the
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

// Predecessor is a store written by a project this one succeeded.
//
// Only ever read, and only when the user asks. Two independently evolving
// projects cannot share a backup root — they stamp the same schema versions
// into the same filenames — so a predecessor's store is FOREIGN, and this is
// the one seam between them. It exists so someone arriving from an earlier name
// does not have to move credentials by hand.
type Predecessor struct {
	// Name is what the project called itself, and what its store directory and
	// Keychain service are named after.
	Name string
	// Roots are the places its store could be on this platform, most-current
	// layout first.
	Roots []string
	// KeychainService is where it filed backup credentials on macOS.
	KeychainService string
}

// Found is a predecessor store that actually exists.
type Found struct {
	Predecessor
	Root string
}

// predecessorNames are the projects this one succeeded, closest ancestor first.
//
// Order is the answer for a machine that ran both: the closer ancestor is far
// likelier to hold the store someone actually wants.
var predecessorNames = []string{"ccswap", "claude-swap"}

// Predecessors lists every project whose store this one can adopt.
//
// Both layouts are returned for each rather than just the one this platform
// would write, because a store can arrive from another machine through file
// sync — the same reason [Resolver.MigrateLegacyBackupDir] has to reason about
// a legacy directory appearing on a host that would never create one.
func (r *Resolver) Predecessors() []Predecessor {
	out := make([]Predecessor, 0, len(predecessorNames))
	for _, name := range predecessorNames {
		out = append(out, Predecessor{
			Name:            name,
			Roots:           r.predecessorRoots(name),
			KeychainService: name,
		})
	}
	return out
}

func (r *Resolver) predecessorRoots(name string) []string {
	legacy := filepath.Join(r.Home, "."+name+"-backup")
	if !r.Platform.UsesXDG() {
		return []string{legacy}
	}
	xdg := filepath.Join(r.Home, ".local", "share", name)
	if custom := r.expandTilde(r.XDGDataHome); filepath.IsAbs(custom) {
		xdg = filepath.Join(custom, name)
	}
	return []string{xdg, legacy}
}

// FindPredecessor returns the first predecessor store that exists and carries
// an account table, and whether there was one.
//
// The table is the test rather than the directory: an empty directory left by
// an uninstall is not a store worth telling anyone about, and offering to
// import nothing is worse than saying nothing.
//
// This tool's own roots are skipped, so a store already in use is never offered
// back to its owner as something to import.
func (r *Resolver) FindPredecessor() (Found, bool) {
	ours := map[string]bool{
		r.BackupRoot():       true,
		r.LegacyBackupRoot(): true,
	}
	for _, predecessor := range r.Predecessors() {
		for _, root := range predecessor.Roots {
			if ours[root] {
				continue
			}
			if exists(filepath.Join(root, RosterFileName)) {
				return Found{Predecessor: predecessor, Root: root}, true
			}
		}
	}
	return Found{}, false
}

// WithProviderHome is this resolver with one provider's home relocated, the
// way exporting its home variable would relocate it for the tool.
//
// For a login sandbox: the tool is run with the variable set to a throwaway
// directory, and everything aaswap then reads about that login has to resolve
// against the same directory — the config beside it, the credential inside it,
// and on macOS the Keychain item whose name is derived from the exported
// string. Anything else the resolver knows is unchanged.
//
// The secure-storage override is dropped on purpose. Claude Code sources its
// Keychain item from CLAUDE_SECURESTORAGE_CONFIG_DIR when that is defined, and
// a sandbox that inherited it would land its credential in the caller's live
// store rather than its own.
func (r *Resolver) WithProviderHome(env, dir string) *Resolver {
	clone := *r
	clone.HomeOverrides = maps.Clone(r.HomeOverrides)
	if clone.HomeOverrides == nil {
		clone.HomeOverrides = map[string]string{}
	}
	switch env {
	case ClaudeConfigDirEnv:
		clone.ConfigDir = dir
		clone.SecureStorageConfigDir, clone.SecureStorageConfigDirSet = "", false
	case CodexHomeEnv:
		clone.CodexHomeDir = dir
	}
	if env != "" {
		clone.HomeOverrides[env] = dir
	}
	return &clone
}
