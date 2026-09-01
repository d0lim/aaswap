package cli

import (
	"encoding/base64"
	json "encoding/json/v2"
	"os"
	"strings"
	"testing"
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
