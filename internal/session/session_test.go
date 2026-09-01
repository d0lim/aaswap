package session

import (
	"context"
	json "encoding/json/v2"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/realiti4/claude-swap/internal/claudeapi"
	"github.com/realiti4/claude-swap/internal/credstore"
	"github.com/realiti4/claude-swap/internal/keychain"
	"github.com/realiti4/claude-swap/internal/paths"
	"github.com/realiti4/claude-swap/internal/platform"

	"github.com/realiti4/claude-swap/internal/testutil"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

var testNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// fakeProber answers the auth probe without a Claude Code binary.
type fakeProber struct {
	status  AuthStatus
	verdict Verdict
	calls   int
}

func (p *fakeProber) AuthStatus(string) (AuthStatus, Verdict) {
	p.calls++
	return p.status, p.verdict
}

type refusingKeychain struct{}

func (refusingKeychain) Run(ctx context.Context, args []string, stdin string) (keychain.Result, error) {
	return keychain.Result{}, os.ErrNotExist
}

type fixture struct {
	*Manager
	t     *testing.T
	home  string
	probe *fakeProber
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, ".local", "share", "claude-swap")
	resolver := paths.New(home, platform.Linux)
	if err := os.MkdirAll(resolver.ClaudeConfigHome(), 0o700); err != nil {
		t.Fatal(err)
	}

	probe := &fakeProber{status: AuthStatus{LoggedIn: true, AuthMethod: "claude.ai"}}
	return &fixture{
		t: t, home: home, probe: probe,
		Manager: &Manager{
			BackupRoot: root,
			Platform:   platform.Linux,
			Creds:      credstore.New(resolver, root, keychain.NewWithRunner(refusingKeychain{}, 0)),
			Probe:      probe,
			Now:        func() time.Time { return testNow },
		},
	}
}

