package paths

import (
	"path/filepath"
	"testing"

	"github.com/d0lim/aaswap/internal/platform"
)

// Codex keeps everything in one directory, and one file inside it holds both
// the credential and the identity — unlike Claude, which splits them.
func TestCodexPaths(t *testing.T) {
	home := t.TempDir()
	r := New(home, platform.MacOS)

	if got, want := r.CodexHome(), filepath.Join(home, ".codex"); got != want {
		t.Errorf("CodexHome() = %q, want %q", got, want)
	}
	if got, want := r.CodexAuthPath(), filepath.Join(home, ".codex", "auth.json"); got != want {
		t.Errorf("CodexAuthPath() = %q, want %q", got, want)
	}

	// CODEX_HOME relocates the whole directory, the way CLAUDE_CONFIG_DIR does.
	custom := filepath.Join(t.TempDir(), "elsewhere")
	r.CodexHomeDir = custom
	if got := r.CodexHome(); got != custom {
		t.Errorf("CodexHome() with CODEX_HOME = %q, want %q", got, custom)
	}
	if got, want := r.CodexAuthPath(), filepath.Join(custom, "auth.json"); got != want {
		t.Errorf("CodexAuthPath() = %q, want %q", got, want)
	}
}

// The credential path a caller asks for depends on which provider it is
// addressing. Getting this wrong reads one tool's file and calls it another's.
func TestCredentialPathFollowsTheProvider(t *testing.T) {
	home := t.TempDir()
	r := New(home, platform.MacOS)

	if got, want := r.ProviderCredentialsPath("claude"), r.CredentialsPath(); got != want {
		t.Errorf("claude credential path = %q, want %q", got, want)
	}
	if got, want := r.ProviderCredentialsPath("codex"), r.CodexAuthPath(); got != want {
		t.Errorf("codex credential path = %q, want %q", got, want)
	}
	// An unknown provider falls back to the default rather than an empty path:
	// an empty path is a read of the current directory.
	if got := r.ProviderCredentialsPath("nonsense"); got != r.CredentialsPath() {
		t.Errorf("unknown provider path = %q, want the default", got)
	}
}
