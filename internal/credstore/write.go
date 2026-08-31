package credstore

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"slices"
	"strings"

	"github.com/realiti4/claude-swap/internal/apperr"
	"github.com/realiti4/claude-swap/internal/fsutil"
	"github.com/realiti4/claude-swap/internal/keychain"
	"github.com/realiti4/claude-swap/internal/platform"
)

// WriteActive writes Claude Code's active credential, enforcing a single auth
// axis.
//
// It detects the kind from the payload — a raw sk-ant-api… key versus OAuth
// JSON — and mirrors Claude Code's own saveApiKey/removeApiKey: activating one
// axis clears the other, so a stale credential cannot shadow the switch.
//
//   - OAuth: write the OAuth credential, then clear any managed key. The
//     approved list is left intact, exactly as removeApiKey leaves it.
//   - API key: record the key's approved form, store the key, then clear the
//     OAuth credential.
//
// Note that the *write* side is default-profile-only: it targets the fixed
// service name, unlike the read side which follows CLAUDE_CONFIG_DIR to a
// hashed item. Switching accounts operates on the default profile; session mode
// isolates itself through its own profile directory instead.
func (s *Store) WriteActive(credentials string) error {
	if LooksLikeAPIKey(credentials) {
		return s.writeManagedCredentials(strings.TrimSpace(credentials))
	}
	if err := s.writeOAuthCredentials(credentials); err != nil {
		return err
	}
	s.clearManagedKey()
	return nil
}

// writeOAuthCredentials writes Claude Code's active OAuth credential.
//
// macOS writes the Keychain when it is usable. On a successful Keychain write it
// then REWRITES AN ALREADY-PRESENT .credentials.json with the same fresh
// credentials — never creating one when absent, never deleting one. That bumps
// the file's mtime so a running Claude Code session's cache invalidation fires
// and it hot-reloads the new account instead of serving its memoized token until
// restart (#86), and it keeps the file consistent for the shared-~/.claude
// consumer (#1414) rather than stranding it on stale content. Keychain-only
// users keep their fileless posture — their absent-file path already hot-reloads
// via the ~30s Keychain TTL — and never gain a plaintext credential on disk.
//
// If the Keychain write fails, or the Keychain is already known unusable, it
// writes the plaintext file and best-effort clears any stale Keychain entry
// (#30337). Linux, WSL and Windows always write the file.
func (s *Store) writeOAuthCredentials(credentials string) error {
	if s.cap.useKeychain() {
		_, err := s.cap.observe(func() (struct{}, error) {
			return struct{}{}, s.kc.Set(ClaudeOAuthService, keychain.AccountName(), credentials)
		})
		if err == nil {
			// The Keychain, the primary on macOS, now holds the fresh
			// credential. Bump an already-present shadow file's mtime so
			// running sessions hot-reload.
			s.refreshStaleCredentialsFile(credentials)
			s.lastActiveBackend = "keychain"
			return nil
		}
		if !errors.Is(err, keychain.ErrUnavailable) {
			return err // a defect, not a fallback condition
		}
		slog.Warn("Keychain write failed, falling back to the file", "error", err)
	}

	// File mode: off macOS, a Keychain already known unusable, or a Keychain
	// write that just failed.
	if err := s.writeActiveCredentialsFile(credentials); err != nil {
		return fmt.Errorf("write credentials: %w: %w", apperr.ErrCredentialWrite, err)
	}
	cleared := s.deleteActiveKeychainEntry()
	if s.platform == platform.MacOS {
		// The delete above is best-effort, so a stale Keychain item may remain.
		// Pin file mode so a later cooldown re-probe cannot read that residual
		// and resurrect the wrong account. Its outcome is also the answer to
		// whether the file is now the authority, so hand it over rather than
		// re-deriving it from flags.
		s.cap.pinFileMode(cleared)
	}
	s.lastActiveBackend = "file"
	return nil
}

// refreshStaleCredentialsFile bumps an already-present .credentials.json's mtime
// after a Keychain write. Rewrite-when-present, never create (#86).
//
// Claude Code invalidates its memoized OAuth token only when this file's mtime
// changes or the file is absent, so a Keychain-only switch leaves a stale file's
// mtime frozen and a running session serves the old token until restart.
// Rewriting the existing file with the same fresh credentials bumps the mtime —
// the publish is an atomic rename, so it bumps even when the content is
// identical.
//
// Best-effort: the Keychain write is authoritative on macOS and has already
// succeeded, so a failure here must not fail the switch. It only means a running
// session may lag until restart.
func (s *Store) refreshStaleCredentialsFile(credentials string) {
	if _, err := os.Lstat(s.paths.CredentialsPath()); err != nil {
		return // absent: Keychain-only users keep their fileless posture
	}
	if err := s.writeActiveCredentialsFile(credentials); err != nil {
		slog.Warn("could not refresh .credentials.json after a Keychain write; "+
			"a running session may not hot-reload until restart", "error", err)
	}
}