// seedSlot gives a slot a stored credential and config.
func (f *fixture) seedSlot(num, email, credentials string) {
	f.t.Helper()
	if err := f.Creds.WriteAccount(num, email, credentials); err != nil {
		f.t.Fatal(err)
	}
	configs := filepath.Join(f.BackupRoot, "configs")
	if err := os.MkdirAll(configs, 0o700); err != nil {
		f.t.Fatal(err)
	}
	config := `{"oauthAccount":{"emailAddress":"` + email + `"},"theme":"light"}`
	path := filepath.Join(configs, ".claude-config-"+num+"-"+email+".json")
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

func TestSlugifyEmail(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"user@example.com", "user_example.com"},
		{"first.last+tag@example.com", "first.last_tag_example.com"},
		{"UPPER@Example.COM", "UPPER_Example.COM"},
		// Anything outside the safe set becomes an underscore, including
		// characters some filesystems forbid outright.
		{`a/b\c:d*e?f"g<h>i|j@x.com`, "a_b_c_d_e_f_g_h_i_j_x.com"},
		// Per RUNE, not per byte: one accented letter becomes one underscore.
		{"ünïcödé@example.com", "_n_c_d__example.com"},
	}
	for _, tt := range tests {
		if got := SlugifyEmail(tt.in); got != tt.want {
			t.Errorf("SlugifyEmail(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Uniqueness comes from the slot prefix, so the slug only has to be safe.
func TestTwoAddressesMayShareASlug(t *testing.T) {
	if SlugifyEmail("a/b@x.com") != SlugifyEmail("a\\b@x.com") {
		t.Skip("these do not collide, which is fine; the point is the prefix carries uniqueness")
	}
	one := DirFor("/root", "1", "a/b@x.com")
	two := DirFor("/root", "2", "a\\b@x.com")
	if one == two {
		t.Error("two slots produced the same profile directory")
	}
}

func TestStaleMarker(t *testing.T) {
	f := newFixture(t)
	dir := f.Dir("1", "a@example.com")

	if IsStale(dir) {
		t.Error("a fresh profile reads as stale")
	}
	if err := MarkStale(dir); err != nil {
		t.Fatal(err)
	}
	if !IsStale(dir) {
		t.Error("the marker did not take")
	}
	if !ClearStale(dir) {
		t.Error("ClearStale reported nothing to clear")
	}
	if IsStale(dir) {
		t.Error("the marker survived being cleared")
	}
}

// The marker is a SIBLING of the profile, so an invalidation that could not
// write into the profile — the very fault it exists for — can still record
// itself, and removing the profile does not take it.
func TestTheStaleMarkerIsASibling(t *testing.T) {
	dir := "/root/sessions/1-a_example.com"
	marker := StaleMarkerFor(dir)
	if filepath.Dir(marker) != filepath.Dir(dir) {
		t.Errorf("the marker at %q is not a sibling of %q", marker, dir)
	}
	if strings.HasPrefix(marker, dir+string(filepath.Separator)) {
		t.Errorf("the marker at %q is inside the profile", marker)
	}
}

func TestBootstrapSeedsAProfile(t *testing.T) {
	f := newFixture(t)
	const credentials = `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r"}}`
	f.seedSlot("1", "a@example.com", credentials)
	dir := f.Dir("1", "a@example.com")

	if err := f.Bootstrap(dir, "1", "a@example.com"); err != nil {
		t.Fatal(err)
	}

	if got := f.ReadCredentials(dir); got != credentials {
		t.Errorf("the seeded credential is %q", got)
	}
	// Owner-only: this is a live login sitting in a directory.
	for _, path := range []string{dir, filepath.Join(dir, ".credentials.json")} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0o600)
		if info.IsDir() {
			want = 0o700
		}
		testutil.AssertPermInfo(t, path, info, want)
	}

	config := readConfig(t, filepath.Join(dir, ".claude.json"))
	account := config["oauthAccount"].(map[string]any)
	if account["emailAddress"] != "a@example.com" {
		t.Errorf("oauthAccount = %v", account)
	}
	// Load-bearing: Claude Code shows onboarding without these, and a fresh
	// profile that walks the user through setup is not the session they asked
	// for.
	if config["hasCompletedOnboarding"] != true {
		t.Errorf("hasCompletedOnboarding = %v", config["hasCompletedOnboarding"])
	}
	if config["theme"] != "light" {
		t.Errorf("theme = %v, want the stored one", config["theme"])
	}
}

// Re-seeding must not throw away the profile's own accumulated state.
func TestBootstrapPreservesTheProfilesOwnState(t *testing.T) {
	f := newFixture(t)
	f.seedSlot("1", "a@example.com", `{"claudeAiOauth":{"accessToken":"a"}}`)
	dir := f.Dir("1", "a@example.com")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(
		`{"projects":{"/w":{}},"theme":"dark","oauthAccount":{"emailAddress":"old@example.com"}}`),
		0o600); err != nil {
		t.Fatal(err)
	}

	if err := f.Bootstrap(dir, "1", "a@example.com"); err != nil {
		t.Fatal(err)
	}

	config := readConfig(t, filepath.Join(dir, ".claude.json"))
	if _, present := config["projects"]; !present {
		t.Errorf("the profile's own projects were dropped: %v", config)
	}
	// The profile's existing theme wins over the stored one: it is the user's
	// choice inside this profile.
	if config["theme"] != "dark" {
		t.Errorf("theme = %v, want the profile's own", config["theme"])
	}
	// The identity is replaced, because that is what a re-seed is for.
	account := config["oauthAccount"].(map[string]any)
	if account["emailAddress"] != "a@example.com" {
		t.Errorf("oauthAccount = %v, want the slot's identity", account)
	}
}

func TestBootstrapRefusals(t *testing.T) {
	tests := []struct {
		name    string
		setup   func(*fixture)
		wantErr []string
	}{
		{
			name:    "no stored credential",
			setup:   func(f *fixture) {},
			wantErr: []string{"no stored credentials", "ccswap add --slot 1"},
		},
		{
			name: "no stored config",
			setup: func(f *fixture) {
				if err := f.Creds.WriteAccount("1", "a@example.com", `{"claudeAiOauth":{}}`); err != nil {
					f.t.Fatal(err)
				}
			},
			wantErr: []string{"no stored config backup"},
		},
		{
			name: "a stored config with no identity",
			setup: func(f *fixture) {
				if err := f.Creds.WriteAccount("1", "a@example.com", `{"claudeAiOauth":{}}`); err != nil {
					f.t.Fatal(err)
				}
				configs := filepath.Join(f.BackupRoot, "configs")
				if err := os.MkdirAll(configs, 0o700); err != nil {
					f.t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(configs, ".claude-config-1-a@example.com.json"),
					[]byte(`{"projects":{}}`), 0o600); err != nil {
					f.t.Fatal(err)
				}
			},
			wantErr: []string{"carries no account identity"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			tt.setup(f)
			err := f.Bootstrap(f.Dir("1", "a@example.com"), "1", "a@example.com")
			if err == nil {
				t.Fatal("Bootstrap accepted an unusable slot")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not mention %q: %v", want, err)
				}
			}
		})
	}
}

