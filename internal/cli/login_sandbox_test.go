package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/paths"
)

// toolRun records what the login runner was asked to do.
type toolRun struct {
	argv  []string
	env   []string
	calls int
}

// home is the directory the environment pointed the tool at.
func (r *toolRun) home(t *testing.T, variable string) string {
	t.Helper()
	for _, entry := range r.env {
		if value, ok := strings.CutPrefix(entry, variable+"="); ok {
			return value
		}
	}
	t.Fatalf("the login was not pointed anywhere: %s is not in its environment", variable)
	return ""
}

// loggingIn installs a login runner that behaves like the tool would: it
// finishes a login into the home the environment points it at. It also makes
// the harness interactive, which is what makes `login` run a login at all.
func (h *harness) loggingIn(t *testing.T, variable string, land func(home string) error) *toolRun {
	t.Helper()
	record := &toolRun{}
	h.app.RunTool = func(_ context.Context, argv, env []string) error {
		record.argv, record.env = argv, env
		record.calls++
		return land(record.home(t, variable))
	}
	if h.app.Choose == nil {
		h.app.Choose = func(string, []Choice) string { return "" }
	}
	return record
}

// landClaude writes what `claude auth login` leaves in a config directory.
func landClaude(email, uuid, token string) func(home string) error {
	return func(home string) error {
		config := `{"oauthAccount":{"emailAddress":"` + email + `","accountUuid":"` + uuid + `"},"projects":{}}`
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(config), 0o600); err != nil {
			return err
		}
		credentials := `{"claudeAiOauth":{"accessToken":"` + token + `","refreshToken":"r-` + token + `"}}`
		return os.WriteFile(filepath.Join(home, ".credentials.json"), []byte(credentials), 0o600)
	}
}

// sandboxesLeft counts login sandboxes still on disk.
func (h *harness) sandboxesLeft(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(h.switcher.BackupRoot(), "login"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return len(entries)
}

// The feature: `aaswap login` logs in. It runs the tool's own login pointed at
// a sandbox, stores what lands, and leaves the live login exactly as it was —
// which the old "log out, log in as the other one" dance could not, because
// current Claude Code revokes the token on logout.
func TestLoginRunsTheToolIntoASandboxAndStoresWhatLands(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"one": "one@example.com"})
	h.login("one", "one@example.com")
	run := h.loggingIn(t, paths.ClaudeConfigDirEnv, landClaude("two@example.com", "acct-2", "tok-2"))

	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}

	if got := strings.Join(run.argv, " "); got != "claude auth login --claudeai" {
		t.Errorf("ran %q, want Claude Code's login subcommand", got)
	}
	home := run.home(t, paths.ClaudeConfigDirEnv)
	if !strings.HasPrefix(home, filepath.Join(h.switcher.BackupRoot(), "login")+string(filepath.Separator)) {
		t.Errorf("the login was pointed at %q, want a sandbox under the backup root", home)
	}
	wantContains(t, h.stdout(), "Logging in with Claude Code", "left as it is",
		"Added two: two@example.com", "aaswap switch two")

	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if roster.Accounts["two"] == nil {
		t.Errorf("accounts = %v, want two stored", roster.Names())
	}
	// The live login is one's, before and after.
	if roster.Active != "one" {
		t.Errorf("active = %q after a sandboxed login, want one: the live login was not touched", roster.Active)
	}
	if live, ok := h.switcher.LiveIdentity(); !ok || live.Email != "one@example.com" {
		t.Errorf("live identity = %+v, want one@example.com untouched", live)
	}
	if stored, _ := h.switcher.Creds.ReadAccount("two", "two@example.com"); !strings.Contains(stored, "tok-2") {
		t.Errorf("two's stored credential = %q, want the one the login produced", stored)
	}
	if n := h.sandboxesLeft(t); n != 0 {
		t.Errorf("%d login sandboxes left on disk, want none: each holds a live token", n)
	}
}

