package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/paths"
)

// `account adopt` takes over a store left by ccswap or claude-swap. Those
// predate providers, so every account in one is a Claude Code login — but the
// command read the roster through whichever provider the invocation addressed.
//
// `aaswap --provider codex account adopt` therefore moved the directory, found
// no accounts in the Codex section, and reported "0 account(s) are now
// aaswap's". The Keychain adoption it drives ran over an empty list and is
// never retried, so on macOS every credential the predecessor kept in the
// Keychain was left behind — with the directory it came from already moved.

// seedPredecessor writes a ccswap store: a version 1 table and its configs.
func seedPredecessor(t *testing.T, h *harness, accounts map[string]string) string {
	t.Helper()
	root := filepath.Join(h.switcher.Paths.Home, ".ccswap-backup")
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o700); err != nil {
		t.Fatal(err)
	}

	var entries, order []string
	for slot, email := range accounts {
		entries = append(entries, `"`+slot+`":{"email":"`+email+`"}`)
		order = append(order, `"`+slot+`"`)
		config := filepath.Join(root, "configs", ".claude-config-"+slot+"-"+email+".json")
		if err := os.WriteFile(config,
			[]byte(`{"oauthAccount":{"emailAddress":"`+email+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	table := `{"accounts":{` + strings.Join(entries, ",") + `},"order":[` +
		strings.Join(order, ",") + `]}`
	if err := os.WriteFile(filepath.Join(root, paths.RosterFileName),
		[]byte(table), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func TestAdoptingAPredecessorFilesItsAccountsUnderClaude(t *testing.T) {
	h := newHarness(t)
	seedPredecessor(t, h, map[string]string{"1": "one@example.com", "2": "two@example.com"})

	// Addressed at Codex, which is the invocation that lost the accounts.
	if code := h.run("--provider", "codex", "account", "adopt", "--yes"); code != ExitOK {
		t.Fatalf("adopt: exit = %d: %s", code, h.stderr())
	}

	// The store is Claude's afterwards, and Codex's section is untouched.
	if code := h.run("list"); code != ExitOK {
		t.Fatalf("list: exit = %d: %s", code, h.stderr())
	}
	for _, want := range []string{"one@example.com", "two@example.com"} {
		if !strings.Contains(h.stdout(), want) {
			t.Errorf("Claude does not list %s after the adoption:\n%s", want, h.stdout())
		}
	}
	if code := h.run("--provider", "codex", "list"); code != ExitOK {
		t.Fatalf("codex list: exit = %d: %s", code, h.stderr())
	}
	if strings.Contains(h.stdout(), "@example.com") {
		t.Errorf("Codex lists accounts out of a Claude store:\n%s", h.stdout())
	}
}

// The count it reports has to be the accounts it actually adopted, or a person
// with a full store is told it was empty and goes looking for what went wrong.
func TestAdoptingAPredecessorReportsWhatItTookOver(t *testing.T) {
	h := newHarness(t)
	seedPredecessor(t, h, map[string]string{"1": "one@example.com", "2": "two@example.com"})

	if code := h.run("--provider", "codex", "account", "adopt", "--yes"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "2 account(s)")
}

// And with nothing to adopt, the message says where it looked — as paths, not
// as a dump of the structs it looked through.
func TestAdoptWithNoPredecessorSaysWhereItLooked(t *testing.T) {
	h := newHarness(t)

	if code := h.run("account", "adopt"); code != ExitError {
		t.Fatalf("exit = %d, want an error: %s", code, h.stdout())
	}
	for _, want := range []string{".ccswap-backup", ".claude-swap-backup"} {
		if !strings.Contains(h.stderr(), want) {
			t.Errorf("the message does not name %s: %s", want, h.stderr())
		}
	}
	// A Go struct printed with %v: "{ccswap [/path] ccswap}".
	if strings.Contains(h.stderr(), "} {") {
		t.Errorf("the message dumps internal structs: %s", h.stderr())
	}
}