// An in-session login re-points the profile's credential at another account
// while the directory keeps claiming its slot.
func TestIdentityDrift(t *testing.T) {
	tests := []struct {
		name   string
		config string
		want   Identity
		drift  bool
	}{
		{
			name:   "the same account",
			config: `{"oauthAccount":{"emailAddress":"a@example.com"}}`,
			want:   Identity{Email: "a@example.com"},
		},
		{
			name:   "a different account",
			config: `{"oauthAccount":{"emailAddress":"b@example.com"}}`,
			want:   Identity{Email: "a@example.com"},
			drift:  true,
		},
		{
			name:   "the same address under another organization",
			config: `{"oauthAccount":{"emailAddress":"a@example.com","organizationUuid":"org-2"}}`,
			want:   Identity{Email: "a@example.com", OrganizationUUID: "org-1"},
			drift:  true,
		},
		{
			// Compared only when both sides state one, so a renamed field
			// degrades to an address check rather than producing false drift.
			name:   "an organization on only one side",
			config: `{"oauthAccount":{"emailAddress":"a@example.com"}}`,
			want:   Identity{Email: "a@example.com", OrganizationUUID: "org-1"},
		},
		{
			// Missing metadata degrades to trusting the profile — whose token
			// family is normally the slot's freshest — rather than abandoning
			// it over a broken file.
			name:   "an unreadable identity is not drift",
			config: `{oops`,
			want:   Identity{Email: "a@example.com"},
		},
		{
			name:   "no identity block at all",
			config: `{"projects":{}}`,
			want:   Identity{Email: "a@example.com"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			dir := f.Dir("1", "a@example.com")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(tt.config), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := f.IdentityDrifted(dir, tt.want); got != tt.drift {
				t.Errorf("IdentityDrifted = %v, want %v", got, tt.drift)
			}
		})
	}
}

// The four verdicts exist because "the profile is bad" and "the probe failed"
// have opposite consequences.
func TestValidity(t *testing.T) {
	want := Identity{Email: "a@example.com", OrganizationUUID: "org-1"}
	tests := []struct {
		name    string
		status  AuthStatus
		verdict Verdict
		want    Verdict
	}{
		{
			name:   "logged in as the right account",
			status: AuthStatus{LoggedIn: true, AuthMethod: "claude.ai", Email: "a@example.com", OrgID: "org-1"},
			want:   Valid,
		},
		{
			name:   "not logged in",
			status: AuthStatus{LoggedIn: false},
			want:   Invalid,
		},
		{
			// An environment API key reports a different method, and the probe
			// already drops those variables — so seeing one means the profile
			// itself is not an account login.
			name:   "authenticated some other way",
			status: AuthStatus{LoggedIn: true, AuthMethod: "api-key", Email: "a@example.com"},
			want:   Invalid,
		},
		{
			name:   "logged in as someone else",
			status: AuthStatus{LoggedIn: true, AuthMethod: "claude.ai", Email: "b@example.com"},
			want:   Invalid,
		},
		{
			name:   "a different organization",
			status: AuthStatus{LoggedIn: true, AuthMethod: "claude.ai", Email: "a@example.com", OrgID: "org-2"},
			want:   Invalid,
		},
		{
			// Lenient: a renamed field degrades to an address check.
			name:   "no organization reported",
			status: AuthStatus{LoggedIn: true, AuthMethod: "claude.ai", Email: "a@example.com"},
			want:   Valid,
		},
		{
			name: "the probe timed out", verdict: Unknown, want: Unknown,
		},
		{
			name: "the binary could not be run", verdict: Unreachable, want: Unreachable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			dir := f.Dir("1", "a@example.com")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			f.probe.status, f.probe.verdict = tt.status, tt.verdict

			if got := f.Validity(dir, want); got != tt.want {
				t.Errorf("Validity = %q, want %q", got, tt.want)
			}
		})
	}

	t.Run("a profile that does not exist", func(t *testing.T) {
		f := newFixture(t)
		if got := f.Validity(f.Dir("1", "a@example.com"), want); got != Invalid {
			t.Errorf("Validity = %q, want %q", got, Invalid)
		}
	})

	// No way to ask establishes nothing, which is Unknown — not Invalid, which
	// would license destroying the profile.
	t.Run("no prober at all", func(t *testing.T) {
		f := newFixture(t)
		f.Probe = nil
		dir := f.Dir("1", "a@example.com")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if got := f.Validity(dir, want); got != Unknown {
			t.Errorf("Validity = %q, want %q", got, Unknown)
		}
	})
}

