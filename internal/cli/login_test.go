package cli

import (
	"strings"
	"testing"
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
		wantContains(t, h.stderr(), "no active Claude Code account")
	})
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