// On a machine with no login, "log in" means "and use it".
func TestLoginOnAMachineWithNoLoginMakesTheNewAccountLive(t *testing.T) {
	h := newHarness(t)
	h.loggingIn(t, paths.ClaudeConfigDirEnv, landClaude("new@example.com", "acct-n", "tok-n"))

	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Added new: new@example.com", "Now the live login")
	if live, ok := h.switcher.LiveIdentity(); !ok || live.Email != "new@example.com" {
		t.Errorf("live identity = %+v, want the new account made live", live)
	}
	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if roster.Active != "new" {
		t.Errorf("active = %q, want new", roster.Active)
	}
	if n := h.sandboxesLeft(t); n != 0 {
		t.Errorf("%d login sandboxes left on disk, want none", n)
	}
}

// The same operation for Codex, through its own home variable and command.
func TestLoginForCodexPointsCodexHomeAtTheSandbox(t *testing.T) {
	h := newHarness(t)
	run := h.loggingIn(t, paths.CodexHomeEnv, func(home string) error {
		return os.WriteFile(filepath.Join(home, "auth.json"), codexAuthJSON(t, "work@example.com", "acct-9", "plus"), 0o600)
	})

	if code := h.run("--provider", "codex", "login"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if got := strings.Join(run.argv, " "); got != "codex login" {
		t.Errorf("ran %q, want `codex login`", got)
	}
	wantContains(t, h.stdout(), "Logging in with Codex", "Added work: work@example.com")
	if n := h.sandboxesLeft(t); n != 0 {
		t.Errorf("%d login sandboxes left on disk, want none", n)
	}
}

// A login that did not complete stores nothing and leaves nothing behind.
func TestAFailedToolLoginStoresNothing(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"one": "one@example.com"})
	h.loggingIn(t, paths.ClaudeConfigDirEnv, func(string) error { return errors.New("exit status 1") })

	if code := h.run("login"); code != ExitError {
		t.Fatalf("exit = %d, want a failure: %s", code, h.stdout())
	}
	wantContains(t, h.stderr(), "claude auth login --claudeai", "did not complete")
	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Accounts) != 1 {
		t.Errorf("accounts = %v, want the one there was", roster.Names())
	}
	if n := h.sandboxesLeft(t); n != 0 {
		t.Errorf("%d login sandboxes left after a failure, want none", n)
	}
}

// The tool exiting cleanly is not the same as a login landing: a closed
// browser tab ends the command with nothing written.
func TestAToolLoginThatLandsNothingIsSaidAndCleanedUp(t *testing.T) {
	h := newHarness(t)
	h.loggingIn(t, paths.ClaudeConfigDirEnv, func(string) error { return nil })

	if code := h.run("login"); code != ExitError {
		t.Fatalf("exit = %d, want a failure: %s", code, h.stdout())
	}
	wantContains(t, h.stderr(), "ended without a Claude Code account landing", "Nothing was changed")
	if n := h.sandboxesLeft(t); n != 0 {
		t.Errorf("%d login sandboxes left, want none", n)
	}
}

// Older Claude Code moved the config with CLAUDE_CONFIG_DIR but kept writing
// the credential to the shared store. The live login is then overwritten with
// the new account's token, and the sandbox holds an identity with no credential
// under it. Both have to be put right: the live store gets its credential back,
// and the new account gets the one the login produced.
func TestAToolThatWroteIntoTheLiveStoreIsPutRight(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"one": "one@example.com"})
	h.login("one", "one@example.com")
	h.loggingIn(t, paths.ClaudeConfigDirEnv, func(home string) error {
		config := `{"oauthAccount":{"emailAddress":"two@example.com","accountUuid":"acct-2"},"projects":{}}`
		if err := os.WriteFile(filepath.Join(home, ".claude.json"), []byte(config), 0o600); err != nil {
			return err
		}
		// Into the LIVE store, not the sandbox.
		return h.switcher.Creds.WriteActive(`{"claudeAiOauth":{"accessToken":"tok-2","refreshToken":"r-2"}}`)
	})

	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Added two: two@example.com")
	if live := h.switcher.Creds.ReadActive().Value; !strings.Contains(live, "tok-one") {
		t.Errorf("the live credential = %q, want one's restored", live)
	}
	if stored, _ := h.switcher.Creds.ReadAccount("two", "two@example.com"); !strings.Contains(stored, "tok-2") {
		t.Errorf("two's stored credential = %q, want the one the login produced", stored)
	}
}

