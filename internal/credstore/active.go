package credstore

import (
	json "encoding/json/v2"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/realiti4/claude-swap/internal/fsutil"
	"github.com/realiti4/claude-swap/internal/keychain"
	"github.com/realiti4/claude-swap/internal/platform"
)

// Bounded retry for the active OAuth Keychain read.
//
// A locked or contended login Keychain can fail a single security(1) call
// transiently — just after wake while the keychain is still settling, or under
// contention with Claude Code's own statusline polling the same item — and a
// second attempt a moment later usually succeeds. This is an I/O backoff
// between retries of an external CLI, not a sleep papering over an internal
// race.
const activeReadAttempts = 2

// A var rather than a const so tests can collapse the backoff; production never
// reassigns it.
var activeReadRetryDelay = 300 * time.Millisecond

// ActiveCredentials is the outcome of reading Claude Code's active credential.
type ActiveCredentials struct {
	// Value is the credential — OAuth JSON or a raw managed key — or "" when
	// none exists in any backend.
	Value string

	// FileReadFailed reports that the plaintext credentials file existed but
	// could not be read. It is distinct from an empty Value: "nothing is
	// stored" and "something is stored but unreachable" call for different
	// advice.
	FileReadFailed bool

	// KeychainUnavailable is true only when the macOS OAuth Keychain read
	// failed — locked, denied, timed out — and nothing else covered it. It lets
	// the display layer say "keychain unavailable" rather than "no credentials"
	// for a merely unreadable slot, which would otherwise nudge the user into
	// an unnecessary re-login.
	KeychainUnavailable bool

	// Degraded is true whenever the OAuth Keychain read failed, *even when a
	// fallback covered it*: the bytes served may then be a stale generation,
	// because on macOS Claude Code rotates Keychain-only and the plaintext file
	// can lag.
	//
	// A degraded credential may be adopted or served, but its refresh token
	// must never be consumed — POSTing a superseded one-time token yields
	// invalid_grant and a false dead-token strike against a live account.
	Degraded bool
}

