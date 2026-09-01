package swap

import (
	"errors"
	"os"
	"testing"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/claudeapi"
)

func TestLiveIdentity(t *testing.T) {
	f := newFixture(t)
	f.setLiveIdentity("a@example.com", "org-1", "Example Inc", "acct-1")

	got, ok := f.LiveIdentity()
	if !ok {
		t.Fatal("LiveIdentity found no live login")
	}
	want := LiveIdentity{
		Email: "a@example.com", OrganizationUUID: "org-1",
		OrganizationName: "Example Inc", AccountUUID: "acct-1",
	}
	if got != want {
		t.Errorf("LiveIdentity = %+v, want %+v", got, want)
	}
	if got.Identity() != (Identity{Email: "a@example.com", OrganizationUUID: "org-1"}) {
		t.Errorf("Identity = %+v", got.Identity())
	}
	if got.DisplayTag() != "Example Inc" {
		t.Errorf("DisplayTag = %q", got.DisplayTag())
	}
}

func TestNoLiveIdentity(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{"no account block", `{"projects":{}}`},
		// An account with no address is not an identity, and treating it as one
		// would let every comparison against it succeed vacuously.
		{"no email address", `{"oauthAccount":{"organizationUuid":"org-1"}}`},
		{"an empty email address", `{"oauthAccount":{"emailAddress":""}}`},
		{"an account block that is not an object", `{"oauthAccount":"a@example.com"}`},
		{"malformed JSON", `{"oauthAccount":`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if err := os.WriteFile(f.Paths.GlobalConfigPath(), []byte(tt.config), 0o600); err != nil {
				t.Fatal(err)
			}
			if got, ok := f.LiveIdentity(); ok {
				t.Errorf("LiveIdentity = %+v, want none", got)
			}
			if f.HasLiveLogin() {
				t.Error("HasLiveLogin reported a login")
			}
		})
	}

	t.Run("no config file at all", func(t *testing.T) {
		f := newFixture(t)
		f.clearLiveIdentity()
		if _, ok := f.LiveIdentity(); ok {
			t.Error("LiveIdentity found a login with no config file")
		}
	})
}

// A personal account has no organization name; it still has to read as
// something.
func TestDisplayTagFallsBackToPersonal(t *testing.T) {
	if got := (LiveIdentity{}).DisplayTag(); got != "personal" {
		t.Errorf("DisplayTag = %q, want %q", got, "personal")
	}
	if got := (&Account{}).DisplayTag(); got != "personal" {
		t.Errorf("DisplayTag = %q, want %q", got, "personal")
	}
	if got := (*Account)(nil).DisplayTag(); got != "personal" {
		t.Errorf("DisplayTag on a nil record = %q, want %q", got, "personal")
	}
}

// Two managed slots may share an email across organizations. Matching on the
// address alone would let one account's transaction operate on another's
// credential.
func TestLiveIdentityMatchesComparesTheOrganization(t *testing.T) {
	f := newFixture(t)
	f.setLiveIdentity("a@example.com", "org-1", "Example", "acct-1")

	if !f.LiveIdentityMatches(Identity{Email: "a@example.com", OrganizationUUID: "org-1"}) {
		t.Error("the live identity did not match itself")
	}
	if f.LiveIdentityMatches(Identity{Email: "a@example.com"}) {
		t.Error("a personal identity matched an organization login with the same address")
	}
	if f.LiveIdentityMatches(Identity{Email: "b@example.com", OrganizationUUID: "org-1"}) {
		t.Error("a different address matched")
	}

	f.clearLiveIdentity()
	if f.LiveIdentityMatches(Identity{Email: "a@example.com", OrganizationUUID: "org-1"}) {
		t.Error("an absent live login matched an identity")
	}
}

