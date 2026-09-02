package credstore

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/keychain"
	"github.com/d0lim/aaswap/internal/paths"
	"github.com/d0lim/aaswap/internal/platform"
)

// linuxStore builds a store on the platform where credentials are files, which
// is where the layout question lives — the Keychain has its own namespace.
func linuxStore(t *testing.T, root, provider string) *Store {
	t.Helper()
	r := paths.New(t.TempDir(), platform.Linux)
	kc := keychain.NewWithRunner(newFakeKeychain(), 0)
	if provider == "" {
		return New(r, root, kc)
	}
	return NewForProvider(r, root, kc, provider, Layout{Keychain: provider != "codex",
		LivePath: r.ProviderCredentialsPath("CODEX_HOME", ".codex", "auth.json")})
}

// Two providers can hold the same person's account under the same name: one
// address, one handle, two tools. Filing both in one place would give whichever
// wrote last both accounts' credentials.
func TestProvidersDoNotShareAStoragePlace(t *testing.T) {
	root := t.TempDir()
	claude := linuxStore(t, root, "claude")
	codex := linuxStore(t, root, "codex")

	const email, name = "same@example.com", "work"
	if err := claude.WriteAccount(name, email, `{"who":"claude"}`); err != nil {
		t.Fatal(err)
	}
	if err := codex.WriteAccount(name, email, `{"who":"codex"}`); err != nil {
		t.Fatal(err)
	}

	got, unreadable := claude.ReadAccount(name, email)
	if unreadable {
		t.Fatal("claude's credential became unreadable")
	}
	if !strings.Contains(got, "claude") {
		t.Errorf("claude reads %q — the other provider overwrote it", got)
	}
	if got, _ := codex.ReadAccount(name, email); !strings.Contains(got, "codex") {
		t.Errorf("codex reads %q", got)
	}

	// Separate directories, not just separate filenames: a person looking at
	// the store should see which tool an account belongs to.
	if claude.CredentialsDir() == codex.CredentialsDir() {
		t.Errorf("both providers file into %s", claude.CredentialsDir())
	}
	if filepath.Base(claude.CredentialsDir()) != "claude" {
		t.Errorf("claude files into %s", claude.CredentialsDir())
	}
}

// The unscoped layout is what every store written before providers existed
// uses. The upgrade has to be able to read it.
func TestTheLegacyLayoutIsStillReadable(t *testing.T) {
	root := t.TempDir()
	legacy := linuxStore(t, root, "")
	if err := legacy.WriteAccount("1", "one@example.com", `{"old":true}`); err != nil {
		t.Fatal(err)
	}
	if filepath.Base(legacy.CredentialsDir()) != "credentials" {
		t.Errorf("the legacy store files into %s, want the unscoped directory",
			legacy.CredentialsDir())
	}

	// A provider-scoped store must NOT see it: that is the whole point of the
	// upgrade having to move things.
	scoped := linuxStore(t, root, "claude")
	if got, _ := scoped.ReadAccount("1", "one@example.com"); got != "" {
		t.Errorf("the scoped store read the legacy credential as %q", got)
	}
	if got, _ := legacy.ReadAccount("1", "one@example.com"); got == "" {
		t.Error("the legacy store cannot read what it wrote")
	}
}

// Unscoped has to name the directory a version 1 store actually used. Deriving
// it by walking back up from the scoped path is how it came to point one level
// too deep — and every test that seeded through the same accessor agreed with
// the bug.
func TestUnscopedPointsAtTheRealLegacyDirectory(t *testing.T) {
	root := t.TempDir()
	scoped := linuxStore(t, root, "claude")
	want := filepath.Join(root, "credentials")
	if got := scoped.Unscoped().CredentialsDir(); got != want {
		t.Errorf("Unscoped().CredentialsDir() = %q, want %q", got, want)
	}
	if got := linuxStore(t, root, "").CredentialsDir(); got != want {
		t.Errorf("an unscoped store files into %q, want %q", got, want)
	}
}
