package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/provider"
)

// These are the places where the provider work moved the mechanism but a
// Claude-shaped constant stayed behind. None of them was failing yet; each
// would have the moment someone leaned on it.

// A session drops the environment variables that would override the account
// inside the tool — or `run work` silently runs as something else. The list
// was Claude's three, hardcoded. A Codex session with OPENAI_API_KEY exported
// ran as the key, not as the account, and said nothing.
func TestASessionScrubsItsOwnProvidersAuthOverrides(t *testing.T) {
	h := newHarness(t)
	h.twoCodexAccounts(t)
	h.onPath(t, "codex")
	t.Setenv("OPENAI_API_KEY", "sk-proj-would-override-the-account")

	record := h.capturing()
	if code := h.run("--provider", "codex", "run", "work"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if value, set := record.env_("OPENAI_API_KEY"); set {
		t.Errorf("OPENAI_API_KEY=%q reached the Codex session, which will run as the "+
			"key rather than as work", value)
	}
	// And it was said, not silently dropped: the person exported that variable
	// for a reason and should know it is not in effect here.
	wantContains(t, h.stdout(), "OPENAI_API_KEY")
}

// The waiting screen tells the person how to log in. It said "claude, then
// /login" for every provider; Codex has no /login, it has `codex login`.
func TestWaitingForALoginNamesTheProvidersOwnCommand(t *testing.T) {
	h := newHarness(t)
	h.twoCodexAccounts(t)

	// A different login lands while it waits, so the wait ends the way it is
	// meant to. What is under test is what it printed first.
	h.fastWait()
	go func() {
		time.Sleep(20 * time.Millisecond)
		h.codexLogin("third@example.com", "acct-3", "plus")
	}()
	if code := h.run("--provider", "codex", "login", "--wait"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}

	out := h.stdout()
	if strings.Contains(out, "/login") {
		t.Errorf("a Codex wait tells the user to run /login, which is Claude Code's:\n%s", out)
	}
	if !strings.Contains(out, "codex login") {
		t.Errorf("the wait does not name `codex login`:\n%s", out)
	}
}

// A provider's name is now a directory segment in the vault, the sessions tree
// and the configs tree, and a filename suffix for the mappings and usage
// tables. A name that is not a plain identifier escapes all four.
func TestAProviderNameMustBeAPlainIdentifier(t *testing.T) {
	for _, name := range []string{"../claude", "a/b", "", "Codex", "with space", ".hidden", "-lead"} {
		spec := provider.Spec{
			Name:  name,
			Home:  provider.Home{Default: ".x"},
			Files: []provider.File{{Path: "auth.json", Role: provider.RoleSecret}},
		}
		if err := provider.Register(spec); err == nil {
			provider.Unregister(name)
			t.Errorf("a provider named %q was accepted; it becomes a path segment", name)
		}
	}
}

// A Codex install signed in with an API key has an auth.json and no account in
// it. "No active Codex account found. Log in first" tells someone who IS logged
// in to log in.
func TestCapturingAnAPIKeyLoginSaysWhatItFound(t *testing.T) {
	h := newHarness(t)
	h.codexAPIKeyLogin(t)

	if code := h.run("--provider", "codex", "login", "--capture"); code != ExitError {
		t.Fatalf("exit = %d, want a refusal: %s", code, h.stdout())
	}
	wantContains(t, h.stderr(), "API key")
	if strings.Contains(h.stderr(), "Log in first") {
		t.Errorf("the refusal tells a logged-in user to log in:\n%s", h.stderr())
	}
}

// codexAPIKeyLogin writes what a Codex install signed in with an API key looks
// like: an auth.json with the key and no tokens, so no account in it.
func (h *harness) codexAPIKeyLogin(t *testing.T) {
	t.Helper()
	home := h.switcher.Paths.CodexHome()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "auth.json"),
		[]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-proj-not-real"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}
