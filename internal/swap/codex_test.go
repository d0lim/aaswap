package swap

import (
	"encoding/base64"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/keychain"
	"github.com/d0lim/aaswap/internal/paths"
	"github.com/d0lim/aaswap/internal/platform"
	"github.com/d0lim/aaswap/internal/settings"
	"github.com/d0lim/aaswap/internal/usagestore"
)

// codexFixture is a Switcher addressing Codex, over a temp home.
func codexFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, ".local", "share", paths.BackupDirName)
	r := paths.New(home, platform.Linux)
	for _, dir := range []string{r.CodexHome(), root} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f := &fixture{t: t, now: testNow, home: home, root: root}
	f.Switcher = &Switcher{
		Provider: ProviderCodex,
		Paths:    r,
		Creds: credstore.NewForProvider(r, root,
			keychain.NewWithRunner(refusingKeychain{}, 0), ProviderCodex),
		Usage:    usagestore.New(r.CacheDir()),
		Settings: settings.Defaults(),
	}
	f.SetClock(func() time.Time { return f.now })
	return f
}

// codexLogin writes what a logged-in Codex install looks like.
func (f *fixture) codexLogin(email, accountID, plan string) {
	f.t.Helper()
	claims, err := json.Marshal(map[string]any{
		"email": email,
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": accountID,
			"chatgpt_plan_type":  plan,
			"chatgpt_user_id":    "user-1",
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	token := strings.Join([]string{enc([]byte(`{"alg":"none"}`)), enc(claims), ""}, ".")
	auth, err := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens":    map[string]any{"id_token": token, "access_token": "at", "refresh_token": "rt"},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.Paths.CodexAuthPath(), auth, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// Claude keeps the identity in a config beside the credential; Codex keeps it
// INSIDE the credential. A switcher addressing Codex that read Claude's config
// would report whoever is logged into the other tool.
func TestLiveIdentityFollowsTheProvider(t *testing.T) {
	f := codexFixture(t)
	f.codexLogin("work@example.com", "acct-9", "plus")

	identity, ok := f.LiveIdentity()
	if !ok {
		t.Fatal("the Codex login did not resolve")
	}
	if identity.Email != "work@example.com" {
		t.Errorf("email = %q", identity.Email)
	}
	if identity.OrganizationUUID != "acct-9" || identity.DisplayTag() != "plus" {
		t.Errorf("identity = %+v, want the ChatGPT account and plan", identity)
	}
}

// A Claude config sitting on the same machine must not answer for Codex.
func TestACodexSwitcherIgnoresTheClaudeConfig(t *testing.T) {
	f := codexFixture(t)
	if err := os.MkdirAll(f.Paths.ClaudeConfigHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Paths.GlobalConfigPath(),
		[]byte(`{"oauthAccount":{"emailAddress":"claude@example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if identity, ok := f.LiveIdentity(); ok {
		t.Errorf("LiveIdentity = %+v, want nothing — that is Claude's login", identity)
	}
}

// An API-key Codex install has no token to read. Reporting an identity from one
// would let every comparison against it succeed vacuously.
func TestACodexAPIKeyInstallHasNoIdentity(t *testing.T) {
	f := codexFixture(t)
	if err := os.WriteFile(f.Paths.CodexAuthPath(),
		[]byte(`{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if identity, ok := f.LiveIdentity(); ok {
		t.Errorf("LiveIdentity = %+v, want nothing", identity)
	}
}