// Being logged in as an account that is not stored yet is the one state where
// running a login would ask someone to log in as who they already are.
func TestALiveUnstoredLoginIsCapturedRatherThanReLoggedIn(t *testing.T) {
	h := newHarness(t)
	h.login("one", "one@example.com")
	run := h.loggingIn(t, paths.ClaudeConfigDirEnv, landClaude("two@example.com", "acct-2", "tok-2"))

	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Added one: one@example.com")
	if run.calls != 0 {
		t.Errorf("the tool's login ran %d times for an account that was already logged in", run.calls)
	}
}

// Logging in again as a stored account is a refresh of it, not a second copy —
// the same rule a capture follows.
func TestReLoggingInAsAStoredAccountRefreshesIt(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"one": "one@example.com"})
	h.login("one", "one@example.com")
	h.loggingIn(t, paths.ClaudeConfigDirEnv, landClaude("one@example.com", "acct-one", "tok-fresh"))

	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Updated credentials for one")
	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Accounts) != 1 {
		t.Errorf("accounts = %v, want the one refreshed in place", roster.Names())
	}
	if stored, _ := h.switcher.Creds.ReadAccount("one", "one@example.com"); !strings.Contains(stored, "tok-fresh") {
		t.Errorf("stored = %q, want the fresh credential", stored)
	}
}

// Anything in the environment that could redirect where the tool reads or
// writes its credential is scrubbed, or the sandbox is not one.
func TestTheLoginEnvironmentCannotRedirectTheCredential(t *testing.T) {
	h := newHarness(t)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-api03-elsewhere")
	t.Setenv("CLAUDE_SECURESTORAGE_CONFIG_DIR", "/somewhere/else")
	run := h.loggingIn(t, paths.ClaudeConfigDirEnv, landClaude("new@example.com", "acct-n", "tok-n"))

	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	for _, entry := range run.env {
		for _, banned := range []string{"ANTHROPIC_API_KEY=", "CLAUDE_SECURESTORAGE_CONFIG_DIR="} {
			if strings.HasPrefix(entry, banned) {
				t.Errorf("the login environment carries %q", entry)
			}
		}
	}
}

// --name pins the handle on this path too.
func TestAToolLoginTakesAName(t *testing.T) {
	h := newHarness(t)
	h.loggingIn(t, paths.ClaudeConfigDirEnv, landClaude("new@example.com", "acct-n", "tok-n"))
	if code := h.run("login", "--name", "Work"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if roster.Accounts["work"] == nil {
		t.Errorf("accounts = %v, want one called \"work\"", roster.Names())
	}
}

// A stored account whose activation failed is a stored account. Reporting the
// failure as the login's would send the person to log in again, filing the
// same account and failing the same way.
func TestAnActivationThatFailsAfterTheLoginStillReportsTheStore(t *testing.T) {
	h := newHarness(t)
	h.loggingIn(t, paths.ClaudeConfigDirEnv, landClaude("new@example.com", "acct-n", "tok-n"))
	// The live credential file cannot be written: its directory is a file.
	home := h.switcher.Paths.ClaudeConfigHome()
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(home, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit = %d, want success for a stored account: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Added new: new@example.com",
		"could not make it the live login", "aaswap switch new")
	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if roster.Accounts["new"] == nil {
		t.Errorf("accounts = %v, want new stored despite the failed activation", roster.Names())
	}
	if n := h.sandboxesLeft(t); n != 0 {
		t.Errorf("%d login sandboxes left, want none", n)
	}
}
