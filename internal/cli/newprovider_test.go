package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/provider"
)

// This file is the design's central claim under test: adding an agent CLI is a
// DECLARATION, not a port.
//
// The provider below is deliberately the least aaswap could be told. There is
// no parser for its token, no usage source, no refresher, no liveness detector
// and no config beside the credential — the state cursor, antigravity and grok
// are all in until somebody logs into one and looks. If these tests pass, that
// state is enough to manage accounts for a tool nobody has written code for.

// declareMinimalProvider registers a provider with nothing but a name, a home
// and one secret file, for the duration of one test.
func declareMinimalProvider(t *testing.T, name string) provider.Spec {
	t.Helper()
	spec := provider.Spec{
		Name:  name,
		Home:  Home(name),
		Files: []provider.File{{Path: "auth.json", Role: provider.RoleSecret}},
	}
	if err := provider.Register(spec); err != nil {
		t.Fatalf("registering %s: %v", name, err)
	}
	t.Cleanup(func() { provider.Unregister(name) })
	return spec
}

// Home is the declaration's home directory for a made-up provider.
func Home(name string) provider.Home {
	return provider.Home{Env: strings.ToUpper(name) + "_HOME", Default: "." + name}
}

// logIn writes what a logged-in install of the made-up tool looks like: one
// opaque file whose format aaswap knows nothing about.
func (h *harness) logInAs(t *testing.T, spec provider.Spec, token string) {
	t.Helper()
	home := filepath.Join(h.switcher.Paths.Home, spec.Home.Default)
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"),
		[]byte(`{"opaque_session":"`+token+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The whole claim, end to end: capture two logins, list them, switch between
// them, and have the right bytes land in the right file.
func TestAnUndocumentedProviderIsFullyManageable(t *testing.T) {
	h := newHarness(t)
	spec := declareMinimalProvider(t, "madeup")
	home := filepath.Join(h.switcher.Paths.Home, spec.Home.Default)
	credential := filepath.Join(home, "auth.json")

	// Two logins, captured as they land. Neither has an address anywhere in
	// it, so both are named by a digest of the credential.
	h.logInAs(t, spec, "first-session")
	if code := h.run("--provider", "madeup", "login", "--capture"); code != ExitOK {
		t.Fatalf("capturing the first login: exit = %d: %s", code, h.stderr())
	}
	h.logInAs(t, spec, "second-session")
	if code := h.run("--provider", "madeup", "login", "--capture"); code != ExitOK {
		t.Fatalf("capturing the second login: exit = %d: %s", code, h.stderr())
	}

	s, err := h.app.NewSwitcher("madeup")
	if err != nil {
		t.Fatal(err)
	}
	roster, err := s.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	names := roster.Names()
	if len(names) != 2 {
		t.Fatalf("accounts = %v, want both logins stored separately", names)
	}

	// Renaming works, which is what makes a digest an acceptable default name.
	if code := h.run("--provider", "madeup", "account", "rename", names[0], "work"); code != ExitOK {
		t.Fatalf("renaming: exit = %d: %s", code, h.stderr())
	}

	// And the switch puts the right bytes in the right file. Everything above
	// could pass while this wrote nothing.
	if code := h.run("--provider", "madeup", "switch", "work"); code != ExitOK {
		t.Fatalf("switching: exit = %d: %s", code, h.stderr())
	}
	data, err := os.ReadFile(credential)
	if err != nil {
		t.Fatalf("reading the live credential: %v", err)
	}
	if !strings.Contains(string(data), "first-session") {
		t.Errorf("the live credential is %q, want the account that was switched to", data)
	}
}

// Every capability the declaration omits is reported unsupported, by name and
// with a reason — never silently skipped and never guessed at.
func TestAnUndocumentedProviderReportsItsGaps(t *testing.T) {
	h := newHarness(t)
	declareMinimalProvider(t, "madeup")

	if code := h.run("doctor", "--json"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	var row map[string]any
	for _, entry := range h.decodeJSON()["providers"].([]any) {
		candidate := entry.(map[string]any)
		if candidate["name"] == "madeup" {
			row = candidate
		}
	}
	if row == nil {
		t.Fatal("a registered provider is missing from the report")
	}

	if row["identityTier"] != "hash" {
		t.Errorf("identityTier = %v, want the hash fallback", row["identityTier"])
	}
	if row["usageScope"] != "none" {
		t.Errorf("usageScope = %v, want none declared", row["usageScope"])
	}

	capabilities := row["capabilities"].(map[string]any)
	for _, capability := range provider.BaselineCapabilities {
		if !supported(t, capabilities[string(capability)]) {
			t.Errorf("a minimal declaration cannot %q, so adding a provider is "+
				"not the two-line job the design claims", capability)
		}
	}
	for _, capability := range []provider.Capability{
		provider.CapSession, provider.CapUsage, provider.CapRefresh,
	} {
		detail := capabilities[string(capability)].(map[string]any)
		if detail["supported"] == true {
			t.Errorf("%q is claimed but never declared", capability)
		}
		if reason, _ := detail["reason"].(string); !strings.Contains(reason, "madeup") {
			t.Errorf("the reason for %q does not name the provider: %q", capability, reason)
		}
	}
}

// A command needing an undeclared capability refuses by name, and points at
// something that does work.
func TestAnUndocumentedProviderRefusesSessionsWithAWayForward(t *testing.T) {
	h := newHarness(t)
	spec := declareMinimalProvider(t, "madeup")
	h.logInAs(t, spec, "only-session")
	if code := h.run("--provider", "madeup", "login", "--capture"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}

	if code := h.run("--provider", "madeup", "run", "anything"); code != ExitError {
		t.Fatalf("exit = %d, want a refusal: %s", code, h.stdout())
	}
	wantContains(t, h.stderr(), "madeup", "switch")
}

// Two providers' stores must not see each other, however late one was declared.
func TestAnUndocumentedProviderIsIsolatedFromTheRest(t *testing.T) {
	h := newHarness(t)
	spec := declareMinimalProvider(t, "madeup")

	h.login("one", "one@example.com")
	if code := h.run("login", "--capture"); code != ExitOK {
		t.Fatalf("storing a Claude account: exit = %d: %s", code, h.stderr())
	}
	h.logInAs(t, spec, "unrelated")
	if code := h.run("--provider", "madeup", "login", "--capture"); code != ExitOK {
		t.Fatalf("storing a madeup account: exit = %d: %s", code, h.stderr())
	}

	for _, tc := range []struct{ provider, want string }{
		{"claude", "one@example.com"},
		{"madeup", ""},
	} {
		s, err := h.app.NewSwitcher(tc.provider)
		if err != nil {
			t.Fatal(err)
		}
		roster, err := s.RosterOrEmpty()
		if err != nil {
			t.Fatal(err)
		}
		if len(roster.Names()) != 1 {
			t.Fatalf("%s holds %v, want exactly its own account", tc.provider, roster.Names())
		}
		account := roster.Accounts[roster.Names()[0]]
		if account.Email != tc.want {
			t.Errorf("%s's account has email %q, want %q — the stores are crossed",
				tc.provider, account.Email, tc.want)
		}
	}
}

// Export and import are in BaselineCapabilities, so a provider with the minimal
// declaration has to round-trip. This is the assertion that keeps that promise
// honest — the capability table saying "supported" is not evidence.
func TestAnUndocumentedProviderRoundTripsThroughAnExport(t *testing.T) {
	h := newHarness(t)
	spec := declareMinimalProvider(t, "madeup")
	h.logInAs(t, spec, "only-session")
	if code := h.run("--provider", "madeup", "login", "--capture"); code != ExitOK {
		t.Fatalf("storing: exit = %d: %s", code, h.stderr())
	}

	archive := filepath.Join(t.TempDir(), "accounts.aaswap")
	if code := h.run("--provider", "madeup", "account", "export", archive); code != ExitOK {
		t.Fatalf("export: exit = %d: %s", code, h.stderr())
	}
	if code := h.run("--provider", "madeup", "account", "remove", "--all", "--yes"); code != ExitOK {
		t.Fatalf("removing: exit = %d: %s", code, h.stderr())
	}
	if code := h.run("--provider", "madeup", "account", "import", archive, "--yes"); code != ExitOK {
		t.Fatalf("import: exit = %d: %s", code, h.stderr())
	}

	s, err := h.app.NewSwitcher("madeup")
	if err != nil {
		t.Fatal(err)
	}
	roster, err := s.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Names()) != 1 {
		t.Fatalf("accounts = %v, want the one that was exported", roster.Names())
	}
	name := roster.Names()[0]
	value, unreadable := s.Creds.ReadAccount(name, roster.Accounts[name].Email)
	if unreadable || !strings.Contains(value, "only-session") {
		t.Errorf("the restored credential is %q, want the exported one", value)
	}
}

// Capturing the same credential twice must refresh the one account, not add a
// second.
//
// With no address to compare, the digest is the only thing that can tell "this
// login is already stored" from "this is a new one". A roster record with no
// fingerprint compares equal to every other identityless record, which reads as
// "already stored" for the wrong account — or, when nothing matches, adds a
// duplicate on every capture.
func TestRecapturingAnUndocumentedProvidersLoginDoesNotDuplicateIt(t *testing.T) {
	h := newHarness(t)
	spec := declareMinimalProvider(t, "madeup")
	h.logInAs(t, spec, "the-only-session")

	for i := range 3 {
		if code := h.run("--provider", "madeup", "login", "--capture"); code != ExitOK {
			t.Fatalf("capture %d: exit = %d: %s", i+1, code, h.stderr())
		}
	}

	s, err := h.app.NewSwitcher("madeup")
	if err != nil {
		t.Fatal(err)
	}
	roster, err := s.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Names()) != 1 {
		t.Errorf("accounts = %v, want one — the same login was stored repeatedly",
			roster.Names())
	}
}

// A different credential IS a different account, even with no address to tell
// them apart.
func TestADifferentCredentialIsADifferentUndocumentedAccount(t *testing.T) {
	h := newHarness(t)
	spec := declareMinimalProvider(t, "madeup")

	for _, token := range []string{"session-a", "session-b"} {
		h.logInAs(t, spec, token)
		if code := h.run("--provider", "madeup", "login", "--capture"); code != ExitOK {
			t.Fatalf("capturing %s: exit = %d: %s", token, code, h.stderr())
		}
	}

	s, err := h.app.NewSwitcher("madeup")
	if err != nil {
		t.Fatal(err)
	}
	roster, err := s.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Names()) != 2 {
		t.Errorf("accounts = %v, want two distinct ones", roster.Names())
	}
	// And each carries the digest that identifies it, or the next capture
	// cannot tell them apart either.
	seen := map[string]bool{}
	for _, name := range roster.Names() {
		fingerprint := roster.Accounts[name].Fingerprint
		if fingerprint == "" {
			t.Errorf("%s was stored with no fingerprint", name)
			continue
		}
		if seen[fingerprint] {
			t.Errorf("two accounts share the fingerprint %q", fingerprint)
		}
		seen[fingerprint] = true
	}
}
