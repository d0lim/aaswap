package provider

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/keychain"
	"github.com/d0lim/aaswap/internal/platform"
)

// fileProfiles is a provider's profile credential store.
//
// Up to two places, in a fixed order: the macOS Keychain under a service name
// derived from the profile directory, then a plaintext file inside it. That
// order is Claude Code's own — it reads the Keychain first — which is why a
// stale item has to be deleted before a seed rather than merely overwritten.
//
// Only Claude has the Keychain half. For every other provider this is the file
// and nothing else, which is why the file name comes from the declaration
// rather than being fixed: seeding a Codex profile with a .credentials.json
// would write a file the tool never reads and leave the session logged out
// while every other check passed.
type fileProfiles struct {
	platform platform.Platform
	// secret is the credential's name inside a profile directory.
	secret string
	// kc is nil wherever there is no Keychain to use: every platform but
	// macOS, any macOS host that could not reach security(1), and every
	// provider that does not declare one. Nil is a supported state, not a
	// degraded one — the file is then the whole store.
	kc *keychain.Keychain
}

// NewProfiles returns a provider's profile credential store.
//
// The Keychain is dropped unless the provider declares one AND this is macOS.
// Passing a handle that will refuse every call is not the same thing: the
// difference shows up in MayHold, where "no Keychain here" is definitely-absent
// and "the Keychain refused" is not.
func NewProfiles(spec Spec, p platform.Platform, kc *keychain.Keychain) ProfileStore {
	if p != platform.MacOS || !spec.Keychain {
		kc = nil
	}
	secret := claudeProfileSecret
	if secrets := spec.SecretFiles(); len(secrets) > 0 {
		secret = secrets[0].Path
	}
	return &fileProfiles{platform: p, secret: secret, kc: kc}
}

// claudeProfileSecret is the fallback profile credential name, for a
// declaration that named no secret at all.
const claudeProfileSecret = ".credentials.json"

// credentialsFile is where this provider keeps a profile's plaintext
// credential.
func (c *fileProfiles) credentialsFile(dir string) string {
	return filepath.Join(dir, c.secret)
}

func (c *fileProfiles) usable() bool { return c.kc != nil }

func (c *fileProfiles) Read(dir string) string {
	if c.usable() {
		value, found, err := c.kc.Get(credstore.KeychainServiceName(dir), keychain.AccountName())
		if err == nil && found && value != "" {
			return value
		}
		// An absent item is Claude Code's own signal to read the file. An
		// unreadable one falls through too: the file is the next-best truth for
		// a read that is best effort by contract.
	}
	data, err := os.ReadFile(c.credentialsFile(dir))
	if err != nil {
		return ""
	}
	return string(data)
}

func (c *fileProfiles) MayHold(dir string) bool {
	if data, err := os.ReadFile(c.credentialsFile(dir)); err == nil && len(data) > 0 {
		return true
	}
	if !c.usable() {
		// No second store to be uncertain about. The file was the whole answer.
		return false
	}
	value, found, err := c.kc.Get(credstore.KeychainServiceName(dir), keychain.AccountName())
	if err != nil {
		// Indeterminate leans present: an unreadable Keychain is not evidence
		// of an empty one, and re-seeding over a live item destroys it.
		return true
	}
	return found && value != ""
}

func (c *fileProfiles) Clear(dir string) {
	if !c.usable() {
		return
	}
	if err := c.kc.Delete(credstore.KeychainServiceName(dir), keychain.AccountName()); err != nil {
		slog.Debug("could not delete a session profile's Keychain item",
			"profile", dir, "error", err)
	}
}
