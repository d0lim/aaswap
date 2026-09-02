package swap

import (
	json "encoding/json/v2"
	"slices"
	"strings"
	"testing"
)

// parse is ParseFile with the test's fixed clock.
func parse(t *testing.T, raw string) (*File, []Rename) {
	t.Helper()
	file, renames, err := ParseFile([]byte(raw), testNow)
	if err != nil {
		t.Fatalf("ParseFile: %v\n%s", err, raw)
	}
	return file, renames
}

// names returns a provider's accounts keyed by name, for terse assertions.
func names(t *testing.T, file *File, provider string) map[string]string {
	t.Helper()
	roster := file.Providers[provider]
	if roster == nil {
		t.Fatalf("no roster for provider %q; file holds %v", provider, file.Providers)
	}
	out := map[string]string{}
	for name, account := range roster.Accounts {
		out[name] = account.Email
	}
	return out
}

// The alias a person chose is a better handle than anything derivable, so it
// becomes the name outright.
func TestUpgradeTakesTheNameFromTheAlias(t *testing.T) {
	file, renames := parse(t, `{
	  "activeAccountNumber": 1,
	  "sequence": [1, 2],
	  "accounts": {
	    "1": {"email":"one@example.com","alias":"work"},
	    "2": {"email":"two@example.com"}
	  }}`)

	got := names(t, file, ProviderClaude)
	if got["work"] != "one@example.com" {
		t.Errorf("accounts = %v, want the aliased slot named \"work\"", got)
	}
	// No alias, so the address supplies the name.
	if got["two"] != "two@example.com" {
		t.Errorf("accounts = %v, want the unaliased slot named \"two\"", got)
	}

	// The rename list is what moves the credentials, so it has to name both
	// ends of every move.
	if len(renames) != 2 {
		t.Fatalf("renames = %v, want one per slot", renames)
	}
	for _, r := range renames {
		if r.Number == "" || r.Email == "" || r.Name == "" {
			t.Errorf("rename %+v is missing an end", r)
		}
	}
}