// A /login landing between the ownership check and the write puts one account's
// identity on another's credential.
func TestRejectIdentityDrift(t *testing.T) {
	f := newFixture(t)
	f.setLiveIdentity("a@example.com", "org-1", "Example", "acct-1")
	verified, _ := f.LiveIdentity()

	if err := f.RejectIdentityDrift(verified); err != nil {
		t.Errorf("an unchanged identity was refused: %v", err)
	}

	tests := []struct {
		name  string
		drift func()
		names string
	}{
		{"another account logged in", func() {
			f.setLiveIdentity("b@example.com", "", "", "acct-2")
		}, "b@example.com"},
		{"the same address under another organization", func() {
			f.setLiveIdentity("a@example.com", "org-2", "Other", "acct-3")
		}, "a@example.com"},
		// Even a change confined to a field the identity composite ignores is
		// drift: the config was rewritten by something, and the bytes about to
		// be stored are no longer the ones that were verified.
		{"the account uuid moved", func() {
			f.setLiveIdentity("a@example.com", "org-1", "Example", "acct-rotated")
		}, "a@example.com"},
		{"the login went away", func() { f.clearLiveIdentity() }, "unknown"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.drift()
			err := f.RejectIdentityDrift(verified)
			wantErr(t, err, "changed while", "Nothing was changed", tt.names)
			if !errors.Is(err, apperr.ErrConfig) {
				t.Errorf("error is not a config error: %v", err)
			}
		})
	}
}

// The identity guard catches a /login. A plain refresh of the SAME account
// moves only the credential, and storing the pre-refresh bytes hands the slot a
// generation the server has already retired.
func TestRejectCredentialDrift(t *testing.T) {
	const before = `{"claudeAiOauth":{"accessToken":"a1","refreshToken":"r1"}}`

	tests := []struct {
		name    string
		current string
		wantErr bool
	}{
		{"nothing moved", before, false},
		// The fingerprint hashes the refresh token, so an access-token-only
		// rotation compares equal on purpose: the lineage did not advance.
		{"only the access token rotated",
			`{"claudeAiOauth":{"accessToken":"a2","refreshToken":"r1"}}`, false},
		{"the lineage advanced",
			`{"claudeAiOauth":{"accessToken":"a2","refreshToken":"r2"}}`, true},
		// Unreadable is unverifiable, not a refusal: this guard can only ever
		// ADD a refusal, and an unreadable store is the fail-open case.
		{"the store went empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if tt.current != "" {
				if err := f.Creds.WriteActive(tt.current); err != nil {
					t.Fatal(err)
				}
			}
			err := f.RejectCredentialDrift(before)
			if tt.wantErr {
				wantErr(t, err, "rotated while", "Nothing was changed")
			} else if err != nil {
				t.Errorf("unexpected refusal: %v", err)
			}
		})
	}
}

// A raw managed key captured as OAuth becomes a kindless account, which
// corrupts every path that keys off the kind.
func TestRejectLiveAPIKeyCapture(t *testing.T) {
	tests := []struct {
		name    string
		creds   string
		wantErr bool
	}{
		{"a managed API key", "sk-ant-api03-abcdef", true},
		{"one with surrounding whitespace", "  sk-ant-api03-abcdef\n", true},
		{"OAuth JSON", `{"claudeAiOauth":{"accessToken":"x"}}`, false},
		// A setup token is not an API key, whatever it starts with.
		{"a setup token", "sk-ant-oat01-abcdef", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := RejectLiveAPIKeyCapture(tt.creds)
			if tt.wantErr {
				wantErr(t, err, "API-key account", "add-token")
				if !errors.Is(err, apperr.ErrValidation) {
					t.Errorf("error is not a validation error: %v", err)
				}
			} else if err != nil {
				t.Errorf("unexpected refusal: %v", err)
			}
		})
	}
}

// Identity is the (email, organization) composite alone, so two slots sharing
// an email across kinds could not be told apart at switch time.
func TestRejectCrossKindCollision(t *testing.T) {
	f := newFixture(t)
	roster := newRoster(f.now)
	roster.Insert("1", &Account{Email: "oauth@example.com"}, f.now)
	roster.Insert("2", &Account{Email: "key@example.com", Kind: KindAPIKey}, f.now)
	// A slot under an organization does not collide with a personal token: the
	// composite differs.
	roster.Insert("3", &Account{Email: "org@example.com", OrganizationUUID: "org-3"}, f.now)

	tests := []struct {
		name     string
		email    string
		isAPIKey bool
		wantErr  bool
	}{
		{"a token colliding with an OAuth slot", "oauth@example.com", true, true},
		{"an OAuth capture colliding with a token slot", "key@example.com", false, true},
		{"the same kind is not a collision", "oauth@example.com", false, false},
		{"nor is a token onto a token", "key@example.com", true, false},
		{"an unused address", "new@example.com", true, false},
		{"an organization slot does not collide with a personal token", "org@example.com", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := f.RejectCrossKindCollision(roster, tt.email, tt.isAPIKey)
			if tt.wantErr {
				wantErr(t, err, tt.email, "distinct --email")
			} else if err != nil {
				t.Errorf("unexpected refusal: %v", err)
			}
		})
	}
}

