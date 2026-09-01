package provider

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/d0lim/aaswap/internal/platform"
)

func seedProfile(t *testing.T, dir, credentials string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"),
		[]byte(credentials), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A provider whose tool keeps no Keychain item must never be handed one. This
// is the whole point of the seam: the Keychain is a property of one provider on
// one operating system, and code above must not assume it exists.
func TestAFileOnlyProfileStoreNeverReachesForAKeychain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	seedProfile(t, dir, `{"claudeAiOauth":{"accessToken":"from-the-file"}}`)

	// Built with no Keychain at all — the shape a non-macOS host, and every
	// file-only provider, has.
	store := NewProfiles(MustLookup(Claude), platform.Linux, nil)

	if got := store.Read(dir); got == "" {
		t.Error("the file was not read")
	}
	if !store.MayHold(dir) {
		t.Error("a profile with a credential file reported as definitely empty")
	}
	// Clearing must not panic, and must leave the file — that is the caller's
	// to remove.
	store.Clear(dir)
	if _, err := os.Stat(filepath.Join(dir, ".credentials.json")); err != nil {
		t.Errorf("Clear removed the credential file: %v", err)
	}
}

// An empty profile is definitely empty only when every store it could use says
// so. With no Keychain, the file is every store there is.
func TestAnEmptyFileOnlyProfileIsDefinitelyEmpty(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	store := NewProfiles(MustLookup(Claude), platform.Linux, nil)

	if store.Read(dir) != "" {
		t.Error("an empty profile read as holding something")
	}
	if store.MayHold(dir) {
		t.Error("an empty profile reported as maybe holding a credential")
	}
}
