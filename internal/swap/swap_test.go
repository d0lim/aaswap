package swap

import (
	"context"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/d0lim/ccswap/internal/credstore"
	"github.com/d0lim/ccswap/internal/keychain"
	"github.com/d0lim/ccswap/internal/paths"
	"github.com/d0lim/ccswap/internal/platform"
	"github.com/d0lim/ccswap/internal/settings"
	"github.com/d0lim/ccswap/internal/usagestore"
)

var testNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// refusingKeychain fails every operation, which is what a non-macOS host looks
// like. The default for swap's tests: credentials then live in files, where a
// test can inspect and corrupt them directly.
type refusingKeychain struct{}

func (refusingKeychain) Run(context.Context, []string, string) (keychain.Result, error) {
	return keychain.Result{}, os.ErrNotExist
}

// fixture is a Switcher over a temp home and a temp backup root.
type fixture struct {
	*Switcher
	t    *testing.T
	now  time.Time
	home string
	root string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, ".local", "share", paths.BackupDirName)

	// Linux, so the credential store is files. macOS Keychain behavior is
	// credstore's own subject; here it would only add a fake to see through.
	r := paths.New(home, platform.Linux)
	if err := os.MkdirAll(r.ClaudeConfigHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	f := &fixture{t: t, now: testNow, home: home, root: root}
	f.Switcher = &Switcher{
		// Collapse the request stagger: the STAGGERING is the behavior under
		// test, not the wall clock it costs.
		FetchStagger: time.Millisecond,
		Paths:        r,
		Creds:        credstore.New(r, root, keychain.NewWithRunner(refusingKeychain{}, 0)),
		Usage:        usagestore.New(r.CacheDir()),
		Settings:     settings.Defaults(),
	}
	// One clock for the Switcher and its usage store: the store decides
	// freshness and leases against its own now, and two clocks would make a
	// measurement fresh to one half of a collect pass and stale to the other.
	f.SetClock(func() time.Time { return f.now })
	return f
}

func (f *fixture) advance(d time.Duration) { f.now = f.now.Add(d) }

// seedRoster writes a roster directly, bypassing the add path.
func (f *fixture) seedRoster(roster *Roster) {
	f.t.Helper()
	if err := f.WriteRoster(roster); err != nil {
		f.t.Fatal(err)
	}
}

// account builds a roster holding the given slots, each with a credential and a
// config backup so it is switchable.
func (f *fixture) seedAccounts(accounts map[string]*Account) *Roster {
	f.t.Helper()
	roster := newRoster(f.now)
	for num, account := range accounts {
		roster.Insert(num, account, f.now)
		if err := f.Creds.WriteAccount(num, account.Email, `{"claudeAiOauth":{"accessToken":"tok-`+num+`"}}`); err != nil {
			f.t.Fatal(err)
		}
		if err := f.WriteAccountConfig(num, account.Email, `{"oauthAccount":{"emailAddress":"`+account.Email+`"}}`); err != nil {
			f.t.Fatal(err)
		}
	}
	f.seedRoster(roster)
	return roster
}

// setLiveIdentity writes the live Claude Code config.
func (f *fixture) setLiveIdentity(email, orgUUID, orgName, accountUUID string) {
	f.t.Helper()
	config := map[string]any{
		"oauthAccount": map[string]any{
			"emailAddress":     email,
			"organizationUuid": orgUUID,
			"organizationName": orgName,
			"accountUuid":      accountUUID,
		},
		// A key ccswap does not own, present in every real config.
		"projects": map[string]any{"/home/u/work": map[string]any{"allowedTools": []string{}}},
	}
	data, err := json.Marshal(config)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(f.Paths.GlobalConfigPath(), data, 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// clearLiveIdentity removes the live config entirely.
func (f *fixture) clearLiveIdentity() {
	f.t.Helper()
	if err := os.Remove(f.Paths.GlobalConfigPath()); err != nil && !os.IsNotExist(err) {
		f.t.Fatal(err)
	}
}

func (f *fixture) roster() *Roster {
	f.t.Helper()
	roster, err := f.RosterOrEmpty()
	if err != nil {
		f.t.Fatal(err)
	}
	return roster
}

// rawRoster reads sequence.json as an opaque object, so a test can assert on
// the on-disk shape rather than only on the model.
func (f *fixture) rawRoster() map[string]any {
	f.t.Helper()
	data, err := os.ReadFile(f.RosterPath())
	if err != nil {
		f.t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		f.t.Fatalf("sequence.json is not a JSON object: %v\n%s", err, data)
	}
	return out
}

// wantErr fails unless err mentions every fragment.
func wantErr(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an error mentioning %v", fragments)
	}
	for _, fragment := range fragments {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error does not mention %q: %v", fragment, err)
		}
	}
}

// Every path that replaces a slot's stored credential has to announce it, or a
// session profile keeps serving the generation the write just superseded — and
// the local reuse check cannot tell, because it asks whether a credential is
// well-formed, not whether the server still honours it.
func TestEveryCredentialWriteAnnouncesItself(t *testing.T) {
	tests := []struct {
		name string
		act  func(f *fixture)
		want string
	}{
		{
			name: "add-token registers a new slot",
			act: func(f *fixture) {
				if _, err := f.AddToken(AddTokenRequest{
					Token: "sk-ant-api03-aaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					Email: "token@example.com",
				}); err != nil {
					f.t.Fatal(err)
				}
			},
			want: "1",
		},
		{
			// Only when the credential actually CHANGED. A switch that finds
			// the live bytes untouched since ccswap wrote them backs up the
			// config alone, and a profile seeded from those same bytes is not
			// stale — announcing there would invalidate profiles on every
			// ordinary switch.
			name: "a switch backs up an outgoing account whose token rotated",
			act: func(f *fixture) {
				f.twoAccounts()
				if err := f.Creds.WriteActive(
					`{"claudeAiOauth":{"accessToken":"rotated-under-us"}}`); err != nil {
					f.t.Fatal(err)
				}
				if _, err := f.Switch(f.t.Context(), SwitchRequest{Target: "2"}); err != nil {
					f.t.Fatal(err)
				}
			},
			want: "1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			var announced []string
			f.OnBackupWritten = func(num, _ string) { announced = append(announced, num) }

			tt.act(f)

			if !slices.Contains(announced, tt.want) {
				t.Errorf("announced %v, want it to include account %s", announced, tt.want)
			}
		})
	}
}

// The other half of the same rule: an unchanged credential must NOT announce,
// or every ordinary switch would tear down a session profile that is perfectly
// current.
func TestAnUnchangedCredentialAnnouncesNothing(t *testing.T) {
	f := newFixture(t)
	var announced []string
	f.OnBackupWritten = func(num, _ string) { announced = append(announced, num) }
	f.twoAccounts()

	if _, err := f.Switch(t.Context(), SwitchRequest{Target: "2"}); err != nil {
		t.Fatal(err)
	}
	if len(announced) != 0 {
		t.Errorf("announced %v for a switch that changed no credential", announced)
	}
}

// The hook is optional. A Switcher built without one must not panic on a write:
// most callers have no session profiles to care about.
func TestACredentialWriteWorksWithNoListener(t *testing.T) {
	f := newFixture(t)
	f.OnBackupWritten = nil
	f.twoAccounts()
	if _, err := f.Switch(t.Context(), SwitchRequest{Target: "2"}); err != nil {
		t.Fatal(err)
	}
}