// The alias field is consumed by becoming the key. Left behind it would
// round-trip forever through the unknown-field passthrough and desynchronise
// from the name the moment someone renames the account.
func TestUpgradeConsumesTheAliasField(t *testing.T) {
	file, _ := parse(t, `{"sequence":[1],"accounts":{"1":{"email":"one@example.com","alias":"work"}}}`)

	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"alias"`) {
		t.Errorf("the alias field survived the upgrade:\n%s", data)
	}
}

// An alias the name rules refuse cannot become a name. Failing the whole
// migration over it would strand every account in the store, so the slot falls
// back to its address.
func TestUpgradeFallsBackWhenTheAliasIsNotALegalName(t *testing.T) {
	file, _ := parse(t, `{"sequence":[1],"accounts":{"1":{"email":"one@example.com","alias":".."}}}`)

	got := names(t, file, ProviderClaude)
	if got["one"] != "one@example.com" {
		t.Errorf("accounts = %v, want the slot to fall back to \"one\"", got)
	}
	if _, present := got[".."]; present {
		t.Error(`".." became a name, and it is a path component now`)
	}
}

// The same address across a personal account and an organization is two
// accounts. Both need a name, and the same store must produce the same two
// names every time it is read.
func TestUpgradeResolvesCollisionsDeterministically(t *testing.T) {
	const raw = `{
	  "sequence": [1, 2, 3],
	  "accounts": {
	    "1": {"email":"work@example.com"},
	    "2": {"email":"work@example.com","organizationUuid":"org-2"},
	    "3": {"email":"work@example.com","organizationUuid":"org-3"}
	  }}`

	first, _ := parse(t, raw)
	got := slices.Sorted(func(yield func(string) bool) {
		for name := range first.Providers[ProviderClaude].Accounts {
			if !yield(name) {
				return
			}
		}
	})
	want := []string{"work", "work-2", "work-3"}
	if !slices.Equal(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}

	// Re-reading the same v1 file must not shuffle which account holds which
	// name, or a second run of the migration renames every credential again.
	second, _ := parse(t, raw)
	for name, email := range names(t, first, ProviderClaude) {
		if names(t, second, ProviderClaude)[name] != email {
			t.Errorf("name %q moved between reads", name)
		}
	}
	if a, b := orgOf(t, first, "work-2"), orgOf(t, second, "work-2"); a != b {
		t.Errorf("work-2 named org %q then %q — the ordering is not stable", a, b)
	}
}

func orgOf(t *testing.T, file *File, name string) string {
	t.Helper()
	account := file.Providers[ProviderClaude].Accounts[name]
	if account == nil {
		t.Fatalf("no account named %q", name)
	}
	return account.OrganizationUUID
}

// The active pointer and the display order are both keyed by number in v1 and
// by name in v2. A pointer that survives as a number names nothing.
func TestUpgradeCarriesActiveAndOrder(t *testing.T) {
	file, _ := parse(t, `{
	  "activeAccountNumber": 2,
	  "sequence": [2, 1],
	  "accounts": {
	    "1": {"email":"one@example.com"},
	    "2": {"email":"two@example.com","alias":"work"}
	  }}`)

	roster := file.Providers[ProviderClaude]
	if roster.Active != "work" {
		t.Errorf("active = %q, want \"work\"", roster.Active)
	}
	if !slices.Equal(roster.Order, []string{"work", "one"}) {
		t.Errorf("order = %v, want the v1 sequence carried over by name", roster.Order)
	}
}

// A slot the sequence forgot still exists. v1's reader appended it rather than
// letting it become invisible, and the upgrade must not lose what that saved.
func TestUpgradeKeepsASlotMissingFromTheSequence(t *testing.T) {
	file, _ := parse(t, `{
	  "sequence": [1],
	  "accounts": {
	    "1": {"email":"one@example.com"},
	    "2": {"email":"two@example.com"}
	  }}`)

	got := names(t, file, ProviderClaude)
	if len(got) != 2 {
		t.Errorf("accounts = %v, want both slots", got)
	}
	if !slices.Contains(file.Providers[ProviderClaude].Order, "two") {
		t.Errorf("order = %v, want the forgotten slot appended", file.Providers[ProviderClaude].Order)
	}
}

// A field a future release added has to survive being read and rewritten here.
func TestUpgradePreservesUnknownAccountFields(t *testing.T) {
	file, _ := parse(t, `{"sequence":[1],
	  "accounts":{"1":{"email":"one@example.com","futureField":{"x":1}}}}`)

	data, err := json.Marshal(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "futureField") {
		t.Errorf("an unknown field was dropped:\n%s", data)
	}
}

// Already-current files pass straight through. Re-running the upgrade on one
// would rename every credential for nothing.
func TestParseLeavesACurrentFileAlone(t *testing.T) {
	file, renames := parse(t, `{
	  "schemaVersion": 2,
	  "providers": {
	    "claude": {"active":"work","order":["work"],
	      "accounts":{"work":{"email":"one@example.com"}}},
	    "codex": {"order":["work"],
	      "accounts":{"work":{"email":"one@example.com"}}}
	  }}`)

	if len(renames) != 0 {
		t.Errorf("renames = %v, want none for a current file", renames)
	}
	if got := names(t, file, ProviderClaude); got["work"] != "one@example.com" {
		t.Errorf("claude accounts = %v", got)
	}
	// A provider this build does not implement must survive being read and
	// rewritten, or upgrading one machine strands the other's accounts.
	if got := names(t, file, "codex"); got["work"] != "one@example.com" {
		t.Errorf("codex accounts = %v, want them carried through", got)
	}
}

// An empty store is not a v1 store. Upgrading it must not invent a provider
// section full of nothing.
func TestParseAnEmptyStore(t *testing.T) {
	file, renames := parse(t, `{"sequence":[],"accounts":{}}`)
	if len(renames) != 0 {
		t.Errorf("renames = %v, want none", renames)
	}
	if roster := file.Providers[ProviderClaude]; roster != nil && len(roster.Accounts) != 0 {
		t.Errorf("accounts = %v, want none", roster.Accounts)
	}
}

// The reader refuses rather than guesses. Reading a torn file as "no accounts"
// is what let a later write rebuild the roster from nothing.
func TestParseRefusesWhatItCannotRead(t *testing.T) {
	for _, raw := range []string{`{"accounts":`, `[1,2,3]`, `null`, ``} {
		t.Run(raw, func(t *testing.T) {
			if _, _, err := ParseFile([]byte(raw), testNow); err == nil {
				t.Errorf("ParseFile(%q) returned no error", raw)
			}
		})
	}
}
