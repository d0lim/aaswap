package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/paths"
)

// unpinned returns the harness to production's rule: no provider is the
// default, and the invocation has to work out which one is meant.
func (h *harness) unpinned(t *testing.T) {
	t.Helper()
	t.Setenv(ProviderEnv, "")
}

// `aaswap login` used to mean Claude Code whatever was on the machine. Where
// the store says which tool is in use, that is the one addressed, and no one
// is asked anything.
func TestTheOnlyStoredProviderIsAddressedWithoutBeingNamed(t *testing.T) {
	h := newHarness(t)
	h.twoCodexAccounts(t)
	h.unpinned(t)
	asked := h.choosing("1")

	if code := h.run("list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "work@example.com", "personal@example.com")
	if len(*asked) != 0 {
		t.Errorf("asked %q, want no question: the store already said Codex", *asked)
	}
}

// With nothing to go on, a person at a terminal is asked — and the answer is
// what the command then addresses.
func TestAnUndecidableProviderIsPutToThePerson(t *testing.T) {
	h := newHarness(t)
	h.unpinned(t)
	asked := h.choosing("2") // registry order: claude, codex

	// A capture on an empty store fails either way; the point is WHOSE
	// failure. (A plain `login` with a person to ask would wait for one.)
	if code := h.run("login", "--capture"); code != ExitError {
		t.Fatalf("exit = %d, want a failure: %s", code, h.stdout())
	}
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "Which tool") {
		t.Fatalf("asked %q, want the tool question", *asked)
	}
	wantContains(t, h.stderr(), "no active Codex account")
	if strings.Contains(h.stderr(), "Claude") {
		t.Errorf("the answer was Codex, but the error talks about Claude:\n%s", h.stderr())
	}
}

// Both tools stored is the same question, asked every time until --provider
// or AASWAP_PROVIDER settles it.
func TestTwoStoredProvidersAreAQuestion(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	h.twoCodexAccounts(t)
	h.unpinned(t)
	asked := h.choosing("1")

	if code := h.run("list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if len(*asked) != 1 {
		t.Fatalf("asked %q, want exactly one question", *asked)
	}
	wantContains(t, h.stdout(), "one@example.com")
	if strings.Contains(h.stdout(), "work@example.com") {
		t.Errorf("the answer was Claude, but Codex accounts are listed:\n%s", h.stdout())
	}
}

// Declining the question touches nothing.
func TestDecliningTheProviderQuestionCancels(t *testing.T) {
	h := newHarness(t)
	h.unpinned(t)
	h.choosing("q")
	if code := h.run("login", "--capture"); code != ExitError {
		t.Fatalf("exit = %d, want a failure: %s", code, h.stdout())
	}
	wantContains(t, h.stderr(), "no provider chosen")
}

// A script cannot answer a question, so it is told which two ways there are
// to have said it up front.
func TestAnUndecidableProviderUnderJSONIsAnError(t *testing.T) {
	h := newHarness(t)
	h.unpinned(t)
	if code := h.run("list", "--json"); code != ExitError {
		t.Fatalf("exit = %d, want a failure: %s", code, h.stdout())
	}
	envelope, ok := h.decodeJSON()["error"].(map[string]any)
	if !ok {
		t.Fatalf("stdout is not an error envelope: %s", h.stdout())
	}
	message, _ := envelope["message"].(string)
	wantContains(t, message, "--provider", ProviderEnv, "claude", "codex")
}

// The flag and the variable still answer for the store, whatever it holds.
func TestANamedProviderIsNeverQuestioned(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	h.twoCodexAccounts(t)
	h.unpinned(t)
	asked := h.choosing("1")

	if code := h.run("--provider", "codex", "list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "work@example.com")
	t.Setenv(ProviderEnv, "codex")
	if code := h.run("list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "work@example.com")
	if len(*asked) != 0 {
		t.Errorf("asked %q, want no question", *asked)
	}
}

// `login` is about a NEW account, and the store cannot say which tool it is
// for: having only Claude Code accounts stored is exactly the state of someone
// adding their first Codex account. So a terminal is asked every time.
func TestLoginAsksWhichToolEvenWhenOnlyOneIsStored(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	h.unpinned(t)
	asked := h.choosing("2") // registry order: claude, codex
	run := h.loggingIn(t, paths.CodexHomeEnv, func(home string) error {
		return os.WriteFile(filepath.Join(home, "auth.json"), codexAuthJSON(t, "work@example.com", "acct-9", "plus"), 0o600)
	})

	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if len(*asked) != 1 || !strings.Contains((*asked)[0], "Which tool is this login for") {
		t.Fatalf("asked %q, want the tool question", *asked)
	}
	if got := strings.Join(run.argv, " "); got != "codex login" {
		t.Errorf("ran %q, want Codex's login: the answer was Codex", got)
	}
	wantContains(t, h.stdout(), "Added work: work@example.com")
}

// The same command in a script keeps the store's answer: a script cannot be
// asked, and the one tool with accounts is the only sensible reading.
func TestLoginWithoutATerminalStillTakesTheOnlyStoredTool(t *testing.T) {
	h := newHarness(t)
	h.twoCodexAccounts(t)
	h.unpinned(t)
	h.codexLogin("third@example.com", "acct-3", "plus")

	if code := h.run("login", "--capture"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Added third: third@example.com")
}