// A probe that timed out is not by itself a reason to reuse — or to re-seed.
func TestUsableResolvesUnknownFromArtifacts(t *testing.T) {
	want := Identity{Email: "a@example.com"}

	setup := func(t *testing.T, verdict Verdict, credentials, config string) *fixture {
		f := newFixture(t)
		dir := f.Dir("1", "a@example.com")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if credentials != "" {
			if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(credentials), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if config != "" {
			if err := os.WriteFile(filepath.Join(dir, ".claude.json"), []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		f.probe.verdict = verdict
		return f
	}

	t.Run("credential present and identity intact", func(t *testing.T) {
		f := setup(t, Unknown, `{"claudeAiOauth":{}}`, `{"oauthAccount":{"emailAddress":"a@example.com"}}`)
		if !f.Usable(f.Dir("1", "a@example.com"), want) {
			t.Error("a timed-out probe on a healthy-looking profile refused reuse")
		}
	})

	t.Run("no credential material at all", func(t *testing.T) {
		f := setup(t, Unknown, "", `{"oauthAccount":{"emailAddress":"a@example.com"}}`)
		if f.Usable(f.Dir("1", "a@example.com"), want) {
			t.Error("a profile with no credential was reused")
		}
	})

	t.Run("the identity drifted", func(t *testing.T) {
		f := setup(t, Unknown, `{"claudeAiOauth":{}}`, `{"oauthAccount":{"emailAddress":"b@example.com"}}`)
		if f.Usable(f.Dir("1", "a@example.com"), want) {
			t.Error("a profile logged in as someone else was reused")
		}
	})

	// The question there is not whether the profile holds a credential but
	// whether Claude Code can be run at all, and no local file answers that.
	t.Run("unreachable ignores the artifacts", func(t *testing.T) {
		f := setup(t, Unreachable, `{"claudeAiOauth":{}}`, `{"oauthAccount":{"emailAddress":"a@example.com"}}`)
		if f.Usable(f.Dir("1", "a@example.com"), want) {
			t.Error("an unrunnable binary was reused on the strength of local files")
		}
	})
}

// A profile seeded before a refresh holds the predecessor, and Claude Code's
// own first refresh from it would be rejected.
func TestProfileMatchesBackup(t *testing.T) {
	f := newFixture(t)
	const generation1 = `{"claudeAiOauth":{"accessToken":"a1","refreshToken":"r1"}}`
	const generation2 = `{"claudeAiOauth":{"accessToken":"a2","refreshToken":"r2"}}`
	f.seedSlot("1", "a@example.com", generation1)
	dir := f.Dir("1", "a@example.com")
	if err := f.Bootstrap(dir, "1", "a@example.com"); err != nil {
		t.Fatal(err)
	}

	if !f.ProfileMatchesBackup(dir, "1", "a@example.com") {
		t.Error("a freshly seeded profile does not match its backup")
	}

	// The backup rotates; the profile still holds the predecessor.
	if err := f.Creds.WriteAccount("1", "a@example.com", generation2); err != nil {
		t.Fatal(err)
	}
	if f.ProfileMatchesBackup(dir, "1", "a@example.com") {
		t.Error("a profile on the predecessor matched a rotated backup")
	}

	// An access-token-only rotation is the SAME generation: the lineage did not
	// advance, so re-seeding would be pointless churn.
	if err := f.Creds.WriteAccount("1", "a@example.com",
		`{"claudeAiOauth":{"accessToken":"a1-rotated","refreshToken":"r1"}}`); err != nil {
		t.Fatal(err)
	}
	if !f.ProfileMatchesBackup(dir, "1", "a@example.com") {
		t.Error("an access-token rotation was read as a new generation")
	}
}

func TestHasRefreshToken(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"an OAuth credential", `{"claudeAiOauth":{"refreshToken":"r"}}`, true},
		{"a setup token with none", `{"claudeAiOauth":{"accessToken":"a"}}`, false},
		// An unrecognized shape lets the refresh attempt decide: a blob this
		// version cannot parse is not evidence there is no token in it.
		{"an unrecognized shape", `{"something":"else"}`, true},
		{"garbage", `not json`, true},
	}
	for _, tt := range tests {
		if got := HasRefreshToken(tt.in); got != tt.want {
			t.Errorf("%s: HasRefreshToken = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// Passing an auth override through would make `ccswap run 2` silently run as
// something else.
func TestEnvironmentDropsAuthOverrides(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=sk-ant-api03-abc",
		"CLAUDE_CODE_OAUTH_TOKEN=tok",
		"HOME=/home/u",
		"CLAUDE_CONFIG_DIR=/somewhere/else",
	}
	env, scrubbed := Environment(base, "/profiles/2-a_example.com")

	joined := strings.Join(env, "\n")
	for _, gone := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if strings.Contains(joined, gone) {
			t.Errorf("%s survived into the session environment", gone)
		}
	}
	for _, kept := range []string{"PATH=/usr/bin", "HOME=/home/u"} {
		if !strings.Contains(joined, kept) {
			t.Errorf("%s was dropped from the session environment", kept)
		}
	}
	// The profile is pointed at exactly once, whatever the caller's shell said.
	if strings.Count(joined, "CLAUDE_CONFIG_DIR=") != 1 {
		t.Errorf("CLAUDE_CONFIG_DIR appears %d times:\n%s",
			strings.Count(joined, "CLAUDE_CONFIG_DIR="), joined)
	}
	if !strings.Contains(joined, "CLAUDE_CONFIG_DIR=/profiles/2-a_example.com") {
		t.Errorf("the session was not pointed at its profile:\n%s", joined)
	}
	// The caller is told what was dropped rather than left to wonder.
	if len(scrubbed) != 2 {
		t.Errorf("scrubbed = %v, want both overrides named", scrubbed)
	}
}

// The lineage fingerprint is what identifies a generation, on both sides.
func TestAProfileHoldsTheNewestGeneration(t *testing.T) {
	f := newFixture(t)
	dir := f.Dir("1", "a@example.com")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const rotated = `{"claudeAiOauth":{"accessToken":"a9","refreshToken":"r9"}}`
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(rotated), 0o600); err != nil {
		t.Fatal(err)
	}

	got := f.ReadCredentials(dir)
	if claudeapi.Fingerprint(got) != claudeapi.Fingerprint(rotated) {
		t.Errorf("ReadCredentials returned a different generation: %q", got)
	}
}

func readConfig(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%s is not a JSON object: %v\n%s", path, err, data)
	}
	return out
}

// Profiles outlive the rename from cswap to ccswap, and every marker is named
// after the command. An old spelling that goes unseen is not a lost file but a
// behavior change: the stale marker stops deferring an invalidation, the share
// manifest stops naming the links ccswap may remove, and the mirror marker lets
// the one-time MCP migration run a second time against already-mirrored
// servers.
func TestMarkersWrittenUnderTheOldNameAreAdopted(t *testing.T) {
	for _, current := range []string{
		StaleMarkerSuffix, ShareManifest, MCPMirrorMarker, MCPDisplacedStash,
	} {
		t.Run(current, func(t *testing.T) {
			dir := t.TempDir()
			legacy := filepath.Join(dir, strings.Replace(current, ".ccswap-", ".cswap-", 1))
			if legacy == filepath.Join(dir, current) {
				t.Fatalf("%q does not carry the command name, so the test proves nothing", current)
			}
			if err := os.WriteFile(legacy, []byte(`{"kept":true}`), 0o600); err != nil {
				t.Fatal(err)
			}

			path := filepath.Join(dir, current)
			AdoptLegacyMarker(path)

			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("the marker was not adopted under its current name: %v", err)
			}
			if string(data) != `{"kept":true}` {
				t.Errorf("contents = %q, want the original bytes carried over", data)
			}
			if _, err := os.Lstat(legacy); err == nil {
				t.Error("the old spelling survived, so the profile carries both names")
			}
		})
	}
}

// Adoption must never overwrite a marker that already exists under the current
// name: the current one is what this ccswap wrote, and a leftover old file is
// by definition the older truth.
func TestAdoptionNeverOverwritesACurrentMarker(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ShareManifest)
	legacy := filepath.Join(dir, strings.Replace(ShareManifest, ".ccswap-", ".cswap-", 1))
	if err := os.WriteFile(path, []byte("current"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacy, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}

	AdoptLegacyMarker(path)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "current" {
		t.Errorf("contents = %q, want the current marker left alone", data)
	}
}
