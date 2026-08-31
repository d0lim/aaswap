// Package credstore owns where credentials live and how they are read and
// written: the macOS Keychain-versus-file routing, the per-process capability
// detection and sticky fallback, and the reconciliation between a slot's backup
// and the machine's live state.
//
// It is a leaf collaborator. It depends only on OS primitives and path helpers
// (keychain, paths, fsutil) and never on the account orchestration above it, so
// storage and orchestration cannot re-couple.
package credstore

import (
	json "encoding/json/v2"
	"log/slog"
	"maps"
	"slices"
	"strings"
)

// Keychain service names.
const (
	// BackupService holds cswap's own per-account backup credentials.
	// Deliberately distinct from the legacy keyring service so old keyring
	// items and new security(1) items can coexist during migration
	// (safe write, verify, then delete).
	BackupService = "claude-swap"

	// ClaudeOAuthService is Claude Code's *active* OAuth credential. Claude
	// Code reads it; cswap reads and writes it when switching accounts.
	ClaudeOAuthService = "Claude Code-credentials"

	// ClaudeManagedKeyService is Claude Code's *active* managed API key — the
	// one stored after `/login` with an sk-ant-api… key. It sits on a separate
	// auth axis from the OAuth item (getApiKeyFromConfigOrMacOSKeychain), which
	// is why the name has no "-credentials" suffix. Off macOS the managed key
	// lives in ~/.claude.json as primaryApiKey instead.
	ClaudeManagedKeyService = "Claude Code"
)

// sharedCredentialKeys are the siblings of claudeAiOauth that hold
// machine-shared OAuth integrations. They rotate independently of any account
// slot, so on activation the machine's live copy is authoritative.
//
// Everything else — known or unknown — stays with the target slot. The
// asymmetry is deliberate: a stale restore of an unlisted shared field merely
// re-prompts for auth, while carrying a live account-bound field across a
// switch would present one account's credential under another.
var sharedCredentialKeys = []string{
	"mcpOAuth",
	"mcpOAuthClientConfig",
	"mcpXaaIdp",
	"mcpXaaIdpConfig",
	"pluginSecrets",
}

// accountCredentialKeys are the account-scoped siblings cswap knows about,
// named so the unrecognized-key probe below does not flag them. claudeAiOauth
// is the login itself; trustedDeviceToken is enrolled per (device, account) at
// /login.
var accountCredentialKeys = []string{
	"claudeAiOauth",
	"trustedDeviceToken",
}

// LooksLikeAPIKey reports whether a stored active credential is a raw managed
// API key rather than OAuth JSON.
//
// Strict on purpose: a managed key is a bare sk-ant-api… string, while every
// OAuth and setup-token credential is a JSON object. Requiring the prefix *and*
// that the value is not JSON keeps a raw or garbled sk-ant-oat… setup token
// from ever being misclassified as an API key.
func LooksLikeAPIKey(credentials string) bool {
	if credentials == "" {
		return false
	}
	text := strings.TrimSpace(credentials)
	return strings.HasPrefix(text, "sk-ant-api") && !strings.HasPrefix(text, "{")
}

// credentialObject parses a JSON credential object, excluding managed API keys.
// It reports false for anything that is not a JSON object.
func credentialObject(credentials string) (map[string]any, bool) {
	if credentials == "" || LooksLikeAPIKey(credentials) {
		return nil, false
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(credentials), &data); err != nil || data == nil {
		return nil, false
	}
	return data, true
}

// SharedCredentialFields returns the machine-shared fields of a Claude OAuth
// credential object, and whether the input was such an object at all.
//
// Only the allowlist is machine-shared; other siblings of claudeAiOauth are
// account-scoped or unknown and stay slot-owned. A false second return means
// the input is not a JSON credential object — missing, malformed, or a managed
// API key. A returned map, *including an empty one*, is authoritative for every
// allowlisted key: a key absent from it is absent from the machine's current
// shared state.
func SharedCredentialFields(credentials string) (map[string]any, bool) {
	data, ok := credentialObject(credentials)
	if !ok {
		return nil, false
	}
	if _, hasLogin := data["claudeAiOauth"]; hasLogin {
		// A sibling key cswap does not know defaults to slot-owned, which fails
		// safe — but silently. If Claude Code grows a new *shared* key, that
		// default quietly reintroduces the stale-restore papercut for it, so
		// leave a trace that gets noticed.
		var unrecognized []string
		for key := range data {
			if !slices.Contains(sharedCredentialKeys, key) && !slices.Contains(accountCredentialKeys, key) {
				unrecognized = append(unrecognized, key)
			}
		}
		if len(unrecognized) > 0 {
			slices.Sort(unrecognized)
			slog.Debug("live credential has sibling keys cswap does not recognize "+
				"(a newer Claude Code?); treating them as slot-owned",
				"keys", unrecognized)
		}
	}

	shared := map[string]any{}
	for _, key := range sharedCredentialKeys {
		if value, ok := data[key]; ok {
			shared[key] = value
		}
	}
	return shared, true
}

// MergeSharedCredentialFields composes a target slot's Claude login with the
// machine's shared fields.
//
// The allowlisted keys are wholly live-owned, presence and absence alike: the
// target's copies are discarded and sharedFields supplies the current
// generation, so a shared key the machine no longer holds is not resurrected
// from the slot's snapshot. Every other target field passes through untouched.
//
// The target is returned unchanged when it is not a JSON credential object
// carrying a Claude login, so managed API keys and opaque legacy shapes stay
// activatable verbatim.
func MergeSharedCredentialFields(targetCredentials string, sharedFields map[string]any) string {
	target, ok := credentialObject(targetCredentials)
	if !ok {
		return targetCredentials
	}
	if _, hasLogin := target["claudeAiOauth"]; !hasLogin {
		return targetCredentials
	}

	composed := map[string]any{}
	for key, value := range target {
		if !slices.Contains(sharedCredentialKeys, key) {
			composed[key] = value
		}
	}
	maps.Copy(composed, sharedFields)

	// Deterministic so the same inputs always produce the same bytes; JSON
	// member order carries no meaning to Claude Code, which only parses this.
	encoded, err := json.Marshal(composed, json.Deterministic(true))
	if err != nil {
		// The map came from a successful decode plus values of the same
		// provenance, so this cannot fail in practice; returning the target
		// unchanged is the safe answer if it ever does.
		return targetCredentials
	}
	return string(encoded)
}

// ApprovedForm returns the value Claude Code stores in
// customApiKeyResponses.approved.
//
// It mirrors Claude Code's normalizeApiKeyForConfig — the last 20 characters.
// Storing anything else makes Claude Code's "is this key approved?" check miss
// and re-prompt the user to approve the key they just set.
func ApprovedForm(apiKey string) string {
	trimmed := strings.TrimSpace(apiKey)
	if len(trimmed) <= 20 {
		return trimmed
	}
	return trimmed[len(trimmed)-20:]
}
