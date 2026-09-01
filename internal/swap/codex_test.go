package swap

import (
	"encoding/base64"
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/claudeapi"
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
			keychain.NewWithRunner(refusingKeychain{}, 0), ProviderCodex,
			LiveLayout(r, ProviderCodex)),
		Usage:    usagestore.NewForProvider(r.CacheDir(), ProviderCodex),
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

// writeCodexRollout writes a session rollout in the shape Codex writes one.
func (f *fixture) writeCodexRollout(usedPct float64) {
	f.t.Helper()
	dir := filepath.Join(f.Paths.CodexHome(), "sessions", "2026", "08", "18")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	line := fmt.Sprintf(`{"timestamp":"2026-08-18T13:06:00Z","type":"token_count","payload":`+
		`{"rate_limits":{"plan_type":"plus","primary":`+
		`{"used_percent":%v,"window_minutes":10080,"resets_at":1787617468}}}}`, usedPct)
	if err := os.WriteFile(filepath.Join(dir, "rollout-a.jsonl"), []byte(line+"\n"), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// Codex quota comes from what Codex already recorded, attributed to whoever is
// live now — the rollout does not say which account it belonged to.
func TestTheCodexFetcherReportsTheLiveAccountsRecordedQuota(t *testing.T) {
	f := codexFixture(t)
	f.codexLogin("work@example.com", "acct-9", "plus")
	f.writeCodexRollout(73)

	roster := newRoster()
	roster.Insert("work", &Account{Email: "work@example.com", OrganizationUUID: "acct-9"})
	roster.Insert("spare", &Account{Email: "other@example.com"})
	f.seedRoster(roster)

	fetcher := f.liveOnlyUsageFetcher()

	live := fetcher.FetchUsageForAccount(t.Context(), claudeapi.FetchRequest{AccountNum: "work"})
	if live.Usage == nil || live.Usage.SevenDay == nil || live.Usage.SevenDay.Pct != 73 {
		t.Fatalf("the live account reports %+v, want the recorded 73%%", live.Usage)
	}

	// An idle account gets NOTHING rather than the live one's numbers. Handing
	// it those would send an auto-switch onto an account it has never measured,
	// believing it had the other's headroom.
	idle := fetcher.FetchUsageForAccount(t.Context(), claudeapi.FetchRequest{AccountNum: "spare"})
	if idle.Usage != nil {
		t.Errorf("an idle account reports %+v, want no measurement", idle.Usage)
	}
	// And no measurement must not read as an error either: unknown is unknown.
	if !idle.OK() {
		t.Errorf("an unmeasured account reported an error: %+v", idle)
	}
}

// With nothing logged in there is no account to attribute a record to.
func TestTheCodexFetcherAttributesNothingWithNoLiveLogin(t *testing.T) {
	f := codexFixture(t)
	f.writeCodexRollout(73)
	roster := newRoster()
	roster.Insert("work", &Account{Email: "work@example.com"})
	f.seedRoster(roster)

	got := f.liveOnlyUsageFetcher().FetchUsageForAccount(t.Context(),
		claudeapi.FetchRequest{AccountNum: "work"})
	if got.Usage != nil {
		t.Errorf("attributed %+v with nobody logged in", got.Usage)
	}
}

// A stored credential is not a Claude credential.
//
// The static sentinel used to decide "has usable credentials" by parsing a
// Claude OAuth token out of the stored blob. A Codex credential has no such
// field, so a perfectly good login reported "no credentials" — which sends a
// person to re-add an account that is already there.
func TestACodexAccountWithACredentialIsNotReportedEmpty(t *testing.T) {
	f := codexFixture(t)
	f.codexLogin("one@example.com", "acct-1", "plus")
	if _, err := f.Add(t.Context(), AddRequest{}); err != nil {
		t.Fatal(err)
	}

	snapshot, err := f.TakeSnapshot(t.Context(), CollectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Views) != 1 {
		t.Fatalf("views = %d, want the one account", len(snapshot.Views))
	}
	view := snapshot.Views[0]
	if got := f.staticSentinel(view); got == SentinelNoCredentials {
		t.Errorf("the account reads as having no credential, but %d bytes are "+
			"stored for it", len(view.Credentials))
	}
}
