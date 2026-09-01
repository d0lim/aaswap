package lockfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/d0lim/ccswap/internal/apperr"
	"github.com/d0lim/ccswap/internal/paths"
	"github.com/d0lim/ccswap/internal/platform"
)

func testResolver(t *testing.T) *paths.Resolver {
	t.Helper()
	return paths.New(t.TempDir(), platform.MacOS)
}

// These paths are a compatibility contract with Claude Code: locking a
// different directory than it does provides no exclusion at all.
func TestLockDirectoryPaths(t *testing.T) {
	r := testResolver(t)

	tests := []struct {
		name string
		got  string
		want string
	}{
		{
			name: "legacy credentials lock sits beside the config home",
			got:  CredentialsLockDir(r),
			want: filepath.Join(r.Home, ".claude.lock"),
		},
		{
			name: "primary oauth refresh lock sits inside the config home",
			got:  OAuthRefreshLockDir(r),
			want: filepath.Join(r.Home, ".claude", ".oauth_refresh.lock"),
		},
		{
			name: "config lock is the global config path plus .lock",
			got:  ConfigLockDir(r),
			want: filepath.Join(r.Home, ".claude.json.lock"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

// Session mode relocates the whole profile through CLAUDE_CONFIG_DIR, and the
// locks have to follow it — otherwise two sessions would exclude each other
// through a lock neither of their Claude Codes is taking.
func TestLockDirectoriesHonorConfigDir(t *testing.T) {
	r := testResolver(t)
	ccd := t.TempDir()
	r.ConfigDir = ccd

	if got, want := OAuthRefreshLockDir(r), filepath.Join(ccd, ".oauth_refresh.lock"); got != want {
		t.Errorf("OAuthRefreshLockDir = %q, want %q", got, want)
	}
	if got, want := CredentialsLockDir(r), filepath.Join(filepath.Dir(ccd), filepath.Base(ccd)+".lock"); got != want {
		t.Errorf("CredentialsLockDir = %q, want %q", got, want)
	}
	if got, want := ConfigLockDir(r), filepath.Join(ccd, ".claude.json.lock"); got != want {
		t.Errorf("ConfigLockDir = %q, want %q", got, want)
	}
}

// Claude Code's refresh path takes both credential locks, so ccswap must hold
// both for the exclusion to actually cover the refresh window.
func TestCredentialsLockTakesBothLocks(t *testing.T) {
	r := testResolver(t)
	primary, legacy := OAuthRefreshLockDir(r), CredentialsLockDir(r)

	err := WithClaudeCredentials(r, fastOpts(0), func() error {
		for name, dir := range map[string]string{"primary": primary, "legacy": legacy} {
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("%s lock was not held inside the callback: %v", name, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithClaudeCredentials: %v", err)
	}
	for name, dir := range map[string]string{"primary": primary, "legacy": legacy} {
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s lock survived release", name)
		}
	}
}

// If the primary is contended we never get as far as the legacy lock, so the
// legacy lock must be left untouched for whoever does own the sequence.
func TestPrimaryContentionNeverTouchesLegacy(t *testing.T) {
	r := testResolver(t)
	primary, legacy := OAuthRefreshLockDir(r), CredentialsLockDir(r)
	if err := os.MkdirAll(primary, 0o700); err != nil {
		t.Fatal(err)
	}

	err := WithClaudeCredentials(r, fastOpts(0), func() error {
		t.Error("callback ran despite the primary lock being contended")
		return nil
	})
	if !errors.Is(err, apperr.ErrClaudeCodeLockTimeout) {
		t.Fatalf("error = %v, want a Claude Code lock timeout", err)
	}
	if _, err := os.Stat(legacy); !errors.Is(err, os.ErrNotExist) {
		t.Error("legacy lock was created even though the primary was never acquired")
	}
}

// Claude Code releases the primary before retrying a contended legacy lock.
// Mirroring that is what stops the two implementations waiting on each other.
func TestLegacyContentionReleasesPrimary(t *testing.T) {
	r := testResolver(t)
	primary, legacy := OAuthRefreshLockDir(r), CredentialsLockDir(r)
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}

	err := WithClaudeCredentials(r, fastOpts(0), func() error {
		t.Error("callback ran despite the legacy lock being contended")
		return nil
	})
	if !errors.Is(err, apperr.ErrClaudeCodeLockTimeout) {
		t.Fatalf("error = %v, want a Claude Code lock timeout", err)
	}
	if _, err := os.Stat(primary); !errors.Is(err, os.ErrNotExist) {
		t.Error("primary lock was left held after the legacy lock could not be taken")
	}
}

// A credential lock 30s old belongs to a live Claude Code whose toucher merely
// stalled — stealing it would let a swap interleave with a token refresh, which
// is the exact race these locks exist to close.
func TestCredentialsLockUsesSixtySecondStaleness(t *testing.T) {
	r := testResolver(t)
	for _, dir := range []string{OAuthRefreshLockDir(r), CredentialsLockDir(r)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		aged := time.Now().Add(-30 * time.Second)
		if err := os.Chtimes(dir, aged, aged); err != nil {
			t.Fatal(err)
		}
	}

	// Staleness is forced to 60s by WithClaudeCredentials, so the caller's
	// value here must be ignored.
	err := WithClaudeCredentials(r, ProperOptions{Timeout: 50 * time.Millisecond, Staleness: time.Second}, func() error {
		t.Error("a 30s-old credential lock was stolen from a live holder")
		return nil
	})
	if !errors.Is(err, apperr.ErrClaudeCodeLockTimeout) {
		t.Fatalf("error = %v, want a timeout rather than a takeover", err)
	}
}

func TestCredentialsLockTakesOverPastSixtySeconds(t *testing.T) {
	r := testResolver(t)
	for _, dir := range []string{OAuthRefreshLockDir(r), CredentialsLockDir(r)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		aged := time.Now().Add(-90 * time.Second)
		if err := os.Chtimes(dir, aged, aged); err != nil {
			t.Fatal(err)
		}
	}

	ran := false
	err := WithClaudeCredentials(r, ProperOptions{Timeout: time.Second, TouchInterval: 20 * time.Millisecond}, func() error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("a lock abandoned for 90s was not taken over: %v", err)
	}
	if !ran {
		t.Error("callback did not run")
	}
}

// The config lock keeps proper-lockfile's older 10s default, so a 30s-old one
// is genuinely abandoned and must be taken over rather than waited on.
func TestConfigLockKeepsTenSecondStaleness(t *testing.T) {
	r := testResolver(t)
	dir := ConfigLockDir(r)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	aged := time.Now().Add(-30 * time.Second)
	if err := os.Chtimes(dir, aged, aged); err != nil {
		t.Fatal(err)
	}

	ran := false
	err := WithClaudeConfig(r, ProperOptions{Timeout: time.Second, TouchInterval: 20 * time.Millisecond}, func() error {
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("a 30s-old config lock was not taken over under the 10s budget: %v", err)
	}
	if !ran {
		t.Error("callback did not run")
	}
}

func TestConfigLockIsHeldAndReleased(t *testing.T) {
	r := testResolver(t)
	dir := ConfigLockDir(r)

	if err := WithClaudeConfig(r, fastOpts(0), func() error {
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("config lock was not held inside the callback: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithClaudeConfig: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("config lock survived release")
	}
}
