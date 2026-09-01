package paths

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/d0lim/ccswap/internal/platform"
)

// The guard is the Go replacement for the Python suite's audit hook. It is the
// last thing standing between a test that forgets to redirect HOME and the
// developer's live Claude Code credentials, so it gets tested first and
// directly.

func TestGuardRejectsTheRealStore(t *testing.T) {
	if realHome == "" {
		t.Skip("no real home directory captured on this host")
	}

	tests := []struct {
		name     string
		resolver *Resolver
		wantHint string
	}{
		{
			name:     "home itself",
			resolver: New(realHome, platform.MacOS),
			wantHint: "home directory",
		},
		{
			name:     "a directory inside home",
			resolver: New(filepath.Join(realHome, "nested"), platform.Linux),
			wantHint: "home directory",
		},
		{
			// The nastiest shape: HOME is safely redirected, but
			// CLAUDE_CONFIG_DIR still names the real profile.
			name: "CLAUDE_CONFIG_DIR pointing back at the real profile",
			resolver: func() *Resolver {
				r := New(t.TempDir(), platform.MacOS)
				r.ConfigDir = filepath.Join(realHome, ".claude")
				return r
			}(),
			wantHint: "Claude config home",
		},
		{
			// Likewise for XDG_DATA_HOME steering the backup root home.
			name: "XDG_DATA_HOME pointing back at the real home",
			resolver: func() *Resolver {
				r := New(t.TempDir(), platform.Linux)
				r.XDGDataHome = filepath.Join(realHome, ".local", "share")
				return r
			}(),
			wantHint: "backup root",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				got := recover()
				if got == nil {
					t.Fatal("guardRealStore did not panic on a resolver pointing at the real store")
				}
				msg, ok := got.(string)
				if !ok {
					t.Fatalf("panic value = %#v, want a string", got)
				}
				if !strings.Contains(msg, tt.wantHint) {
					t.Errorf("panic message %q does not mention %q", msg, tt.wantHint)
				}
			}()
			guardRealStore(tt.resolver)
		})
	}
}

func TestGuardAllowsIsolatedResolvers(t *testing.T) {
	// A temp directory is outside the home directory on every supported
	// platform, so a correctly isolated test must sail straight through.
	for _, p := range []platform.Platform{platform.MacOS, platform.Linux, platform.WSL, platform.Windows} {
		t.Run(p.String(), func(t *testing.T) {
			defer func() {
				if got := recover(); got != nil {
					t.Fatalf("guardRealStore panicked on an isolated resolver: %v", got)
				}
			}()
			guardRealStore(New(t.TempDir(), p))
		})
	}
}

func TestWithinRealHome(t *testing.T) {
	if realHome == "" {
		t.Skip("no real home directory captured on this host")
	}
	tests := []struct {
		path string
		want bool
	}{
		{realHome, true},
		{filepath.Join(realHome, ".claude"), true},
		{filepath.Join(realHome, "a", "b", "c"), true},
		// Trailing separators and dot segments must not open a hole.
		{realHome + string(filepath.Separator), true},
		{filepath.Join(realHome, "x", ".."), true},
		{"", false},
		{t.TempDir(), false},
		// A sibling directory whose name merely starts with the home path.
		{realHome + "-not-really", false},
	}
	for _, tt := range tests {
		if got := withinRealHome(tt.path); got != tt.want {
			t.Errorf("withinRealHome(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

// FromEnv is the single choke point where the environment becomes paths, so the
// guard has to fire there too — not merely in the helper it calls.
func TestFromEnvPanicsOnTheRealHomeInTests(t *testing.T) {
	if realHome == "" {
		t.Skip("no real home directory captured on this host")
	}
	t.Setenv("HOME", realHome)
	t.Setenv("USERPROFILE", realHome)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")

	defer func() {
		if got := recover(); got == nil {
			t.Fatal("FromEnv returned the real store from a test binary instead of panicking")
		}
	}()
	if _, err := FromEnv(); err != nil {
		t.Fatalf("FromEnv returned an error rather than panicking: %v", err)
	}
}

// With HOME redirected, FromEnv is the normal, allowed path.
func TestFromEnvAcceptsRedirectedHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("XDG_DATA_HOME", "")

	r, err := FromEnv()
	if err != nil {
		t.Fatalf("FromEnv: %v", err)
	}
	if r.Home != home {
		t.Errorf("Home = %q, want %q", r.Home, home)
	}
}
