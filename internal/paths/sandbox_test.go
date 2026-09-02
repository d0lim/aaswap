package paths

import (
	"path/filepath"
	"testing"

	"github.com/d0lim/aaswap/internal/platform"
)

// A login sandbox is a home the tool is pointed at by its variable, and every
// read aaswap then makes about that login has to resolve against it — Claude's
// config beside it, Codex's home itself — while the caller's resolver is left
// as it was.
func TestWithProviderHomeRelocatesExactlyThatProvider(t *testing.T) {
	r := New("/home/u", platform.Linux)
	r.SecureStorageConfigDir, r.SecureStorageConfigDirSet = "/elsewhere", true

	claude := r.WithProviderHome(ClaudeConfigDirEnv, "/sb/claude")
	if got := claude.GlobalConfigPath(); got != filepath.Join("/sb/claude", ".claude.json") {
		t.Errorf("Claude's config resolves to %q, want it inside the sandbox", got)
	}
	if got := claude.CredentialsPath(); got != filepath.Join("/sb/claude", ".credentials.json") {
		t.Errorf("Claude's credential resolves to %q, want it inside the sandbox", got)
	}
	if claude.SecureStorageConfigDirSet {
		t.Error("the secure-storage override survived into the sandbox: the Keychain item would land in the caller's store")
	}
	if got := claude.CodexHome(); got != filepath.Join("/home/u", ".codex") {
		t.Errorf("Codex's home moved to %q while relocating Claude's", got)
	}

	codex := r.WithProviderHome(CodexHomeEnv, "/sb/codex")
	if got := codex.CodexAuthPath(); got != filepath.Join("/sb/codex", "auth.json") {
		t.Errorf("Codex's credential resolves to %q, want it inside the sandbox", got)
	}
	if got := codex.ProviderHome(CodexHomeEnv, ".codex"); got != "/sb/codex" {
		t.Errorf("ProviderHome = %q, want the sandbox", got)
	}
	if codex.ConfigDir != "" || !codex.SecureStorageConfigDirSet {
		t.Error("relocating Codex's home changed Claude's resolution")
	}

	// The original is untouched, including its override map.
	if r.ConfigDir != "" || r.CodexHomeDir != "" || len(r.HomeOverrides) != 0 {
		t.Errorf("the caller's resolver was modified: %+v", r)
	}
}
