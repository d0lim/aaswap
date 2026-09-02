package credstore

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/d0lim/aaswap/internal/paths"
	"github.com/d0lim/aaswap/internal/platform"
)

// The derivation is pinned against Claude Code's own: aaswap and Claude Code must
// name the same Keychain item, or neither can see the other's credential.
func TestKeychainServiceName(t *testing.T) {
	t.Run("is a stable 8-hex-digit suffix", func(t *testing.T) {
		got := KeychainServiceName("/Users/alice/.claude-work")
		const prefix = "Claude Code-credentials-"
		if len(got) != len(prefix)+8 {
			t.Fatalf("service name %q is not the prefix plus 8 hex digits", got)
		}
		if got[:len(prefix)] != prefix {
			t.Errorf("service name %q lacks the %q prefix", got, prefix)
		}
		if second := KeychainServiceName("/Users/alice/.claude-work"); second != got {
			t.Error("the derivation is not deterministic")
		}
	})

	// Claude Code hashes the raw exported string. A trailing slash or a leading
	// ./ is part of that string, so it changes the item — cleaning the path
	// here would send the lookup somewhere Claude Code never wrote.
	t.Run("hashes the raw string, unmodified", func(t *testing.T) {
		base := KeychainServiceName("/tmp/profile")
		for _, variant := range []string{"/tmp/profile/", "./tmp/profile", "/tmp//profile", "/tmp/./profile"} {
			if got := KeychainServiceName(variant); got == base {
				t.Errorf("KeychainServiceName(%q) collapsed to the cleaned path's name", variant)
			}
		}
	})

	// NFC normalization matters for any profile path with accented characters:
	// the same directory typed two ways must hash to one item.
	t.Run("NFC-normalizes before hashing", func(t *testing.T) {
		// Written as escapes, not literals, so an editor or a reformat cannot
		// silently normalize the file and turn this into a vacuous comparison
		// of two identical strings — which the assertion below also catches.
		composed := "/tmp/caf\u00e9"    // é as one code point
		decomposed := "/tmp/cafe\u0301" // e + combining acute
		if composed == decomposed {
			t.Fatal("the two spellings are byte-identical; this test proves nothing")
		}
		if KeychainServiceName(composed) != KeychainServiceName(decomposed) {
			t.Error("the two Unicode spellings of the same path hash to different items")
		}
	})
}

func TestActiveProfileIsDefault(t *testing.T) {
	t.Run("no CLAUDE_CONFIG_DIR is the default profile", func(t *testing.T) {
		r := paths.New(t.TempDir(), platform.MacOS)
		if !ActiveProfileIsDefault(r) {
			t.Error("ActiveProfileIsDefault = false with no CLAUDE_CONFIG_DIR")
		}
	})

	// A fresh machine has no ~/.claude yet; the profile is still the default.
	t.Run("works before the config home exists", func(t *testing.T) {
		home := t.TempDir()
		r := paths.New(home, platform.MacOS)
		if _, err := os.Stat(r.ClaudeConfigHome()); err == nil {
			t.Fatal("setup: the config home should not exist yet")
		}
		if !ActiveProfileIsDefault(r) {
			t.Error("a missing config home was not recognized as the default profile")
		}
	})

	t.Run("a different directory is not the default profile", func(t *testing.T) {
		r := paths.New(t.TempDir(), platform.MacOS)
		r.ConfigDir = t.TempDir()
		if ActiveProfileIsDefault(r) {
			t.Error("ActiveProfileIsDefault = true for a separate profile directory")
		}
	})

	// CLAUDE_CONFIG_DIR pointed at the default profile is still the default
	// profile — the question is where the path resolves, not whether the
	// variable is set.
	t.Run("CLAUDE_CONFIG_DIR naming the default is still the default", func(t *testing.T) {
		home := t.TempDir()
		r := paths.New(home, platform.MacOS)
		r.ConfigDir = filepath.Join(home, ".claude")
		if !ActiveProfileIsDefault(r) {
			t.Error("an explicit CLAUDE_CONFIG_DIR naming the default was treated as another profile")
		}
	})

	// Resolution collapses symlinks, which is how a profile reached through one
	// shows up as itself.
	t.Run("a symlink to the default resolves to the default", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("symlink creation needs elevation on Windows")
		}
		home := t.TempDir()
		real := filepath.Join(home, ".claude")
		if err := os.MkdirAll(real, 0o700); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(home, "link-to-claude")
		if err := os.Symlink(real, link); err != nil {
			t.Fatal(err)
		}

		r := paths.New(home, platform.MacOS)
		r.ConfigDir = link
		if !ActiveProfileIsDefault(r) {
			t.Error("a symlink to the default profile was treated as another profile")
		}
	})
}

