package provider

import (
	"log/slog"
	"os"
	"path/filepath"

	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/keychain"
	"github.com/d0lim/aaswap/internal/platform"
)

// claudeProfiles is Claude Code's profile credential store.
//
// Two places, in a fixed order: the macOS Keychain under a service name derived
// from the profile directory, then a plaintext file inside it. That order is
// Claude Code's own — it reads the Keychain first — which is why a stale item
// has to be deleted before a seed rather than merely overwritten.
type claudeProfiles struct {
	platform platform.Platform
	// kc is nil wherever there is no Keychain to use, which is every platform
	// but macOS and any macOS host that could not reach security(1). Nil is a
	// supported state, not a degraded one: the file is then the whole store.
	kc *keychain.Keychain
}

// NewClaudeProfiles returns Claude Code's profile store.
//
// A nil Keychain is the file-only shape. Callers on non-macOS platforms should
// pass nil rather than a handle that will refuse every call — the difference
// shows up in MayHold, where "no Keychain here" is definitely-absent and "the
// Keychain refused" is not.
func NewClaudeProfiles(p platform.Platform, kc *keychain.Keychain) ProfileStore {
	if p != platform.MacOS {
		kc = nil
	}
	return &claudeProfiles{platform: p, kc: kc}
}

// credentialsFile is where Claude Code keeps a profile's plaintext credential.
func credentialsFile(dir string) string {
	return filepath.Join(dir, ".credentials.json")
}

func (c *claudeProfiles) usable() bool { return c.kc != nil }

func (c *claudeProfiles) Read(dir string) string {
	if c.usable() {
		value, found, err := c.kc.Get(credstore.KeychainServiceName(dir), keychain.AccountName())
		if err == nil && found && value != "" {
			return value
		}
		// An absent item is Claude Code's own signal to read the file. An
		// unreadable one falls through too: the file is the next-best truth for
		// a read that is best effort by contract.
	}
	data, err := os.ReadFile(credentialsFile(dir))
	if err != nil {
		return ""
	}
	return string(data)
}

func (c *claudeProfiles) MayHold(dir string) bool {
	if data, err := os.ReadFile(credentialsFile(dir)); err == nil && len(data) > 0 {
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

func (c *claudeProfiles) Clear(dir string) {
	if !c.usable() {
		return
	}
	if err := c.kc.Delete(credstore.KeychainServiceName(dir), keychain.AccountName()); err != nil {
		slog.Debug("could not delete a session profile's Keychain item",
			"profile", dir, "error", err)
	}
}