// ReadActive reads Claude Code's active credential and classifies the outcome.
//
// It tries the OAuth credential fully first — the macOS Keychain when usable,
// with a bounded retry to ride out transient contention, then the plaintext
// ~/.claude/.credentials.json that Claude Code itself falls back to — and only
// then the managed-key locations. Trying OAuth fully first is what stops a
// macOS OAuth login whose Keychain item is missing but whose file fallback
// exists from being misread as an API key.
//
// Non-mutating apart from the capability bookkeeping every Keychain call feeds.
func (s *Store) ReadActive() ActiveCredentials {
	keychainFailed := false

	// 1. OAuth Keychain (macOS, when usable), with a bounded retry.
	//
	// READ THE ITEM FOR *THIS* PROFILE, NOT THE FIXED NAME. Claude Code stores
	// only the default profile's credential under the unsuffixed service; with
	// CLAUDE_CONFIG_DIR set it scopes the item to that config dir under a hashed
	// name. Reading the fixed name from a custom profile would return a
	// credential belonging to a different account, while the identity read one
	// layer up reports the custom profile's — a silent mispairing that every
	// consumer of this read would inherit.
	//
	// Redirected, not skipped. Skipping the Keychain under a custom profile
	// would leave macOS mostly blind: Claude Code writes rotations Keychain-only
	// there, so the plaintext file frequently does not exist at all and a
	// logged-in profile would render as "no credentials" — trading a wrong
	// answer for a missing one.
	if s.cap.useKeychain() {
		value, failed := s.readActiveOAuthKeychain()
		keychainFailed = failed
		// This read's own verdict, kept so a later success on some OTHER item
		// cannot erase it. Sticky until this read succeeds again, which is what
		// makes it self-heal without being erasable.
		s.cap.setActiveReadFailed(failed)
		if value != "" {
			return ActiveCredentials{Value: value}
		}
	} else if s.cap.residualUnverified() || s.cap.activeReadDidFail() || s.cap.unreadable() {
		// The Keychain is already known unusable this process: if nothing is
		// found below, that absence is "keychain unavailable", not an empty
		// slot. An UNVERIFIED clear says so on its own — no later success on
		// some other item speaks for this one. A verified clear does not appear
		// here at all: it settled the flags at the pin, so anything they say now
		// happened after it.
		keychainFailed = true
	}

	// 2. OAuth plaintext file — Claude Code's own fallback, on every platform.
	//
	// After a FAILED Keychain read this file may hold a stale generation, since
	// Claude Code writes rotations Keychain-only on macOS. The flag travels as
	// Degraded so consume paths refuse these bytes.
	credFile := s.paths.CredentialsPath()
	if text, err := fsutil.ReadText(credFile); err == nil {
		if strings.TrimSpace(text) != "" {
			return ActiveCredentials{Value: text, Degraded: keychainFailed}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		slog.Error("failed to read the credentials file", "path", credFile, "error", err)
		// KeychainUnavailable is NOT forced false here. A denied Keychain AND
		// an unreadable fallback file is the most unreadable state there is;
		// hardcoding false made it render as "no credentials" and sent the user
		// to a re-login that cannot help.
		return ActiveCredentials{
			FileReadFailed:      true,
			KeychainUnavailable: keychainFailed,
			Degraded:            keychainFailed,
		}
	}

	// 3. Managed API key: the macOS Keychain "Claude Code" item, then
	// primaryApiKey in the global config.
	if key := s.readManagedKey(); key != "" {
		return ActiveCredentials{Value: key, Degraded: keychainFailed}
	}

	// Nothing anywhere. Flag a failed-and-uncovered OAuth Keychain read so the
	// UI distinguishes it from a genuinely empty slot.
	return ActiveCredentials{KeychainUnavailable: keychainFailed, Degraded: keychainFailed}
}

// readActiveOAuthKeychain reads the active profile's OAuth Keychain item(s),
// stopping at the first hit.
//
// An unreadable Keychain stops the walk: that is a property of the Keychain,
// not of the item, so a second service name cannot fare better — and the
// capability bookkeeping has already flipped routing to file mode by then.
func (s *Store) readActiveOAuthKeychain() (value string, failed bool) {
	for _, service := range ActiveOAuthKeychainServices(s.paths) {
		value, failed := s.readOneOAuthKeychain(service)
		if value != "" {
			return value, false
		}
		if failed {
			return "", true
		}
	}
	return "", false
}

// readOneOAuthKeychain reads one OAuth Keychain item with a bounded retry.
//
// failed is true only when *every* attempt errored. A genuinely absent item is
// reported as ("", false) and is not retried — there is nothing transient about
// "no such item".
func (s *Store) readOneOAuthKeychain(service string) (value string, failed bool) {
	var lastErr error
	for attempt := range activeReadAttempts {
		got, err := s.cap.observe(func() (string, error) {
			v, _, err := s.kc.Get(service, keychain.AccountName())
			return v, err
		})
		if err == nil {
			return got, false
		}
		lastErr = err
		if attempt+1 < activeReadAttempts {
			time.Sleep(activeReadRetryDelay)
		}
	}
	slog.Warn("Keychain read failed on every attempt, trying the file",
		"service", service, "attempts", activeReadAttempts, "error", lastErr)
	return "", true
}

// readManagedKey returns the active managed API key, or "" when there is none.
//
// The macOS Keychain "Claude Code" item comes first, then primaryApiKey in the
// global config — mirroring Claude Code's getApiKeyFromConfigOrMacOSKeychain.
//
// The Keychain half is default-profile-only. Unlike the OAuth item it is gated
// rather than redirected, because there is no codified derivation to redirect it
// *to*: the hashed-name derivation covers the credentials item, and Claude
// Code's managed-key service name under a custom profile is not pinned anywhere.
// Guessing it is the thing the OAuth half can avoid and this half cannot.
//
// primaryApiKey is read from the active profile's own config regardless, so a
// custom profile with a managed key is still found.
func (s *Store) readManagedKey() string {
	if ActiveProfileIsDefault(s.paths) && s.cap.useKeychain() {
		value, err := s.cap.observe(func() (string, error) {
			v, _, err := s.kc.Get(ClaudeManagedKeyService, keychain.AccountName())
			return v, err
		})
		if err != nil {
			slog.Warn("managed-key Keychain read failed", "error", err)
		} else if value != "" {
			return value
		}
	}
	if cfg, ok := s.readGlobalConfig(); ok {
		if key, isString := cfg["primaryApiKey"].(string); isString && key != "" {
			return key
		}
	}
	return ""
}

// readGlobalConfig reads and parses ~/.claude.json.
//
// The second return is false when the file is absent, unreadable, or not a JSON
// object. Callers that are about to *write* the file must additionally check
// whether it exists, because overwriting an unreadable config would destroy
// everything in it.
func (s *Store) readGlobalConfig() (map[string]any, bool) {
	path := s.paths.GlobalConfigPath()
	text, err := fsutil.ReadText(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("failed to read the global config", "path", path, "error", err)
		}
		return nil, false
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(text), &data); err != nil || data == nil {
		slog.Warn("global config is not a JSON object", "path", path, "error", err)
		return nil, false
	}
	return data, true
}

// deleteActiveKeychainEntry removes the active OAuth Keychain item, best effort.
//
// Claude Code reads the Keychain before the plaintext file, so once cswap falls
// back to the file it must clear any stale Keychain entry or Claude Code would
// resurrect it (#30337). Best-effort: when the Keychain is down the delete
// cannot run, which is the documented recovery residual.
//
// It reports whether no active item can shadow the file. A nil error from the
// delete is proof, because deletion succeeds on both "removed" and "already
// absent" and errors otherwise — the very fact pinFileMode needs and that an
// earlier version discarded. Off macOS there is no Keychain item at all, hence
// true.
func (s *Store) deleteActiveKeychainEntry() bool {
	if s.platform != platform.MacOS {
		return true
	}
	return s.kc.Delete(ClaudeOAuthService, keychain.AccountName()) == nil
}

// writeActiveCredentialsFile atomically writes Claude Code's plaintext
// credentials file at 0600.
//
// It uses the foreign-file writer: ~/.claude is Claude Code's directory, and
// narrowing its mode is not cswap's call to make.
func (s *Store) writeActiveCredentialsFile(credentials string) error {
	return fsutil.WriteForeignFileAtomic(s.paths.CredentialsPath(), []byte(credentials))
}

// removeActiveCredentialsFile deletes the plaintext credentials file if present.
func (s *Store) removeActiveCredentialsFile() {
	if err := os.Remove(s.paths.CredentialsPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("failed to remove the credentials file", "error", err)
	}
}
