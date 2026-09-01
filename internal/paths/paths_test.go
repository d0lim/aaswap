package paths

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/d0lim/ccswap/internal/platform"
)

// These tests pin ccswap's path resolution to Claude Code's own. If they drift,
// ccswap reads the wrong files and misattributes accounts (issue #16).

func TestClaudeConfigHome(t *testing.T) {
	home := t.TempDir()

	t.Run("defaults to .claude in home", func(t *testing.T) {
		r := New(home, platform.MacOS)
		want := filepath.Join(home, ".claude")
		if got := r.ClaudeConfigHome(); got != want {
			t.Errorf("ClaudeConfigHome() = %q, want %q", got, want)
		}
	})

	t.Run("respects CLAUDE_CONFIG_DIR", func(t *testing.T) {
		custom := filepath.Join(t.TempDir(), "custom-claude")
		r := New(home, platform.MacOS)
		r.ConfigDir = custom
		if got := r.ClaudeConfigHome(); got != custom {
			t.Errorf("ClaudeConfigHome() = %q, want %q", got, custom)
		}
	})
}

func TestGlobalConfigPath(t *testing.T) {
	t.Run("without CLAUDE_CONFIG_DIR it sits at home, not inside .claude", func(t *testing.T) {
		home := t.TempDir()
		r := New(home, platform.MacOS)
		want := filepath.Join(home, ".claude.json")
		if got := r.GlobalConfigPath(); got != want {
			t.Errorf("GlobalConfigPath() = %q, want %q", got, want)
		}
	})

	t.Run("with CLAUDE_CONFIG_DIR it sits inside that directory", func(t *testing.T) {
		home, ccd := t.TempDir(), t.TempDir()
		r := New(home, platform.MacOS)
		r.ConfigDir = ccd
		want := filepath.Join(ccd, ".claude.json")
		if got := r.GlobalConfigPath(); got != want {
			t.Errorf("GlobalConfigPath() = %q, want %q", got, want)
		}
	})

	t.Run("an existing legacy .config.json wins", func(t *testing.T) {
		home := t.TempDir()
		configHome := filepath.Join(home, ".claude")
		mkdirAll(t, configHome)
		legacy := filepath.Join(configHome, ".config.json")
		writeFile(t, legacy, "{}")

		r := New(home, platform.MacOS)
		if got := r.GlobalConfigPath(); got != legacy {
			t.Errorf("GlobalConfigPath() = %q, want the legacy %q", got, legacy)
		}
	})

	t.Run("a legacy .config.json inside CLAUDE_CONFIG_DIR wins", func(t *testing.T) {
		home, ccd := t.TempDir(), t.TempDir()
		legacy := filepath.Join(ccd, ".config.json")
		writeFile(t, legacy, "{}")

		r := New(home, platform.MacOS)
		r.ConfigDir = ccd
		if got := r.GlobalConfigPath(); got != legacy {
			t.Errorf("GlobalConfigPath() = %q, want the legacy %q", got, legacy)
		}
	})
}

// DefaultGlobalConfigPath and DefaultClaudeConfigHome must ignore
// CLAUDE_CONFIG_DIR: session sharing mirrors the user's real profile, and would
// otherwise source from whichever session it happens to be invoked inside.
func TestDefaultPathsIgnoreConfigDir(t *testing.T) {
	home, ccd := t.TempDir(), t.TempDir()
	r := New(home, platform.MacOS)
	r.ConfigDir = ccd

	if got, want := r.DefaultClaudeConfigHome(), filepath.Join(home, ".claude"); got != want {
		t.Errorf("DefaultClaudeConfigHome() = %q, want %q", got, want)
	}
	if got, want := r.DefaultGlobalConfigPath(), filepath.Join(home, ".claude.json"); got != want {
		t.Errorf("DefaultGlobalConfigPath() = %q, want %q", got, want)
	}
}

func TestCredentialsPath(t *testing.T) {
	home := t.TempDir()

	r := New(home, platform.MacOS)
	if got, want := r.CredentialsPath(), filepath.Join(home, ".claude", ".credentials.json"); got != want {
		t.Errorf("CredentialsPath() = %q, want %q", got, want)
	}

	ccd := t.TempDir()
	r.ConfigDir = ccd
	if got, want := r.CredentialsPath(), filepath.Join(ccd, ".credentials.json"); got != want {
		t.Errorf("CredentialsPath() with CLAUDE_CONFIG_DIR = %q, want %q", got, want)
	}
}

// BackupRoot follows XDG on Linux and WSL and keeps the legacy layout
// elsewhere. Platform is a Resolver field precisely so every case is reachable
// from one host.
func TestBackupRoot(t *testing.T) {
	home := t.TempDir()
	xdgDefault := filepath.Join(home, ".local", "share", "claude-swap")
	legacy := filepath.Join(home, LegacyBackupDirName)

	// Absolute for the HOST, not for the platform under test: the resolver's
	// own IsAbs runs on this machine, and a Windows runner reads "/opt/xdg" as
	// relative and drops it — which is the branch this row exists to avoid.
	absXDG := filepath.Join(filepath.VolumeName(home)+string(filepath.Separator), "opt", "xdg")

	tests := []struct {
		name     string
		platform platform.Platform
		xdg      string
		want     string
	}{
		{"linux without XDG_DATA_HOME", platform.Linux, "", xdgDefault},
		{"linux with absolute XDG_DATA_HOME", platform.Linux, absXDG, filepath.Join(absXDG, "claude-swap")},
		// Per the XDG spec an empty or relative value must be ignored.
		{"linux ignores empty XDG_DATA_HOME", platform.Linux, "", xdgDefault},
		{"linux ignores relative XDG_DATA_HOME", platform.Linux, "relative/path", xdgDefault},
		// systemd units and Dockerfiles set env vars without shell expansion,
		// so a literal ~/foo has to resolve.
		{"linux expands a leading tilde", platform.Linux, "~/custom-data", filepath.Join(home, "custom-data", "claude-swap")},
		{"wsl uses the XDG layout", platform.WSL, "", xdgDefault},
		{"macos uses the legacy layout", platform.MacOS, "", legacy},
		{"windows uses the legacy layout", platform.Windows, "", legacy},
		// A non-XDG platform must ignore XDG_DATA_HOME even when it is set.
		{"macos ignores XDG_DATA_HOME", platform.MacOS, "/opt/xdg", legacy},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := New(home, tt.platform)
			r.XDGDataHome = tt.xdg
			if got := r.BackupRoot(); got != tt.want {
				t.Errorf("BackupRoot() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLegacyBackupRoot(t *testing.T) {
	home := t.TempDir()
	r := New(home, platform.Linux)
	if got, want := r.LegacyBackupRoot(), filepath.Join(home, LegacyBackupDirName); got != want {
		t.Errorf("LegacyBackupRoot() = %q, want %q", got, want)
	}
}

func mkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("MkdirAll(%q): %v", path, err)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	mkdirAll(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return string(b)
}

func exists_(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}