func TestResolveIdentifier(t *testing.T) {
	f := newFixture(t)
	roster := newRoster(f.now)
	roster.Insert("1", &Account{Email: "a@example.com", Alias: "work"}, f.now)
	roster.Insert("2", &Account{Email: "b@example.com"}, f.now)
	roster.Insert("3", &Account{Email: "shared@example.com", OrganizationUUID: "org-3",
		OrganizationName: "Three"}, f.now)
	roster.Insert("4", &Account{Email: "shared@example.com", OrganizationUUID: "org-4",
		OrganizationName: "Four"}, f.now)

	tests := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{"a slot number", "2", "2", true},
		// Taken literally even when no such slot exists, so "switch 9" reports
		// "no account 9" rather than hunting for an alias named "9".
		{"a number with no slot", "9", "9", true},
		{"an alias", "work", "1", true},
		{"an alias, case-insensitively", "WORK", "1", true},
		{"an email", "b@example.com", "2", true},
		{"nothing that matches", "nobody@example.com", "", false},
		// An aliasless account must not be matched by the empty string.
		{"the empty string", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok, err := f.ResolveIdentifier(roster, tt.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("ResolveIdentifier(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
			}
		})
	}

	// The same address across two organizations is two accounts with two
	// quotas. Picking one would silently switch the user to an account they did
	// not name.
	t.Run("an ambiguous email is an error, not a guess", func(t *testing.T) {
		_, _, err := f.ResolveIdentifier(roster, "shared@example.com")
		wantErr(t, err, "ambiguous", "3 [Three]", "4 [Four]", "account number")
	})
}

// Number beats alias beats email, so a numeric-looking alias can never shadow a
// slot and an address can never shadow an alias.
func TestResolutionPrecedence(t *testing.T) {
	f := newFixture(t)
	roster := newRoster(f.now)
	// Slot 2's alias is the address of slot 1.
	roster.Insert("1", &Account{Email: "a@example.com"}, f.now)
	roster.Insert("2", &Account{Email: "b@example.com", Alias: "a@example.com"}, f.now)

	// The alias wins over the email, because aliases resolve first.
	got, _, err := f.ResolveIdentifier(roster, "a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if got != "2" {
		t.Errorf("ResolveIdentifier = %q, want the alias match", got)
	}
}

func TestAliasInUse(t *testing.T) {
	f := newFixture(t)
	roster := newRoster(f.now)
	roster.Insert("1", &Account{Email: "a@example.com", Alias: "work"}, f.now)

	if num, inUse := AliasInUse(roster, "work", ""); !inUse || num != "1" {
		t.Errorf("AliasInUse = (%q, %v), want slot 1", num, inUse)
	}
	// A slot does not conflict with itself.
	if _, inUse := AliasInUse(roster, "work", "1"); inUse {
		t.Error("a slot conflicted with its own alias")
	}
	if _, inUse := AliasInUse(roster, "home", ""); inUse {
		t.Error("an unused alias reported as in use")
	}
}

// The fingerprint is what binds a strike to a generation, so identity code and
// the store must agree on what it means.
func TestCredentialFingerprintIsLineageIdentity(t *testing.T) {
	const gen1 = `{"claudeAiOauth":{"accessToken":"a1","refreshToken":"r1"}}`
	const gen1Rotated = `{"claudeAiOauth":{"accessToken":"a2","refreshToken":"r1"}}`
	const gen2 = `{"claudeAiOauth":{"accessToken":"a2","refreshToken":"r2"}}`

	if claudeapi.Fingerprint(gen1) != claudeapi.Fingerprint(gen1Rotated) {
		t.Error("an access-token rotation moved the lineage fingerprint")
	}
	if claudeapi.Fingerprint(gen1) == claudeapi.Fingerprint(gen2) {
		t.Error("a refresh-token rotation did not move the fingerprint")
	}
}
