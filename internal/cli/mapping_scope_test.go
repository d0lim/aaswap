package cli

import (
	"strings"
	"testing"
)

// A directory mapping answers "which account does this project belong to". The
// table was keyed by directory alone, so there was one answer per directory for
// every provider at once — and the same person's Claude and Codex logins are
// very often the same address, which is what made the crossing invisible.
//
// Three ways it went wrong, and the second is configuration loss:
//
//   - `aaswap --provider codex run` in a directory mapped for Claude resolved
//     the Claude mapping, then looked that address up in the Codex roster.
//   - Mapping the same directory for a second provider OVERWROTE the first
//     provider's mapping. One directory could not belong to a Claude account
//     and a Codex account at the same time, which is the ordinary case for
//     anyone using both.
//   - Removing an account pruned the other provider's mapping for the same
//     address.

func TestADirectoryCanBeMappedPerProvider(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"work": "shared@example.com"})
	h.twoCodexAccounts(t)
	dir := t.TempDir()

	if code := h.run("dir", "map", "work", dir); code != ExitOK {
		t.Fatalf("mapping for claude: exit = %d: %s", code, h.stderr())
	}
	if code := h.run("--provider", "codex", "dir", "map", "personal", dir); code != ExitOK {
		t.Fatalf("mapping for codex: exit = %d: %s", code, h.stderr())
	}

	// Both survive, and each names its own account.
	if code := h.run("dir", "list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if !strings.Contains(h.stdout(), "shared@example.com") {
		t.Errorf("the Claude mapping was overwritten by the Codex one:\n%s", h.stdout())
	}
	if code := h.run("--provider", "codex", "dir", "list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if !strings.Contains(h.stdout(), "personal@example.com") {
		t.Errorf("the Codex mapping is missing:\n%s", h.stdout())
	}
}

// One provider's mapping must not answer for another, even when nothing has
// been mapped for the one being asked.
func TestOneProvidersMappingDoesNotAnswerForAnother(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"work": "work@example.com"})
	h.twoCodexAccounts(t)
	dir := t.TempDir()

	if code := h.run("dir", "map", "work", dir); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}

	if code := h.run("--provider", "codex", "dir", "list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if strings.Contains(h.stdout(), "work@example.com") {
		t.Errorf("Codex sees a mapping made for Claude:\n%s", h.stdout())
	}
	if !strings.Contains(h.stdout(), "No directories are mapped") {
		t.Errorf("Codex reports mappings it has none of:\n%s", h.stdout())
	}
}

// And forgetting an account leaves the other provider's mapping alone.
func TestRemovingAnAccountLeavesTheOtherProvidersMapping(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"work": "shared@example.com"})
	h.twoCodexAccounts(t)
	dir := t.TempDir()

	if code := h.run("dir", "map", "work", dir); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if code := h.run("--provider", "codex", "dir", "map", "personal", dir); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if code := h.run("--provider", "codex", "account", "remove", "personal", "--yes"); code != ExitOK {
		t.Fatalf("removing: exit = %d: %s", code, h.stderr())
	}

	if code := h.run("dir", "list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if !strings.Contains(h.stdout(), "shared@example.com") {
		t.Errorf("removing a Codex account took Claude's mapping with it:\n%s", h.stdout())
	}
}
