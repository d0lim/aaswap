package swap

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/usagestore"
)

// fakeOracle answers the ownership question without a network.
type fakeOracle struct {
	calls    int
	identity *claudeapi.Identity
}

func (f *fakeOracle) Profile(context.Context, string) *claudeapi.Identity {
	f.calls++
	return f.identity
}

// resolving points the fixture's oracle at an identity that agrees with the
// live login, which is the ordinary case.
func (f *fixture) resolving(uuid, email, org string) *fakeOracle {
	oracle := &fakeOracle{identity: &claudeapi.Identity{
		UUID: uuid, Email: email, OrganizationUUID: org,
	}}
	f.Oracle = oracle
	return oracle
}

// liveLogin sets both halves of a live login: the identity in the config and
// the credential in the store.
func (f *fixture) liveLogin(email, orgUUID, orgName, accountUUID, credentials string) {
	f.t.Helper()
	f.setLiveIdentity(email, orgUUID, orgName, accountUUID)
	if err := f.Creds.WriteActive(credentials); err != nil {
		f.t.Fatal(err)
	}
}

const liveCreds = `{"claudeAiOauth":{"accessToken":"live-token","refreshToken":"r1"}}`

func TestAddRegistersTheLiveLogin(t *testing.T) {
	f := newFixture(t)
	f.liveLogin("a@example.com", "org-1", "Example", "acct-1", liveCreds)
	f.resolving("acct-1", "a@example.com", "org-1")

	got, err := f.Add(t.Context(), AddRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "a" || got.Email != "a@example.com" || got.Tag != "Example" {
		t.Errorf("outcome = %+v", got)
	}
	if got.Unverified != "" {
		t.Errorf("Unverified = %q, want the check to have completed", got.Unverified)
	}

	roster := f.roster()
	account := roster.Accounts["a"]
	if account.Email != "a@example.com" || account.UUID != "acct-1" ||
		account.OrganizationUUID != "org-1" || account.OrganizationName != "Example" {
		t.Errorf("account = %+v", account)
	}
	if account.Added != Timestamp(f.now) {
		t.Errorf("added = %q", account.Added)
	}
	if num, ok := roster.ActiveName(); !ok || num != "a" {
		t.Errorf("Active = (%q, %v), want the account just added", num, ok)
	}

	// The credential and the config both landed, or the slot is not switchable.
	if value, _ := f.Creds.ReadAccount("a", "a@example.com"); value != liveCreds {
		t.Errorf("stored credential = %q", value)
	}
	if config := f.ReadAccountConfig("a", "a@example.com"); !strings.Contains(config, "projects") {
		t.Errorf("stored config = %q, want the live config verbatim", config)
	}
}

// The stored config is the user's own file. A slot must restore exactly what
// was captured, not a re-serialized approximation of it.
func TestTheCapturedConfigIsVerbatim(t *testing.T) {
	f := newFixture(t)
	const exact = "{\n  \"oauthAccount\": {\"emailAddress\": \"a@example.com\"},\n  \"projects\": {\"/w\": {}},\n  \"userID\": \"u-1\"\n}\n"
	if err := os.WriteFile(f.Paths.GlobalConfigPath(), []byte(exact), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.Creds.WriteActive(liveCreds); err != nil {
		t.Fatal(err)
	}
	f.resolving("", "a@example.com", "")

	if _, err := f.Add(t.Context(), AddRequest{}); err != nil {
		t.Fatal(err)
	}
	if got := f.ReadAccountConfig("a", "a@example.com"); got != exact {
		t.Errorf("stored config =\n%q\nwant\n%q", got, exact)
	}
}

// Re-running add on a registered account refreshes its credential in place.
// Inventing a second name would leave two entries holding one account, the
// older one with a credential the server has retired.
func TestAddRefreshesARegisteredAccountInPlace(t *testing.T) {
	f := newFixture(t)
	f.liveLogin("a@example.com", "org-1", "Example", "acct-1", liveCreds)
	f.resolving("acct-1", "a@example.com", "org-1")
	if _, err := f.Add(t.Context(), AddRequest{}); err != nil {
		t.Fatal(err)
	}

	const rotated = `{"claudeAiOauth":{"accessToken":"live-2","refreshToken":"r2"}}`
	if err := f.Creds.WriteActive(rotated); err != nil {
		t.Fatal(err)
	}
	f.advance(time.Hour)

	got, err := f.Add(t.Context(), AddRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Refreshed || got.Name != "a" {
		t.Errorf("outcome = %+v, want an in-place refresh of \"a\"", got)
	}

	roster := f.roster()
	if len(roster.Accounts) != 1 {
		t.Errorf("accounts = %d, want the account to stay under one name", len(roster.Accounts))
	}
	// The name survives an add that named none.
	if _, ok := roster.Accounts["a"]; !ok {
		t.Errorf("accounts = %v, want the name preserved", roster.Names())
	}
	if value, _ := f.Creds.ReadAccount("a", "a@example.com"); value != rotated {
		t.Errorf("stored credential = %q, want the rotated one", value)
	}
}

// Two entries sharing an address across organizations are two accounts, and
// they cannot share a name.
func TestAddDistinguishesOrganizations(t *testing.T) {
	f := newFixture(t)
	f.liveLogin("a@example.com", "", "", "acct-personal", liveCreds)
	f.resolving("acct-personal", "a@example.com", "")
	if _, err := f.Add(t.Context(), AddRequest{}); err != nil {
		t.Fatal(err)
	}

	f.liveLogin("a@example.com", "org-2", "Two", "acct-org", liveCreds)
	f.resolving("acct-org", "a@example.com", "org-2")
	got, err := f.Add(t.Context(), AddRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "a-2" || got.Refreshed {
		t.Errorf("outcome = %+v, want a suffixed name for the organization account", got)
	}
	if len(f.roster().Accounts) != 2 {
		t.Error("the two accounts collapsed into one")
	}
}

func TestAddUnderAGivenName(t *testing.T) {
	f := newFixture(t)
	f.liveLogin("a@example.com", "", "", "acct-1", liveCreds)
	f.resolving("acct-1", "a@example.com", "")

	got, err := f.Add(t.Context(), AddRequest{Name: "Work"})
	if err != nil {
		t.Fatal(err)
	}
	// Normalized on the way in, so the handle a person types and the key the
	// store files it under cannot differ by case.
	if got.Name != "work" {
		t.Errorf("name = %q, want the given name normalized", got.Name)
	}
	if _, ok := f.roster().Accounts["work"]; !ok {
		t.Errorf("accounts = %v, want one called \"work\"", f.roster().Names())
	}
}

// A name is a path component and a Keychain account, so the rules that refuse
// one have to refuse it before anything is stored, not after.
func TestAddRejectsAnUnusableName(t *testing.T) {
	f := newFixture(t)
	f.liveLogin("a@example.com", "", "", "acct-1", liveCreds)
	f.resolving("acct-1", "a@example.com", "")

	for _, name := range []string{"..", ".", "a/b", "-x", "7"} {
		_, err := f.Add(t.Context(), AddRequest{Name: name})
		if err == nil {
			t.Errorf("Add(%q) was accepted", name)
			continue
		}
		if len(f.roster().Accounts) != 0 {
			t.Fatalf("Add(%q) stored something before refusing", name)
		}
	}
}

// Taking a name someone else holds needs a decision, and a caller that cannot
// ask gets a refusal rather than a silent overwrite.
func TestAddConfirmsBeforeTakingAHeldName(t *testing.T) {
	setup := func(t *testing.T) *fixture {
		f := newFixture(t)
		f.liveLogin("first@example.com", "", "", "acct-1", liveCreds)
		f.resolving("acct-1", "first@example.com", "")
		if _, err := f.Add(t.Context(), AddRequest{Name: "shared"}); err != nil {
			t.Fatal(err)
		}
		f.liveLogin("second@example.com", "", "", "acct-2", liveCreds)
		f.resolving("acct-2", "second@example.com", "")
		return f
	}

	t.Run("declined leaves everything alone", func(t *testing.T) {
		f := setup(t)
		got, err := f.Add(t.Context(), AddRequest{Name: "shared", Confirm: func(string) bool { return false }})
		if err != nil {
			t.Fatal(err)
		}
		if !got.Cancelled {
			t.Errorf("outcome = %+v, want a cancellation", got)
		}
		if f.roster().Accounts["shared"].Email != "first@example.com" {
			t.Error("the declined add took the name anyway")
		}
	})

	t.Run("no way to ask is a refusal", func(t *testing.T) {
		f := setup(t)
		got, err := f.Add(t.Context(), AddRequest{Name: "shared"})
		if err != nil {
			t.Fatal(err)
		}
		if !got.Cancelled {
			t.Errorf("outcome = %+v, want a cancellation", got)
		}
		if f.roster().Accounts["shared"].Email != "first@example.com" {
			t.Error("a name was taken with nobody to confirm it")
		}
	})

	t.Run("accepted replaces the occupant and its stored material", func(t *testing.T) {
		f := setup(t)
		got, err := f.Add(t.Context(), AddRequest{Name: "shared", Confirm: func(string) bool { return true }})
		if err != nil {
			t.Fatal(err)
		}
		if got.Displaced != "shared" || got.Name != "shared" {
			t.Errorf("outcome = %+v", got)
		}
		if f.roster().Accounts["shared"].Email != "second@example.com" {
			t.Error("the slot still names the old occupant")
		}
		// The displaced account's material is gone, not orphaned.
		if value, _ := f.Creds.ReadAccount("1", "first@example.com"); value != "" {
			t.Error("the displaced account's credential survived")
		}
		if config := f.ReadAccountConfig("1", "first@example.com"); config != "" {
			t.Error("the displaced account's config survived")
		}
	})

	t.Run("assume-yes skips the question", func(t *testing.T) {
		f := setup(t)
		got, err := f.Add(t.Context(), AddRequest{Name: "shared", AssumeYes: true})
		if err != nil {
			t.Fatal(err)
		}
		if got.Cancelled || got.Displaced != "shared" {
			t.Errorf("outcome = %+v", got)
		}
	})
}

// Pinning a registered account to a different slot moves it, rather than
// leaving a stale copy behind.
func TestAddIntoANewSlotMovesTheAccount(t *testing.T) {
	f := newFixture(t)
	f.liveLogin("a@example.com", "", "", "acct-1", liveCreds)
	f.resolving("acct-1", "a@example.com", "")
	if _, err := f.Add(t.Context(), AddRequest{}); err != nil {
		t.Fatal(err)
	}

	got, err := f.Add(t.Context(), AddRequest{Name: "spare"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "spare" || got.RenamedFrom != "a" {
		t.Errorf("outcome = %+v, want a rename from \"a\" to \"spare\"", got)
	}

	roster := f.roster()
	if _, still := roster.Accounts["a"]; still {
		t.Error("the old name survived the rename")
	}
	if value, _ := f.Creds.ReadAccount("a", "a@example.com"); value != "" {
		t.Error("the old name's credential was left behind")
	}
	if value, _ := f.Creds.ReadAccount("spare", "a@example.com"); value == "" {
		t.Error("the new slot has no credential")
	}
}

// Nothing destructive may run before the replacement is in memory and verified.
func TestAFailedAddChangesNothing(t *testing.T) {
	tests := []struct {
		name    string
		break_  func(*fixture)
		wantErr []string
	}{
		{
			name:    "no live login at all",
			break_:  func(f *fixture) { f.clearLiveIdentity() },
			wantErr: []string{"no active Claude account", "Log in first"},
		},
		{
			name:    "no credential to capture",
			break_:  func(f *fixture) { _ = os.Remove(f.Paths.CredentialsPath()) },
			wantErr: []string{"no credentials found"},
		},
		{
			// The slot would be labelled one account and contain another.
			name:    "the credential belongs to another account",
			break_:  func(f *fixture) { f.resolving("acct-someone-else", "other@example.com", "") },
			wantErr: []string{"does not belong to", "Nothing was changed"},
		},
		{
			name: "the credential belongs to another organization",
			break_: func(f *fixture) {
				f.setLiveIdentity("a@example.com", "org-mine", "Mine", "")
				f.resolving("", "a@example.com", "org-theirs")
			},
			wantErr: []string{"belongs to organization", "org-theirs", "org-mine"},
		},
		{
			name: "the live login is a managed API key",
			break_: func(f *fixture) {
				if err := f.Creds.WriteActive("sk-ant-api03-abcdef"); err != nil {
					f.t.Fatal(err)
				}
			},
			wantErr: []string{"API-key account", "add-token"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			// A registered account already holding the name, so a
			// destructive step would be visible.
			f.liveLogin("resident@example.com", "", "", "acct-resident", liveCreds)
			f.resolving("acct-resident", "resident@example.com", "")
			if _, err := f.Add(t.Context(), AddRequest{Name: "held"}); err != nil {
				t.Fatal(err)
			}
			before := f.roster()
			beforeCreds, _ := f.Creds.ReadAccount("held", "resident@example.com")

			f.liveLogin("a@example.com", "", "", "acct-1", liveCreds)
			f.resolving("acct-1", "a@example.com", "")
			tt.break_(f)

			_, err := f.Add(t.Context(), AddRequest{Name: "held", AssumeYes: true})
			wantErr(t, err, tt.wantErr...)

			// The resident account is untouched.
			after := f.roster()
			resident, ok := after.Accounts["held"]
			if !ok || resident.Email != "resident@example.com" {
				t.Errorf("the roster changed: %v (was %v)", after.Accounts, before.Accounts)
			}
			if value, _ := f.Creds.ReadAccount("held", "resident@example.com"); value != beforeCreds {
				t.Error("the resident's credential was disturbed by a failed add")
			}
		})
	}
}

// Unresolvable is a notice, not a refusal: the check must not block a
// registration that worked before it existed.
func TestAnUnverifiableCaptureProceedsWithANotice(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fixture)
		want  string
	}{
		{
			name:  "the lookup did not resolve",
			setup: func(f *fixture) { f.Oracle = &fakeOracle{} },
			want:  "did not resolve",
		},
		{
			name:  "there is no oracle at all",
			setup: func(f *fixture) { f.Oracle = nil },
			want:  "no identity oracle",
		},
		{
			name: "the access token is expired",
			setup: func(f *fixture) {
				expired := `{"claudeAiOauth":{"accessToken":"t","refreshToken":"r","expiresAt":1}}`
				if err := f.Creds.WriteActive(expired); err != nil {
					f.t.Fatal(err)
				}
			},
			want: "expired",
		},
		{
			name: "the credential carries no access token",
			setup: func(f *fixture) {
				if err := f.Creds.WriteActive(`{"claudeAiOauth":{"refreshToken":"r"}}`); err != nil {
					f.t.Fatal(err)
				}
			},
			want: "no access token",
		},
		{
			name: "the resolved identity carries no address",
			setup: func(f *fixture) {
				f.setLiveIdentity("a@example.com", "", "", "")
				f.Oracle = &fakeOracle{identity: &claudeapi.Identity{UUID: "acct-1"}}
			},
			want: "no address",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.liveLogin("a@example.com", "", "", "acct-1", liveCreds)
			f.resolving("acct-1", "a@example.com", "")
			tt.setup(f)

			got, err := f.Add(t.Context(), AddRequest{})
			if err != nil {
				t.Fatalf("an unresolvable check refused the capture: %v", err)
			}
			if !strings.Contains(got.Unverified, tt.want) {
				t.Errorf("Unverified = %q, want it to mention %q", got.Unverified, tt.want)
			}
			if _, ok := f.roster().Accounts[got.Name]; !ok {
				t.Error("the account was not registered")
			}
		})
	}
}

// A response with no organization block is indistinguishable from a personal
// account: unverifiable about the organization alone, never condemning.
func TestAnAbsentResolvedOrganizationDoesNotRefuse(t *testing.T) {
	f := newFixture(t)
	f.liveLogin("a@example.com", "org-1", "Example", "acct-1", liveCreds)
	f.Oracle = &fakeOracle{identity: &claudeapi.Identity{UUID: "acct-1", Email: "a@example.com"}}

	if _, err := f.Add(t.Context(), AddRequest{}); err != nil {
		t.Errorf("an absent resolved organization refused the capture: %v", err)
	}
}

// A uuid match under a disagreeing organization is another account: the uuid
// arm must fall through to the organization check, not return early.
func TestAUUIDMatchStillChecksTheOrganization(t *testing.T) {
	f := newFixture(t)
	f.liveLogin("a@example.com", "org-mine", "Mine", "acct-1", liveCreds)
	f.Oracle = &fakeOracle{identity: &claudeapi.Identity{
		UUID: "acct-1", Email: "a@example.com", OrganizationUUID: "org-theirs",
	}}

	_, err := f.Add(t.Context(), AddRequest{})
	wantErr(t, err, "belongs to organization", "org-theirs")
}

// The address decides only when the config carries no uuid; a uuid conflict
// condemns regardless of a matching address.
func TestUUIDBeatsEmail(t *testing.T) {
	f := newFixture(t)
	f.liveLogin("a@example.com", "", "", "acct-config", liveCreds)
	f.Oracle = &fakeOracle{identity: &claudeapi.Identity{
		UUID: "acct-token", Email: "a@example.com",
	}}

	_, err := f.Add(t.Context(), AddRequest{})
	wantErr(t, err, "resolves to account acct-token", "not acct-config")
}

// Without a uuid, the address decides — case-insensitively, because addresses
// are.
func TestWithoutAUUIDTheAddressDecides(t *testing.T) {
	f := newFixture(t)
	f.liveLogin("A@Example.com", "", "", "", liveCreds)
	f.Oracle = &fakeOracle{identity: &claudeapi.Identity{UUID: "acct-1", Email: "a@example.com"}}

	if _, err := f.Add(t.Context(), AddRequest{}); err != nil {
		t.Errorf("a case difference in the address refused the capture: %v", err)
	}
}

// A capture replaces the credential a strike was issued against, so the
// quarantine no longer describes reality.
func TestAddLiftsTheDeadTokenQuarantine(t *testing.T) {
	f := newFixture(t)
	f.liveLogin("a@example.com", "", "", "acct-1", liveCreds)
	f.resolving("acct-1", "a@example.com", "")
	if _, err := f.Add(t.Context(), AddRequest{}); err != nil {
		t.Fatal(err)
	}

	ids := map[string]usagestore.Identity{"a": {Email: "a@example.com"}}
	if _, err := f.Usage.Record(map[string]usagestore.FetchRecord{
		"a": {Error: claudeapi.KindInvalidGrant, StruckFP: "sha256:dead"},
	}, ids, nil, nil); err != nil {
		t.Fatal(err)
	}
	if !f.Usage.Entries(ids, nil)["a"].TokenDead("") {
		t.Fatal("the account was not quarantined")
	}

	if _, err := f.Add(t.Context(), AddRequest{}); err != nil {
		t.Fatal(err)
	}
	if f.Usage.Entries(ids, nil)["a"].TokenDead("") {
		t.Error("the quarantine survived a fresh capture")
	}
}
