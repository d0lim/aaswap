package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/usagestore"
)

// choosing makes the harness interactive and answers every prompt with the
// given key, recording what it was asked.
func (h *harness) choosing(key string) *[]string {
	var asked []string
	h.app.Choose = func(prompt string, options []Choice) string {
		asked = append(asked, prompt)
		for _, option := range options {
			if option.Key == key {
				return key
			}
		}
		return ""
	}
	return &asked
}

// --capture is the old `add`: take what is live, ask nothing.
func TestLoginCaptureTakesTheLiveLogin(t *testing.T) {
	h := newHarness(t)
	h.login("one", "one@example.com")

	if code := h.run("login", "--capture"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Added", "one@example.com")
}

// --token is the old `add-token`: no live login needed at all.
func TestLoginTokenRegistersWithoutALiveLogin(t *testing.T) {
	h := newHarness(t)
	h.app.In = strings.NewReader("sk-ant-oat01-piped\n")

	if code := h.run("login", "--token", "-"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Accounts) != 1 {
		t.Fatalf("accounts = %v, want the token registered", roster.Accounts)
	}
}

// The flags are three answers to one question, so naming two is not a request
// this can satisfy.
func TestLoginRefusesContradictoryFlags(t *testing.T) {
	for _, args := range [][]string{
		{"login", "--capture", "--wait"},
		{"login", "--capture", "--token", "sk-ant-oat01-x"},
		{"login", "--wait", "--token", "sk-ant-oat01-x"},
	} {
		t.Run(strings.Join(args[1:], " "), func(t *testing.T) {
			h := newHarness(t)
			if code := h.run(args...); code != ExitError {
				t.Errorf("exit = %d, want a refusal: %s", code, h.stdout())
			}
		})
	}
}

// A machine cannot be asked and cannot go and log in, so the non-interactive
// answers are exactly what `add` did before.
func TestLoginWithoutAnyoneToAsk(t *testing.T) {
	t.Run("an unstored live login is captured", func(t *testing.T) {
		h := newHarness(t)
		h.login("one", "one@example.com")
		if code := h.run("login"); code != ExitOK {
			t.Fatalf("exit = %d: %s", code, h.stderr())
		}
		wantContains(t, h.stdout(), "Added")
	})

	t.Run("a stored live login is refreshed in place", func(t *testing.T) {
		h := newHarness(t)
		h.seed(map[string]string{"one": "one@example.com"})
		h.login("one", "one@example.com")
		if code := h.run("login"); code != ExitOK {
			t.Fatalf("exit = %d: %s", code, h.stderr())
		}
		wantContains(t, h.stdout(), "Updated credentials for")
	})

	t.Run("nothing logged in is an error, never a wait", func(t *testing.T) {
		h := newHarness(t)
		if code := h.run("login"); code != ExitError {
			t.Fatalf("exit = %d, want the unchanged error: %s", code, h.stdout())
		}
		wantContains(t, h.stderr(), "no active Claude account")
	})
}

// The argument `add` could never settle: when the live login is already stored,
// "refresh this" and "add another" are both plausible and the command has no
// way to know. So it asks, once, naming what it found.
func TestLoginAsksWhenTheLiveLoginIsAlreadyStored(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"one": "one@example.com"})
	h.login("one", "one@example.com")
	asked := h.choosing("r")

	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if len(*asked) != 1 {
		t.Fatalf("asked %v, want exactly one question", *asked)
	}
	// The prompt has to name the account and the slot, or it is asking about
	// nothing a person can identify.
	if !strings.Contains((*asked)[0], "one@example.com") {
		t.Errorf("the prompt does not name the account: %q", (*asked)[0])
	}
	wantContains(t, h.stdout(), "Updated credentials for")
}

// The same question, answered the other way, becomes the wait.
func TestLoginCanWaitFromThePrompt(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"one": "one@example.com"})
	h.login("one", "one@example.com")
	h.choosing("w")
	h.fastWait()

	landed := make(chan struct{})
	go func() {
		defer close(landed)
		time.Sleep(20 * time.Millisecond)
		h.landLogin("two@example.com", "acct-2")
	}()

	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	<-landed
	wantContains(t, h.stdout(), "Waiting", "two@example.com", "Added")
}

// Declining changes nothing.
func TestLoginCancelsCleanly(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"one": "one@example.com"})
	h.login("one", "one@example.com")
	h.choosing("q")

	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Cancelled")
}

// An account whose refresh token is dead has exactly one plausible reason to be
// logged in again. Asking would be asking a question with one answer.
func TestLoginRefreshesADeadAccountWithoutAsking(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"one": "one@example.com"})
	h.login("one", "one@example.com")
	h.quarantine("one", "one@example.com")
	asked := h.choosing("q") // would cancel, if it were asked

	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if len(*asked) != 0 {
		t.Errorf("asked %v about an account with one plausible answer", *asked)
	}
	wantContains(t, h.stdout(), "Updated credentials for")
}

// With nothing logged in there is nothing to ask about: the wait IS the answer.
func TestLoginWaitsWithNoLiveLoginAndNoQuestion(t *testing.T) {
	h := newHarness(t)
	asked := h.choosing("q")
	h.fastWait()

	landed := make(chan struct{})
	go func() {
		defer close(landed)
		time.Sleep(20 * time.Millisecond)
		h.landLogin("two@example.com", "acct-2")
	}()

	if code := h.run("login"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	<-landed
	if len(*asked) != 0 {
		t.Errorf("asked %v when there was nothing to choose between", *asked)
	}
	wantContains(t, h.stdout(), "Added", "two@example.com")
}

// `login --name` still pins the handle, whichever path it takes.
func TestLoginNamesTheAccount(t *testing.T) {
	h := newHarness(t)
	h.login("one", "one@example.com")

	if code := h.run("login", "--capture", "--name", "Work"); code != ExitOK {
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

// quarantine records that an account's stored refresh token was refused, which
// is the state a re-login is the only sensible answer to.
func (h *harness) quarantine(name, email string) {
	h.t.Helper()
	ids := map[string]usagestore.Identity{name: {Email: email}}
	if _, err := h.switcher.Usage.Record(map[string]usagestore.FetchRecord{
		name: {Error: claudeapi.KindInvalidGrant, StruckFP: "sha256:dead"},
	}, ids, nil, nil); err != nil {
		h.t.Fatal(err)
	}
	if !h.switcher.Usage.Entries(ids, nil)[name].TokenDead("") {
		h.t.Fatal("the account was not quarantined")
	}
}
