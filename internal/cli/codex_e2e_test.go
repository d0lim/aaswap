package cli

import (
	"encoding/base64"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/session"
)

// codexLogin writes what a logged-in Codex install looks like on this harness.
func (h *harness) codexLogin(email, accountID, plan string) {
	h.t.Helper()
	r := h.switcher.Paths
	if err := os.MkdirAll(r.CodexHome(), 0o700); err != nil {
		h.t.Fatal(err)
	}
	claims, err := json.Marshal(map[string]any{
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID, "chatgpt_plan_type": plan,
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	token := strings.Join([]string{enc([]byte(`{"alg":"none"}`)), enc(claims), ""}, ".")
	auth, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token": token, "access_token": "at", "refresh_token": "rt",
		},
	})
	if err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(r.CodexAuthPath(), auth, 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// The whole point of the dimension: a second provider costs no commands and no
// flags, only a value --provider accepts. This walks one end to end.
func TestCodexAccountsAreManagedThroughTheSameCommands(t *testing.T) {
	h := newHarness(t)
	h.codexLogin("work@example.com", "acct-9", "plus")

	if code := h.run("--provider", "codex", "login", "--capture"); code != ExitOK {
		t.Fatalf("login exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Added", "work@example.com", "plus")

	if code := h.run("--provider", "codex", "list"); code != ExitOK {
		t.Fatalf("list exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "work@example.com")
}

// Switching is the command aaswap exists for, and it had never been exercised
// for a second provider: Codex has no account-scoped config, and the switch
// demanded an identity block out of one before it would write anything.
func TestSwitchingBetweenCodexAccounts(t *testing.T) {
	h := newHarness(t)
	h.twoCodexAccounts(t)

	// personal is live after storing both. Move to work.
	if code := h.run("--provider", "codex", "switch", "work"); code != ExitOK {
		t.Fatalf("switch exit = %d: %s", code, h.stderr())
	}

	// The bytes have to land in Codex's own file, and be work's.
	data, err := os.ReadFile(h.switcher.Paths.CodexAuthPath())
	if err != nil {
		t.Fatalf("reading the live Codex credential: %v", err)
	}
	identity, ok := provider.CodexIdentity(string(data))
	if !ok {
		t.Fatalf("the live Codex credential carries no identity: %s", data)
	}
	if identity.Email != "work@example.com" {
		t.Errorf("live account = %q, want work@example.com", identity.Email)
	}

	// And the roster agrees, or `list` and reality disagree from here on.
	s, err := h.app.NewSwitcher("codex")
	if err != nil {
		t.Fatal(err)
	}
	roster, err := s.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if active, _ := roster.ActiveName(); active != "work" {
		t.Errorf("active = %q, want work", active)
	}

	// Switching must not have written Claude's config file into the Codex home,
	// nor touched the machine-scoped config.toml.
	if _, err := os.Stat(filepath.Join(h.switcher.Paths.CodexHome(), ".claude.json")); err == nil {
		t.Error("a Claude config was written into the Codex home")
	}
}

// The two providers must not see each other's accounts. They are separate auth
// domains, and a listing that mixed them would offer a switch that cannot work.
func TestTheProvidersDoNotSeeEachOther(t *testing.T) {
	h := newHarness(t)
	h.codexLogin("work@example.com", "acct-9", "plus")
	if code := h.run("--provider", "codex", "login", "--capture"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}

	// Claude's listing, on a machine whose only account is a Codex one.
	if code := h.run("list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if strings.Contains(h.stdout(), "work@example.com") {
		t.Errorf("the Claude listing shows a Codex account:\n%s", h.stdout())
	}

	// And the store holds both sections rather than one overwriting the other.
	file, _, err := h.switcher.StoreOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if roster := file.Providers["codex"]; roster == nil || len(roster.Accounts) != 1 {
		t.Errorf("the codex section holds %+v, want the captured account", roster)
	}
}

// twoCodexAccounts stores two Codex accounts and leaves the SECOND live.
//
// Two, because `run` short-circuits for the account that is already the default
// login — a test with one account exercises the fast path and never reaches
// session mode at all.
func (h *harness) twoCodexAccounts(t *testing.T) {
	t.Helper()
	for _, account := range []struct{ name, email, id string }{
		{"work", "work@example.com", "acct-1"},
		{"personal", "personal@example.com", "acct-2"},
	} {
		h.codexLogin(account.email, account.id, "plus")
		if code := h.run("--provider", "codex", "login", "--capture",
			"--name", account.name); code != ExitOK {
			t.Fatalf("storing %s: exit = %d: %s", account.name, code, h.stderr())
		}
	}
}

// Session mode used to be Claude Code's alone. It is now whatever a provider
// declares, and Codex declares it: CODEX_HOME repoints the whole home the way
// CLAUDE_CONFIG_DIR does, which is the one thing isolation needs.
func TestRunWorksForCodex(t *testing.T) {
	h := newHarness(t)
	h.twoCodexAccounts(t)
	h.onPath(t, "codex")
	record := h.capturing()

	// personal is live, so work has to go through a profile.
	if code := h.run("--provider", "codex", "run", "work"); code != ExitOK {
		t.Fatalf("exit = %d, want a launch: %s", code, h.stderr())
	}
	if !record.called {
		t.Fatal("nothing was launched")
	}
	// The binary has to be Codex's own. Launching `claude` for a Codex account
	// would run the wrong tool as the wrong account and look like it worked.
	if filepath.Base(record.binary) != "codex" {
		t.Errorf("launched %q, want codex", record.binary)
	}
	// Pinned by CODEX_HOME, or Codex reads the default home and runs as
	// whoever is logged in there.
	home, ok := record.env_("CODEX_HOME")
	if !ok || home == "" {
		t.Fatalf("CODEX_HOME was not set: the session is not isolated at all")
	}
	if !strings.Contains(home, "work") {
		t.Errorf("CODEX_HOME = %q, want work's profile directory", home)
	}
	// Claude's variable must NOT come along: it would repoint a Claude Code
	// that the session's tooling happens to invoke.
	if value, set := record.env_("CLAUDE_CONFIG_DIR"); set {
		t.Errorf("CLAUDE_CONFIG_DIR = %q leaked into a Codex session", value)
	}

	// And the profile must hold CODEX's credential file, not Claude's. This is
	// what the declaration decides, and getting it wrong seeds a profile the
	// tool cannot read while every other assertion here still passes.
	if _, err := os.Stat(filepath.Join(home, "auth.json")); err != nil {
		entries, _ := os.ReadDir(home)
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		t.Errorf("the profile holds %v, want Codex's auth.json", names)
	}
	if _, err := os.Stat(filepath.Join(home, ".credentials.json")); err == nil {
		t.Error("the profile holds Claude's .credentials.json: the session was " +
			"laid out with the wrong provider's declaration")
	}
}

// The fail-safe that lets Codex host sessions before anyone has worked out how
// to detect its running processes.
//
// aaswap cannot see a live Codex session, so it must never refresh a profile's
// credential on its own — the one thing that would yank a token out from under
// a running agent. It says so instead.
func TestCodexSessionsAreNotReseededSilently(t *testing.T) {
	h := newHarness(t)
	h.twoCodexAccounts(t)
	h.onPath(t, "codex")
	h.capturing()
	if code := h.run("--provider", "codex", "run", "work"); code != ExitOK {
		t.Fatalf("first launch: exit = %d: %s", code, h.stderr())
	}

	// Mark the profile stale, which for Claude would trigger a reseed.
	profile := h.sessionDir(t, "codex", "work", "work@example.com")
	if err := session.MarkStale(profile); err != nil {
		t.Fatal(err)
	}

	if code := h.run("--provider", "codex", "run", "work"); code != ExitOK {
		t.Fatalf("second launch: exit = %d: %s", code, h.stderr())
	}
	// The stale marker survives: nothing cleared it, because nothing reseeded.
	if !session.IsStale(profile) {
		t.Error("a Codex profile was reseeded despite aaswap being unable to " +
			"see whether a session was running against it")
	}
	// And the user is told why, or they get an old credential with no
	// explanation.
	wantContains(t, h.stderr()+h.stdout(), "cannot tell", "codex")
}

// Claude still reseeds, because it CAN prove a profile idle. Losing that would
// be a silent regression: every launch would quietly serve a stale credential.
func TestClaudeSessionsAreStillReseeded(t *testing.T) {
	h := newHarness(t)
	// Stored the way a person stores them, so the config backup a session
	// profile seeds from actually exists.
	for _, account := range []struct{ name, email string }{
		{"one", "one@example.com"}, {"two", "two@example.com"},
	} {
		h.login(account.name, account.email)
		if code := h.run("login", "--capture", "--name", account.name); code != ExitOK {
			t.Fatalf("storing %s: exit = %d: %s", account.name, code, h.stderr())
		}
	}
	h.onPath(t, "claude")
	h.capturing()
	if code := h.run("run", "one"); code != ExitOK {
		t.Fatalf("first launch: exit = %d: %s", code, h.stderr())
	}

	profile := h.sessionDir(t, "claude", "one", "one@example.com")
	if err := session.MarkStale(profile); err != nil {
		t.Fatal(err)
	}
	if code := h.run("run", "one"); code != ExitOK {
		t.Fatalf("second launch: exit = %d: %s", code, h.stderr())
	}
	if session.IsStale(profile) {
		t.Error("a quiet Claude profile was not reseeded, so the session runs " +
			"on a credential the server may already have rotated")
	}
}
