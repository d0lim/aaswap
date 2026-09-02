package credstore

import (
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/keychain"
	"github.com/d0lim/aaswap/internal/paths"
	"github.com/d0lim/aaswap/internal/platform"
)

// codexStore builds a store addressing Codex on macOS — the platform where
// getting the provider wrong would reach for a Keychain.
func codexStore(t *testing.T) (*Store, *paths.Resolver) {
	t.Helper()
	r := paths.New(t.TempDir(), platform.MacOS)
	if err := os.MkdirAll(r.CodexHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	kc := keychain.NewWithRunner(newFakeKeychain(), 0)
	return NewForProvider(r, t.TempDir(), kc, "codex",
		Layout{LivePath: r.CodexAuthPath()}), r
}

const codexAuth = `{"auth_mode":"chatgpt","tokens":{"id_token":"a.b.","access_token":"at"}}`

// Codex keeps its live credential in one plaintext file and nothing else. A
// store addressing it must read that file, on every platform.
func TestCodexActiveCredentialIsTheAuthFile(t *testing.T) {
	s, r := codexStore(t)
	if err := os.WriteFile(r.CodexAuthPath(), []byte(codexAuth), 0o600); err != nil {
		t.Fatal(err)
	}

	got := s.ReadActive()
	if !strings.Contains(got.Value, "chatgpt") {
		t.Errorf("value = %q, want the auth file", got.Value)
	}
	// A single-source store is never degraded: degraded means "the bytes may
	// be a superseded generation because a fresher store could not be asked",
	// and there is no fresher store here.
	if got.Degraded || got.KeychainUnavailable {
		t.Errorf("a file-only provider reported %+v", got)
	}
}

// Writing has to land where the tool reads, with owner-only permissions: this
// file holds a refresh token.
func TestCodexActiveCredentialWriteRoundTrips(t *testing.T) {
	s, r := codexStore(t)
	if err := s.WriteActive(codexAuth); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(r.CodexAuthPath())
	if err != nil {
		t.Fatalf("the credential did not land where Codex reads: %v", err)
	}
	if string(data) != codexAuth {
		t.Errorf("wrote %q", data)
	}
	if runtime.GOOS != "windows" { // POSIX modes are not meaningful there
		info, err := os.Stat(r.CodexAuthPath())
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("mode = %o, want 0600 on a file holding a refresh token", perm)
		}
	}
	if got := s.ReadActive(); got.Value != codexAuth {
		t.Errorf("read back %q", got.Value)
	}
}

// An absent file is an empty store, not a failed read — a fresh machine has
// never logged in.
func TestCodexWithNoLoginReadsEmpty(t *testing.T) {
	s, _ := codexStore(t)
	got := s.ReadActive()
	if got.Value != "" || got.FileReadFailed {
		t.Errorf("ReadActive = %+v, want an empty store", got)
	}
}
