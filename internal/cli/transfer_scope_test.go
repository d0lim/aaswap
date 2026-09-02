package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An export file exists to be moved: to another machine, to a backup, to a
// colleague. Nothing in it said which tool's logins it held, and nothing at
// import checked — so
//
//	aaswap account export accounts.aaswap
//	aaswap --provider codex account import accounts.aaswap --yes
//
// filed Claude Code credentials as Codex accounts, and reported success. The
// next `aaswap --provider codex switch` writes a {"claudeAiOauth":…} object
// into ~/.codex/auth.json, over the login that worked.
//
// Two ordinary commands, one wrong word, and the file names give no hint: both
// archives end in .aaswap and both look alike inside.

func TestAnArchiveIsRefusedByADifferentProvider(t *testing.T) {
	h := newHarness(t)
	// A Claude-only address, so an account appearing in the Codex store can
	// only have come out of this archive.
	h.seed(map[string]string{"claudeside": "only-claude@example.com"})
	h.twoCodexAccounts(t)

	archive := filepath.Join(t.TempDir(), "accounts.aaswap")
	if code := h.run("account", "export", archive); code != ExitOK {
		t.Fatalf("export: exit = %d: %s", code, h.stderr())
	}

	if code := h.run("--provider", "codex", "account", "import", archive, "--yes"); code != ExitError {
		t.Fatalf("a Claude archive imported into the Codex store: exit = %d\n%s",
			code, h.stdout())
	}
	// The refusal has to name both, or it is a puzzle rather than a message.
	wantContains(t, h.stderr(), "claude", "codex")

	// And nothing was written behind it.
	s, err := h.app.NewSwitcher("codex")
	if err != nil {
		t.Fatal(err)
	}
	roster, err := s.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range roster.Names() {
		if roster.Accounts[name].Email == "only-claude@example.com" {
			t.Errorf("%s was filed in the Codex store from a Claude archive", name)
		}
	}
}

// The archive says whose it is, so a person can tell two files apart without
// importing them.
func TestAnArchiveNamesItsProvider(t *testing.T) {
	h := newHarness(t)
	h.twoCodexAccounts(t)

	archive := filepath.Join(t.TempDir(), "accounts.aaswap")
	if code := h.run("--provider", "codex", "account", "export", archive); code != ExitOK {
		t.Fatalf("export: exit = %d: %s", code, h.stderr())
	}
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"provider"`) {
		t.Errorf("the archive does not record which tool it came from:\n%s", data)
	}
	if !strings.Contains(string(data), "codex") {
		t.Errorf("the archive does not name codex:\n%s", data)
	}
}

// An archive written before this — there is no provider in it — is Claude's,
// because Claude is the only provider that existed when it could have been
// written. Refusing it would strand every backup anyone already has.
func TestAnArchiveWithNoProviderIsClaudes(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"work": "work@example.com"})

	archive := filepath.Join(t.TempDir(), "accounts.aaswap")
	if code := h.run("account", "export", archive); code != ExitOK {
		t.Fatalf("export: exit = %d: %s", code, h.stderr())
	}
	// Strip the field, which is what an older aaswap's file looks like.
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	stripped := strings.Replace(string(data), `"provider": "claude",`, "", 1)
	if stripped == string(data) {
		t.Fatalf("the provider field was not where this test expected it:\n%s", data)
	}
	if err := os.WriteFile(archive, []byte(stripped), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := h.run("account", "remove", "--all", "--yes"); code != ExitOK {
		t.Fatalf("clearing: exit = %d: %s", code, h.stderr())
	}
	if code := h.run("account", "import", archive, "--yes"); code != ExitOK {
		t.Fatalf("an archive from an older release was refused: exit = %d: %s",
			code, h.stderr())
	}
}
