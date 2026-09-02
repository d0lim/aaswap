package cli

import (
	"os"
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/swap"
)

// fastWait collapses the login wait's polling. The behavior under test is what
// the wait does, never how long it takes to do it.
func (h *harness) fastWait() {
	h.app.awaitTuning = swap.AwaitOptions{Interval: time.Millisecond, Confirmations: 2}
}

// landLogin writes a finished login the way Claude Code would, from whatever
// goroutine calls it. Errors are reported rather than fatal because FailNow off
// the test goroutine does not stop the test.
func (h *harness) landLogin(email, uuid string) {
	config := `{"oauthAccount":{"emailAddress":"` + email + `","accountUuid":"` + uuid + `"},"projects":{}}`
	if err := os.WriteFile(h.switcher.Paths.GlobalConfigPath(), []byte(config), 0o600); err != nil {
		h.t.Error(err)
		return
	}
	if err := h.switcher.Creds.WriteActive(
		`{"claudeAiOauth":{"accessToken":"tok-` + uuid + `","refreshToken":"r-` + uuid + `"}}`); err != nil {
		h.t.Error(err)
	}
}

// The point of --wait: one command instead of "run add, read the error, go log
// in, come back, run add again".
func TestAddWaitCapturesTheLoginThatLands(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	h.login("1", "one@example.com")
	h.fastWait()

	landed := make(chan struct{})
	go func() {
		defer close(landed)
		time.Sleep(20 * time.Millisecond)
		h.landLogin("two@example.com", "acct-2")
	}()

	if code := h.run("login", "--wait"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	<-landed

	// The instructions have to name the thing to go and do, and the hazard of
	// doing it the obvious wrong way.
	wantContains(t, h.stdout(),
		"one@example.com", "already stored as 1",
		"/login", "Do not log out first", "Waiting",
		"two@example.com", "Added")

	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if account := roster.Accounts["two"]; account == nil || account.Email != "two@example.com" {
		t.Errorf("accounts = %v, want one called \"two\"", roster.Names())
	}
}

// Re-logging in as an account already stored is a credential refresh, and the
// wait must end on it rather than holding out for a stranger.
func TestAddWaitEndsOnAReLoginAndRefreshesInPlace(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	h.login("1", "one@example.com")
	h.fastWait()

	landed := make(chan struct{})
	go func() {
		defer close(landed)
		time.Sleep(20 * time.Millisecond)
		h.landLogin("two@example.com", "acct-2")
	}()

	if code := h.run("login", "--wait"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	<-landed

	wantContains(t, h.stdout(), "Updated credentials for", "two@example.com")

	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	// A refresh, not a second registration: two slots holding one account is
	// the state where the older one carries a token the server has retired.
	if len(roster.Accounts) != 2 {
		t.Errorf("the roster holds %d accounts, want 2: %v", len(roster.Accounts), roster.Accounts)
	}
}

// Without --wait, `add` on a machine with no login is unchanged for anything
// that is not a person at a terminal: a buffer is not one, so this must be the
// same error it has always been rather than a hang.
func TestAddWithoutAWaitStillFailsFastWhenNoOneIsLoggedIn(t *testing.T) {
	h := newHarness(t)
	if code := h.run("login"); code != ExitError {
		t.Fatalf("exit = %d, want a failure: %s", code, h.stdout())
	}
	wantContains(t, h.stderr(), "no active Claude Code account")
}

// --json is a machine asking a question, and a machine cannot go and log in.
func TestAddUnderJSONNeverWaits(t *testing.T) {
	h := newHarness(t)
	h.fastWait()
	if code := h.run("login", "--json"); code != ExitError {
		t.Fatalf("exit = %d, want a failure: %s", code, h.stdout())
	}
	payload := h.decodeJSON()
	envelope, ok := payload["error"].(map[string]any)
	if !ok {
		t.Fatalf("payload = %v, want an error envelope", payload)
	}
	if envelope["type"] != "ConfigError" {
		t.Errorf("error = %v, want the unchanged ConfigError", envelope)
	}
}
