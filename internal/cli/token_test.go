package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/provider"
)

// `login --token` writes a credential in a format only Claude Code reads: an
// sk-ant- prefix classified as OAuth-or-managed-key, wrapped in a claudeAiOauth
// object. Offering it for every provider was not a cosmetic mismatch — a
// switch onto the resulting account writes that object into the other tool's
// credential file, where the login it replaces was the working one.
//
// The whole point of the declaration is that a capability aaswap does not have
// for a provider is REPORTED, not approximated with Claude's shape.

func TestStoringARawTokenIsRefusedWhereItWouldNotWork(t *testing.T) {
	h := newHarness(t)

	h.in.WriteString("sk-ant-oat01-not-a-real-token\n")
	if code := h.run("--provider", "codex", "login", "--token", "-"); code != ExitError {
		t.Fatalf("exit = %d, want a refusal: %s%s", code, h.stdout(), h.stderr())
	}
	wantContains(t, h.stderr(), "codex")

	// And nothing was filed under it. A refusal that still writes is worse
	// than no refusal, because the account looks managed.
	s, err := h.app.NewSwitcher("codex")
	if err != nil {
		t.Fatal(err)
	}
	roster, err := s.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Names()) != 0 {
		t.Errorf("accounts = %v, want none stored", roster.Names())
	}
}

// A provider declared with only the required fields has no token format either,
// and must be refused for the same reason rather than by name.
func TestStoringARawTokenIsRefusedForAnUndeclaredProvider(t *testing.T) {
	h := newHarness(t)
	spec := declareMinimalProvider(t, "madeup")

	h.in.WriteString("whatever-this-tool-calls-a-token\n")
	if code := h.run("--provider", "madeup", "login", "--token", "-"); code != ExitError {
		t.Fatalf("exit = %d, want a refusal: %s%s", code, h.stdout(), h.stderr())
	}
	wantContains(t, h.stderr(), "madeup")

	// Specifically: the credential file was not created behind the refusal.
	credential := filepath.Join(h.switcher.Paths.Home, spec.Home.Default, "auth.json")
	if _, err := os.Stat(credential); err == nil {
		data, _ := os.ReadFile(credential)
		t.Errorf("the live credential was written anyway: %s", data)
	}
}

// Claude declares a token format, so it keeps working.
func TestStoringARawTokenWorksWhereItIsDeclared(t *testing.T) {
	h := newHarness(t)

	if code := h.run("login", "--token", "sk-ant-api03-managed-key", "--name", "bykey"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	account, stored := roster.Accounts["bykey"]
	if !stored {
		t.Fatalf("accounts = %v, want the token account", roster.Names())
	}
	value, unreadable := h.switcher.Creds.ReadAccount("bykey", account.Email)
	if unreadable || !strings.Contains(value, "sk-ant-api03-managed-key") {
		t.Errorf("stored credential = %q, want the managed key verbatim", value)
	}
}

// And the gap is on the report, by name and with a reason, like every other one.
func TestDoctorReportsWhoCannotStoreARawToken(t *testing.T) {
	h := newHarness(t)
	if code := h.run("doctor", "--json"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	seen := map[string]map[string]any{}
	for _, entry := range h.decodeJSON()["providers"].([]any) {
		row := entry.(map[string]any)
		seen[row["name"].(string)] = row["capabilities"].(map[string]any)
	}
	if !supported(t, seen[provider.Claude][string(provider.CapToken)]) {
		t.Error("claude cannot store a raw token, but it declares the format")
	}
	if supported(t, seen[provider.Codex][string(provider.CapToken)]) {
		t.Error("codex claims it can store a raw token")
	}
}
