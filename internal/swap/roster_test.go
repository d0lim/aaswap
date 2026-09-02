package swap

import (
	json "encoding/json/v2"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/testutil"
)

func TestAnAbsentStoreIsAnEmptyStart(t *testing.T) {
	f := newFixture(t)
	_, found, _, err := f.readStore()
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Error("a store that was never written reported as found")
	}

	roster, err := f.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Accounts) != 0 {
		t.Errorf("accounts = %v, want none", roster.Accounts)
	}
}

// A torn table read as "no accounts" is what let the next write rebuild it from
// nothing, overwriting a live credential backup on the way. It must be an
// error, never an empty start.
func TestAnUnreadableStoreIsAnErrorNotAnEmptyStart(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"truncated mid-object", `{"providers":`},
		{"not an object", `[1,2,3]`},
		{"literally null", `null`},
		{"empty", ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if err := os.WriteFile(f.RosterPath(), []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, _, _, err := f.readStore(); err == nil {
				t.Error("an unreadable table read as an empty start")
			}
			if _, err := f.RosterOrEmpty(); err == nil {
				t.Error("RosterOrEmpty invented an empty roster over an unreadable file")
			}
		})
	}
}

// A table written by a newer release must survive being read and rewritten by
// an older one — which happens routinely while two releases share a machine.
func TestUnknownFieldsSurviveARewrite(t *testing.T) {
	f := newFixture(t)
	const original = `{"schemaVersion":2,"futureTopLevel":{"a":1},
	  "providers":{"claude":{"active":"work","order":["work"],
	    "accounts":{"work":{"email":"one@example.com","futureField":{"x":1}}}}}}`
	if err := os.WriteFile(f.RosterPath(), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	roster, err := f.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if err := f.WriteRoster(roster); err != nil {
		t.Fatal(err)
	}

	raw := f.rawRoster()
	if _, present := raw["futureTopLevel"]; !present {
		t.Errorf("an unknown top-level field was dropped: %v", raw)
	}
	data, err := os.ReadFile(f.RosterPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "futureField") {
		t.Errorf("an unknown account field was dropped:\n%s", data)
	}
}

// A provider this build does not implement must survive a write aimed at
// another one. Its accounts' credentials are still on disk, and dropping the
// section is what would leave nothing naming them.
func TestWritingOneProviderLeavesTheOthersAlone(t *testing.T) {
	f := newFixture(t)
	const original = `{"schemaVersion":2,"providers":{
	  "claude":{"order":[],"accounts":{}},
	  "codex":{"active":"work","order":["work"],
	    "accounts":{"work":{"email":"one@example.com"}}}}}`
	if err := os.WriteFile(f.RosterPath(), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	roster, err := f.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	roster.Insert("mine", &Account{Email: "mine@example.com"})
	if err := f.WriteRoster(roster); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(f.RosterPath())
	if err != nil {
		t.Fatal(err)
	}
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	codex := file.Providers["codex"]
	if codex == nil || codex.Accounts["work"] == nil {
		t.Fatalf("the codex section did not survive:\n%s", data)
	}
	if codex.Active != "work" {
		t.Errorf("codex active = %q, want it untouched", codex.Active)
	}
}

// An account that exists must never become invisible because the ordering list
// forgot it — a table edited by hand, or a write interrupted between the two.
func TestNamesIncludeAccountsMissingFromTheOrder(t *testing.T) {
	roster := newRoster()
	roster.Accounts["one"] = &Account{Email: "one@example.com"}
	roster.Accounts["two"] = &Account{Email: "two@example.com"}
	roster.Order = []string{"two"}

	if got := roster.Names(); !slices.Equal(got, []string{"two", "one"}) {
		t.Errorf("Names() = %v, want the forgotten account appended", got)
	}
}

// An order entry naming an account that is gone must not produce a phantom.
func TestNamesSkipStaleOrderEntries(t *testing.T) {
	roster := newRoster()
	roster.Accounts["one"] = &Account{Email: "one@example.com"}
	roster.Order = []string{"one", "ghost"}

	if got := roster.Names(); !slices.Equal(got, []string{"one"}) {
		t.Errorf("Names() = %v, want only the account that exists", got)
	}
}

func TestInsertAppendsToTheOrder(t *testing.T) {
	roster := newRoster()
	for _, name := range []string{"b", "a", "c"} {
		roster.Insert(name, &Account{Email: name + "@example.com"})
	}
	// Insertion order, not alphabetical: this list is the rotation order, and
	// an account added last should not jump ahead of one added first.
	if !slices.Equal(roster.Order, []string{"b", "a", "c"}) {
		t.Errorf("order = %v, want insertion order", roster.Order)
	}
	roster.Insert("a", &Account{Email: "a@example.com"})
	if !slices.Equal(roster.Order, []string{"b", "a", "c"}) {
		t.Errorf("order = %v after a re-insert, want no duplicate", roster.Order)
	}
}

// The active pointer must not outlive the account it names, or the next read
// reports something that no longer exists.
func TestRemovingTheActiveAccountClearsThePointer(t *testing.T) {
	roster := newRoster()
	roster.Insert("one", &Account{Email: "one@example.com"})
	roster.Insert("two", &Account{Email: "two@example.com"})
	roster.SetActive("one")

	roster.Remove("one")
	if _, ok := roster.ActiveName(); ok {
		t.Error("the pointer survived the account it named")
	}
	if slices.Contains(roster.Order, "one") {
		t.Errorf("order = %v, want the removed account gone", roster.Order)
	}

	roster.SetActive("two")
	roster.Remove("gone")
	if name, ok := roster.ActiveName(); !ok || name != "two" {
		t.Errorf("ActiveName() = (%q, %v), want two", name, ok)
	}
}

// A pointer at an account that vanished behind the roster's back reports
// nothing, rather than sending every caller looking for something gone.
func TestActiveIgnoresAPointerAtAMissingAccount(t *testing.T) {
	roster := newRoster()
	roster.Insert("one", &Account{Email: "one@example.com"})
	roster.Active = "ghost"

	if name, ok := roster.ActiveName(); ok {
		t.Errorf("ActiveName() = (%q, true), want nothing", name)
	}
}

// A rename has to carry the pointer and the order entry with the account, or
// the store names one thing and points at another.
func TestRenameCarriesTheOrderAndThePointer(t *testing.T) {
	roster := newRoster()
	roster.Insert("one", &Account{Email: "one@example.com"})
	roster.Insert("two", &Account{Email: "two@example.com"})
	roster.SetActive("one")

	roster.Rename("one", "work")

	if _, gone := roster.Accounts["one"]; gone {
		t.Error("the old name survived")
	}
	if roster.Accounts["work"].Email != "one@example.com" {
		t.Errorf("accounts = %v", roster.Accounts)
	}
	if !slices.Equal(roster.Order, []string{"work", "two"}) {
		t.Errorf("order = %v, want the rename in place", roster.Order)
	}
	if name, _ := roster.ActiveName(); name != "work" {
		t.Errorf("active = %q, want it renamed too", name)
	}
}

// Email alone is not identity: the same address exists across a personal
// account and its organizations, and those are different accounts with
// different quotas.
func TestFindNameMatchesOnTheWholeComposite(t *testing.T) {
	roster := newRoster()
	roster.Insert("personal", &Account{Email: "same@example.com"})
	roster.Insert("acme", &Account{Email: "same@example.com", OrganizationUUID: "org-1"})

	if got, ok := roster.FindName(Identity{Email: "same@example.com"}); !ok || got != "personal" {
		t.Errorf("FindName(personal) = (%q, %v)", got, ok)
	}
	if got, ok := roster.FindName(Identity{
		Email: "same@example.com", OrganizationUUID: "org-1"}); !ok || got != "acme" {
		t.Errorf("FindName(org) = (%q, %v)", got, ok)
	}
	if _, ok := roster.FindName(Identity{
		Email: "same@example.com", OrganizationUUID: "org-2"}); ok {
		t.Error("an unknown organization matched")
	}
}

// A name already held has to be avoided rather than overwritten.
func TestNameForAvoidsWhatIsTaken(t *testing.T) {
	roster := newRoster()
	roster.Insert("work", &Account{Email: "work@example.com"})

	if got := roster.NameFor("work@other.com"); got != "work-2" {
		t.Errorf("NameFor = %q, want work-2", got)
	}
	if got := roster.NameFor("fresh@example.com"); got != "fresh" {
		t.Errorf("NameFor = %q, want fresh", got)
	}
}

// Anything other than the API-key marker reads as OAuth, including a value from
// a future release: treating an unrecognized kind as an API key would suppress
// the usage the account actually reports.
func TestAuthKindDefaultsToOAuth(t *testing.T) {
	tests := []struct {
		kind Kind
		want Kind
	}{
		{"", KindOAuth},
		{KindOAuth, KindOAuth},
		{KindAPIKey, KindAPIKey},
		{"something-newer", KindOAuth},
	}
	for _, tt := range tests {
		if got := (&Account{Kind: tt.kind}).AuthKind(); got != tt.want {
			t.Errorf("AuthKind(%q) = %q, want %q", tt.kind, got, tt.want)
		}
	}
	if got := (*Account)(nil).AuthKind(); got != KindOAuth {
		t.Errorf("a nil account's AuthKind = %q", got)
	}
}

// The file's shape is a contract with every other release that reads it.
func TestTheStoreOnDiskShape(t *testing.T) {
	f := newFixture(t)
	roster := newRoster()
	roster.Insert("work", &Account{
		Email:            "one@example.com",
		UUID:             "acct-1",
		OrganizationUUID: "org-1",
		OrganizationName: "Example",
		Added:            Timestamp(f.now),
	})
	roster.SetActive("work")
	if err := f.WriteRoster(roster); err != nil {
		t.Fatal(err)
	}

	raw := f.rawRoster()
	if raw["schemaVersion"] != float64(SchemaVersion) {
		t.Errorf("schemaVersion = %v, want %d", raw["schemaVersion"], SchemaVersion)
	}
	providers, ok := raw["providers"].(map[string]any)
	if !ok {
		t.Fatalf("providers = %v, want an object", raw["providers"])
	}
	claude, ok := providers[ProviderClaude].(map[string]any)
	if !ok {
		t.Fatalf("providers.claude = %v, want an object", providers[ProviderClaude])
	}
	if claude["active"] != "work" {
		t.Errorf("active = %v, want \"work\"", claude["active"])
	}
	if order, _ := claude["order"].([]any); len(order) != 1 || order[0] != "work" {
		t.Errorf("order = %v", claude["order"])
	}
	account, ok := claude["accounts"].(map[string]any)["work"].(map[string]any)
	if !ok {
		t.Fatalf("accounts = %v", claude["accounts"])
	}
	for key, want := range map[string]any{
		"email":            "one@example.com",
		"uuid":             "acct-1",
		"organizationUuid": "org-1",
		"organizationName": "Example",
	} {
		if account[key] != want {
			t.Errorf("%s = %v, want %v", key, account[key], want)
		}
	}
	// The name is the key, so it must not also be a field: two spellings of one
	// fact is how they come to disagree.
	if _, present := account["alias"]; present {
		t.Errorf("the account carries an alias field: %v", account)
	}
	if _, present := account["name"]; present {
		t.Errorf("the account repeats its own key as a field: %v", account)
	}
}

// An empty store writes empty containers, not nulls: a reader indexes them.
func TestAnEmptyStoreWritesEmptyContainers(t *testing.T) {
	f := newFixture(t)
	if err := f.WriteRoster(newRoster()); err != nil {
		t.Fatal(err)
	}
	claude := f.rawRoster()["providers"].(map[string]any)[ProviderClaude].(map[string]any)
	if claude["order"] == nil {
		t.Error("order wrote as null")
	}
	if claude["accounts"] == nil {
		t.Error("accounts wrote as null")
	}
}

// The table is a file people open in an editor, and it names their accounts.
func TestTheStoreIsIndentedAndOwnerOnly(t *testing.T) {
	f := newFixture(t)
	roster := newRoster()
	roster.Insert("work", &Account{Email: "one@example.com"})
	if err := f.WriteRoster(roster); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(f.RosterPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "\n  ") {
		t.Errorf("the table is not indented:\n%s", data)
	}
	testutil.AssertPerm(t, f.RosterPath(), 0o600)
}
