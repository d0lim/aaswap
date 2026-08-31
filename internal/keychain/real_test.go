package keychain

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// This is the one test that runs against a genuine Keychain, so it is the only
// proof that the argv and stdin shapes the unit tests pin actually work against
// Apple's binary. It modifies the default keychain, so — exactly like the
// Python suite's mac_ci_only marker — it runs on GitHub Actions macOS only.
func requireRealKeychain(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "darwin" || os.Getenv("GITHUB_ACTIONS") != "true" {
		t.Skip("modifies the default Keychain — runs on GitHub Actions macOS only")
	}
	t.Setenv(AllowRealKeychainEnv, "1")
}

// tmpKeychain creates a temporary keychain, swaps it in as both the default and
// the sole user search-list entry, and restores both afterwards.
//
// The two settings are independent and both matter: `default-keychain` controls
// where new items go, while `list-keychains -d user` controls what
// find-generic-password searches. A read that passes no explicit keychain — as
// every call in this package does — only finds the seeded entry if both are
// redirected.
//
// The restore runs through t.Cleanup so a failing test still puts the host's
// keychain configuration back. CI does not care, but the safe shape is kept so
// this is risk-free if anyone runs it locally.
func tmpKeychain(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "test.keychain")

	run := func(args ...string) (string, error) {
		out, err := exec.Command(SecurityBinary, args...).Output()
		return string(out), err
	}
	mustRun := func(args ...string) {
		t.Helper()
		if _, err := run(args...); err != nil {
			t.Fatalf("security %s: %v", strings.Join(args, " "), err)
		}
	}

	mustRun("create-keychain", "-p", "", path)
	mustRun("unlock-keychain", "-p", "", path)

	// CI runners do not reliably have a default keychain configured ("A default
	// keychain could not be found"), and an earlier swap in the same job may
	// have cleared it. Capture it only if present, and skip that half of the
	// restore otherwise, rather than failing setup.
	originalDefault, defaultErr := run("default-keychain")
	originalList, listErr := run("list-keychains", "-d", "user")

	t.Cleanup(func() {
		if defaultErr == nil {
			if name := unquoteKeychainLine(originalDefault); name != "" {
				_, _ = run("default-keychain", "-s", name)
			}
		}
		if listErr == nil {
			if names := parseKeychainList(originalList); len(names) > 0 {
				_, _ = run(append([]string{"list-keychains", "-d", "user", "-s"}, names...)...)
			}
		}
		_, _ = run("delete-keychain", path)
	})

	mustRun("default-keychain", "-s", path)
	mustRun("list-keychains", "-d", "user", "-s", path)
}

func unquoteKeychainLine(s string) string {
	return strings.Trim(strings.TrimSpace(s), `"`)
}

func parseKeychainList(s string) []string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if name := unquoteKeychainLine(line); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// Covers the full production read/write/delete path end to end: a
// wrapper-created item (no -A any-app access) read back through the keychain
// search list with no explicit keychain argument, then deleted, with the rc-44
// "not found" contract checked at the end.
func TestRoundTripAgainstTheRealKeychain(t *testing.T) {
	requireRealKeychain(t)
	tmpKeychain(t)

	k := New()
	const service, account, secret = "claude-swap-test", "acct-1", "round-trip-token"

	if err := k.Set(service, account, secret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, found, err := k.Get(service, account)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || got != secret {
		t.Fatalf("Get = (%q, %v), want (%q, true)", got, found, secret)
	}
	if !k.Exists(service, account) {
		t.Error("Exists = false for an item that was just written")
	}

	if err := k.Delete(service, account); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, found, err := k.Get(service, account); err != nil || found {
		t.Errorf("after Delete, Get = (found=%v, err=%v), want a clean miss", found, err)
	}
	// Deleting again must stay a success: rc 44 is the outcome the caller wanted.
	if err := k.Delete(service, account); err != nil {
		t.Errorf("second Delete = %v, want nil (deletion is idempotent)", err)
	}
}

// Values whose length pushes the stdin command past security -i's line buffer
// take the argv fallback. Proving that against the real binary is the only way
// to know the fallback actually stores a recoverable value.
func TestOversizedValueRoundTripsThroughTheArgvFallback(t *testing.T) {
	requireRealKeychain(t)
	tmpKeychain(t)

	k := New()
	const service, account = "claude-swap-test", "acct-big"
	secret := strings.Repeat("x", stdinLineLimit)
	t.Cleanup(func() { _ = k.Delete(service, account) })

	if err := k.Set(service, account, secret); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, found, err := k.Get(service, account)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !found || got != secret {
		t.Errorf("oversized value did not survive the round trip (found=%v, len=%d, want %d)",
			found, len(got), len(secret))
	}
}