func TestActiveOAuthKeychainServices(t *testing.T) {
	home := t.TempDir()
	otherProfile := t.TempDir()

	t.Run("the default profile reads the unsuffixed item", func(t *testing.T) {
		r := paths.New(home, platform.MacOS)
		if got := ActiveOAuthKeychainServices(r); !slices.Equal(got, []string{ClaudeOAuthService}) {
			t.Errorf("services = %v, want [%q]", got, ClaudeOAuthService)
		}
	})

	// Reading the fixed name from a custom profile would return a credential
	// belonging to a different account, while the identity read one layer up
	// reports the custom profile's — a silent mispairing.
	t.Run("a custom profile reads its own hashed item and nothing else", func(t *testing.T) {
		r := paths.New(home, platform.MacOS)
		r.ConfigDir = otherProfile

		got := ActiveOAuthKeychainServices(r)
		want := []string{KeychainServiceName(otherProfile)}
		if !slices.Equal(got, want) {
			t.Errorf("services = %v, want %v", got, want)
		}
		if slices.Contains(got, ClaudeOAuthService) {
			t.Error("a custom profile was allowed to fall back to the default profile's item")
		}
	})

	// The one case with a fallback: Claude Code would write a suffixed item for
	// an explicit CLAUDE_CONFIG_DIR, but a user who has always used the default
	// profile may only have the unsuffixed one.
	t.Run("CLAUDE_CONFIG_DIR equal to the default tries both, hashed first", func(t *testing.T) {
		r := paths.New(home, platform.MacOS)
		r.ConfigDir = filepath.Join(home, ".claude")

		got := ActiveOAuthKeychainServices(r)
		want := []string{KeychainServiceName(r.ConfigDir), ClaudeOAuthService}
		if !slices.Equal(got, want) {
			t.Errorf("services = %v, want %v", got, want)
		}
	})

	// Defined-but-empty selects the default secure store, whose item is the
	// unsuffixed one. Collapsing it into "unset" would still land here, but
	// collapsing it into "set" would send the lookup to a hash of "".
	t.Run("a defined but empty secure-storage dir selects the default store", func(t *testing.T) {
		r := paths.New(home, platform.MacOS)
		r.ConfigDir = otherProfile
		r.SecureStorageConfigDirSet = true
		r.SecureStorageConfigDir = ""

		if got := ActiveOAuthKeychainServices(r); !slices.Equal(got, []string{ClaudeOAuthService}) {
			t.Errorf("services = %v, want [%q]", got, ClaudeOAuthService)
		}
	})

	// A defined secure-storage dir names the only store Claude Code will read,
	// so there is no fallback: a miss means Claude Code sees a logged-out
	// profile, and reaching elsewhere would report a credential it is not using.
	t.Run("a defined secure-storage dir wins over CLAUDE_CONFIG_DIR with no fallback", func(t *testing.T) {
		secure := t.TempDir()
		r := paths.New(home, platform.MacOS)
		r.ConfigDir = otherProfile
		r.SecureStorageConfigDirSet = true
		r.SecureStorageConfigDir = secure

		got := ActiveOAuthKeychainServices(r)
		want := []string{KeychainServiceName(secure)}
		if !slices.Equal(got, want) {
			t.Errorf("services = %v, want %v", got, want)
		}
	})
}

func TestResolveLenient(t *testing.T) {
	t.Run("resolves an existing path", func(t *testing.T) {
		dir := t.TempDir()
		if got := resolveLenient(dir); got == "" {
			t.Error("resolveLenient returned empty for an existing directory")
		}
	})

	t.Run("re-appends the missing tail", func(t *testing.T) {
		dir := t.TempDir()
		missing := filepath.Join(dir, "does", "not", "exist")
		got := resolveLenient(missing)
		if filepath.Base(got) != "exist" {
			t.Errorf("resolveLenient(%q) = %q, want the missing tail preserved", missing, got)
		}
	})
}