// writeManagedCredentials activates a managed API key, then clears OAuth so the
// two axes stay mutually exclusive.
//
// The approved form is recorded on every platform, even on a Keychain success —
// Claude Code does the same, and skipping it makes Claude Code re-prompt the
// user to approve the key they just set. The key itself goes to the macOS
// Keychain when usable, else to primaryApiKey in the global config, matching
// saveApiKey's keychain-then-config fallback.
func (s *Store) writeManagedCredentials(apiKey string) error {
	wroteToKeychain := false
	if s.cap.useKeychain() {
		_, err := s.cap.observe(func() (struct{}, error) {
			return struct{}{}, s.kc.Set(ClaudeManagedKeyService, keychain.AccountName(), apiKey)
		})
		switch {
		case err == nil:
			wroteToKeychain = true
		case errors.Is(err, keychain.ErrUnavailable):
			slog.Warn("managed-key Keychain write failed, falling back to the config", "error", err)
		default:
			return err
		}
	}

	approved := ApprovedForm(apiKey)
	err := s.updateGlobalConfig(func(cfg map[string]any) {
		responses, _ := cfg["customApiKeyResponses"].(map[string]any)
		if responses == nil {
			responses = map[string]any{}
		}
		approvedList, _ := responses["approved"].([]any)
		if !slices.Contains(approvedList, any(approved)) {
			approvedList = append(approvedList, approved)
		}
		responses["approved"] = approvedList
		if _, present := responses["rejected"]; !present {
			responses["rejected"] = []any{}
		}
		cfg["customApiKeyResponses"] = responses

		if wroteToKeychain {
			// The Keychain holds the key; keep it out of the plaintext config.
			delete(cfg, "primaryApiKey")
		} else {
			cfg["primaryApiKey"] = apiKey
		}
	})
	if err != nil {
		return fmt.Errorf("write managed API key: %w", err)
	}

	// Mutual exclusion: drop the OAuth credential so it cannot shadow the key.
	s.clearOAuthCredential()

	if s.platform == platform.MacOS && !wroteToKeychain {
		// The same stale-Keychain resurrection guard as the OAuth path: the key
		// fell back to plaintext primaryApiKey while a stale "Claude Code"
		// Keychain item may remain, and managed-key reads check the Keychain
		// before primaryApiKey. Pin file mode so a cooldown re-probe cannot read
		// that residual over the fresh fallback value.
		//
		// The residual is reported UNVERIFIED: the item that would shadow here
		// is the MANAGED one, and nothing deleted it — clearOAuthCredential
		// above removes the OAuth item, a different service. Unverified, so the
		// conservative verdict is the true one.
		s.cap.pinFileMode(false)
	}
	if wroteToKeychain {
		s.lastActiveBackend = "keychain"
	} else {
		s.lastActiveBackend = "file"
	}
	return nil
}

// clearManagedKey clears any active managed API key, with Claude Code's
// removeApiKey semantics.
//
// It deletes the macOS Keychain item (best-effort) and drops primaryApiKey from
// the global config. customApiKeyResponses.approved is left untouched —
// removeApiKey does not clear it either, and removing it would force recovering
// the approved form from the Keychain for no benefit. A profile with no key is a
// no-op, with no config rewrite.
//
// Never returns an error: the write path this feeds must not block on a
// transient read glitch. But an unreadable config is warned about rather than
// silently skipped, because a stale primaryApiKey surviving alongside a freshly
// activated OAuth credential is a live cross-account key that bills per token
// while it lies — a reader has to be able to tell "nothing to clear" from
// "could not check".
func (s *Store) clearManagedKey() {
	if s.platform == platform.MacOS {
		// Best-effort: a down Keychain cannot be cleaned now.
		_ = s.kc.Delete(ClaudeManagedKeyService, keychain.AccountName())
	}

	cfg, ok := s.readGlobalConfig()
	if !ok {
		if _, err := os.Lstat(s.paths.GlobalConfigPath()); err == nil {
			slog.Warn("could not clear primaryApiKey: the global config exists but " +
				"could not be read (unreadable, not absent) — leaving it in place " +
				"rather than overwriting it unread")
		}
		return
	}
	if _, present := cfg["primaryApiKey"]; !present {
		return
	}
	if err := s.updateGlobalConfig(func(c map[string]any) { delete(c, "primaryApiKey") }); err != nil {
		slog.Warn("failed to clear primaryApiKey", "error", err)
	}
}

// clearOAuthCredential removes the active OAuth credential from both the
// Keychain and the plaintext file.
//
// Best-effort: a down Keychain or a missing file is fine. Removing
// .credentials.json is what stops Claude Code falling back to a stale OAuth
// login over the just-activated API key.
func (s *Store) clearOAuthCredential() {
	s.deleteActiveKeychainEntry()
	s.removeActiveCredentialsFile()
}

// updateGlobalConfig atomically applies a mutator to ~/.claude.json, scoped to
// the keys cswap owns.
//
// It reads the current config, lets the mutator change only primaryApiKey and
// customApiKeyResponses, and writes it back atomically — preserving every other
// key: oauthAccount, projects, settings, mcpServers.
//
// An UNREADABLE config is refused rather than treated as absent. Collapsing the
// two would let the atomic replace write a near-empty object over a file it had
// never read: measured against a torn config — a valid prefix with a truncated
// tail, which is what a crash mid-write leaves — oauthAccount, projects and
// mcpServers were all gone. An ABSENT config has nothing to preserve and is a
// genuine start.
func (s *Store) updateGlobalConfig(mutate func(map[string]any)) error {
	path := s.paths.GlobalConfigPath()

	data, ok := s.readGlobalConfig()
	if !ok {
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf(
				"%s exists but could not be read — refusing to overwrite it; "+
					"move or repair the file, then retry: %w",
				path, apperr.ErrCredentialWrite)
		}
		data = map[string]any{}
	}
	mutate(data)

	encoded, err := json.Marshal(data, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("encode global config: %w: %w", apperr.ErrCredentialWrite, err)
	}
	// The foreign writer: the parent of ~/.claude.json is the user's home
	// directory, and hardening it to 0700 is not cswap's call to make.
	if err := fsutil.WriteForeignFileAtomic(path, encoded); err != nil {
		return fmt.Errorf("write global config: %w: %w", apperr.ErrCredentialWrite, err)
	}
	return nil
}
