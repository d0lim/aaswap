package cli

import (
	"os"
	"slices"
	"testing"
)

// A session profile is a whole synthetic home for one account: the credential,
// the config, and symlinks into the real home for everything the declaration
// calls shareable. The directory was named <backup>/sessions/<account>-<email>,
// with no provider in it — so the same address at two tools got ONE directory,
// and the two homes were merged inside it. The directory held
// [.aaswap-shared.json .claude.json .credentials.json auth.json]: two tools'
// live credentials, under one share manifest.
//
// The overlapping declarations are what make that dangerous rather than untidy.
// Claude and Codex both call `skills` and `history.jsonl` shareable, so one
// tool's customizations get linked into the other's home, and one `--no-share`
// removes links the other tool's session is using. Worse, `sessions` is Claude
// Code's own per-process record directory INSIDE a profile, while Codex declares
// `sessions` as shared history pointing at ~/.codex/sessions — the rollout files
// aaswap reads Codex's quota out of.

// sameAddressAtBothTools stores an account called work under one address at both
// providers, and leaves a DIFFERENT account live at each — so `run work` has to
// go through a profile rather than taking the same-account fast path.
func sameAddressAtBothTools(t *testing.T, h *harness) {
	t.Helper()
	const shared = "me@example.com"
	h.seed(map[string]string{"work": shared, "other": "other@example.com"})
	h.login("other", "other@example.com")

	h.codexLogin(shared, "acct-1", "plus")
	if code := h.run("--provider", "codex", "login", "--capture", "--name", "work"); code != ExitOK {
		t.Fatalf("storing the Codex account: exit = %d: %s", code, h.stderr())
	}
	h.codexLogin("elsewhere@example.com", "acct-2", "plus")
	if code := h.run("--provider", "codex", "login", "--capture", "--name", "other"); code != ExitOK {
		t.Fatalf("storing the second Codex account: exit = %d: %s", code, h.stderr())
	}
	h.onPath(t, "claude")
	h.onPath(t, "codex")
}

// launch runs a session and returns the home it pinned.
//
// The recorder is installed on every launch, without exception. Without one,
// `run` exec()s the stub and REPLACES the test process, whose exit 0 reads as a
// pass — which is how a first draft of this test reported success over the bug
// the second draft found.
func launch(t *testing.T, h *harness, homeEnv string, args ...string) string {
	t.Helper()
	record := h.capturing()
	if code := h.run(args...); code != ExitOK {
		t.Fatalf("%v: exit = %d: %s", args, code, h.stderr())
	}
	if !record.called {
		t.Fatalf("%v launched nothing", args)
	}
	home, set := record.env_(homeEnv)
	if !set || home == "" {
		t.Fatalf("%v did not set %s: the session is not isolated at all", args, homeEnv)
	}
	return home
}

func TestASessionProfileBelongsToOneProvider(t *testing.T) {
	h := newHarness(t)
	sameAddressAtBothTools(t, h)

	claudeHome := launch(t, h, "CLAUDE_CONFIG_DIR", "run", "work")
	codexHome := launch(t, h, "CODEX_HOME", "--provider", "codex", "run", "work")

	if claudeHome == codexHome {
		t.Fatalf("both tools were given the same home %s, holding %v",
			claudeHome, entryNames(t, claudeHome))
	}
}

// And each profile holds only its own provider's files. Sharing writes links
// after the profile exists, so this is what catches a directory that was right
// when it was created and merged on the next launch.
func TestASessionProfileHoldsOnlyItsOwnProvidersFiles(t *testing.T) {
	h := newHarness(t)
	sameAddressAtBothTools(t, h)

	claudeHome := launch(t, h, "CLAUDE_CONFIG_DIR", "run", "work")
	codexHome := launch(t, h, "CODEX_HOME", "--provider", "codex", "run", "work")

	// Each tool's credential is the unmistakable marker, and each has to be in
	// exactly one of the two profiles.
	for _, tc := range []struct {
		home, wants, rejects string
	}{
		{claudeHome, ".credentials.json", "auth.json"},
		{codexHome, "auth.json", ".credentials.json"},
	} {
		names := entryNames(t, tc.home)
		if !slices.Contains(names, tc.wants) {
			t.Errorf("%s holds %v, want its own credential %s", tc.home, names, tc.wants)
		}
		if slices.Contains(names, tc.rejects) {
			t.Errorf("%s holds %s, which belongs to the other tool (all: %v)",
				tc.home, tc.rejects, names)
		}
	}
}

func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	slices.Sort(names)
	return names
}
