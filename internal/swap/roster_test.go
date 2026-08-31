package swap

import (
	json "encoding/json/v2"
	"os"
	"reflect"
	"slices"
	"testing"
)

func TestAnAbsentRosterIsAnEmptyStart(t *testing.T) {
	f := newFixture(t)
	roster, ok, err := f.ReadRoster()
	if err != nil {
		t.Fatal(err)
	}
	if ok || roster != nil {
		t.Errorf("ReadRoster = (%+v, %v), want nothing at all", roster, ok)
	}

	// The caller that tolerates a fresh install gets an empty roster, not an
	// error.
	empty, err := f.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Accounts) != 0 || len(empty.Numbers()) != 0 {
		t.Errorf("RosterOrEmpty = %+v, want empty", empty)
	}
}

// A torn roster read as "no accounts" is what let the next write rebuild the
// roster from nothing, overwriting a live credential backup on the way. It must
// be an error, never an empty start.
func TestAnUnreadableRosterIsAnErrorNotAnEmptyStart(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"truncated mid-write", `{"accounts":{"1":{"email":"a@exam`},
		{"a JSON array", `[1,2,3]`},
		{"a bare number", `123`},
		{"a bare string", `"hello"`},
		{"literal null", `null`},
		{"empty", ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if err := os.WriteFile(f.RosterPath(), []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}

			if _, _, err := f.ReadRoster(); err == nil {
				t.Error("an unreadable roster read as usable")
			}
			if _, err := f.RosterOrEmpty(); err == nil {
				t.Error("an unreadable roster read as an empty start")
			}
			// And nothing overwrote it.
			after, err := os.ReadFile(f.RosterPath())
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != tt.content {
				t.Errorf("the unreadable roster was modified: %q", after)
			}
		})
	}
}

// A roster written by a newer release must survive being read and rewritten by
// an older one — which happens routinely while two implementations share a
// machine.
func TestUnknownFieldsSurviveARewrite(t *testing.T) {
	f := newFixture(t)
	const original = `{
  "activeAccountNumber": 2,
  "lastUpdated": "2026-01-01T00:00:00Z",
  "sequence": [1, 2],
  "accounts": {
    "1": {"email": "a@example.com", "futureAccountField": {"nested": [1, 2]}},
    "2": {"email": "b@example.com", "organizationUuid": "org-2"}
  },
  "futureTopLevelField": "keep me"
}`
	if err := os.WriteFile(f.RosterPath(), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	roster := f.roster()
	roster.Accounts["1"].Alias = "work"
	f.seedRoster(roster)

	raw := f.rawRoster()
	if raw["futureTopLevelField"] != "keep me" {
		t.Errorf("a top-level field this version does not know was dropped: %v", raw)
	}
	accounts := raw["accounts"].(map[string]any)
	one := accounts["1"].(map[string]any)
	if _, present := one["futureAccountField"]; !present {
		t.Errorf("an account field this version does not know was dropped: %v", one)
	}
	nested := one["futureAccountField"].(map[string]any)["nested"]
	if !reflect.DeepEqual(nested, []any{1.0, 2.0}) {
		t.Errorf("the unknown field's value changed: %v", nested)
	}
	// The known edit landed.
	if one["alias"] != "work" {
		t.Errorf("alias = %v, want the edit", one["alias"])
	}
}

// A slot that exists must never become invisible because the ordering list
// forgot it — a roster edited by hand, or a write interrupted between the two.
func TestNumbersIncludeSlotsMissingFromTheSequence(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(f.RosterPath(), []byte(`{
	  "sequence": [2],
	  "accounts": {
	    "1": {"email": "a@example.com"},
	    "2": {"email": "b@example.com"},
	    "10": {"email": "j@example.com"}
	  }
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got := f.roster().Numbers()
	// The sequence's own order first, then the strays in numeric order.
	if want := []string{"2", "1", "10"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Numbers = %v, want %v", got, want)
	}
}

// A sequence entry naming a slot that is gone must not produce a phantom
// account.
func TestNumbersSkipStaleSequenceEntries(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(f.RosterPath(), []byte(
		`{"sequence":[1,2,3],"accounts":{"2":{"email":"b@example.com"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := f.roster().Numbers(); !reflect.DeepEqual(got, []string{"2"}) {
		t.Errorf("Numbers = %v, want only the slot that exists", got)
	}
}

// Reusing a number a user just removed would make "account 3" mean a different
// account than it did a minute ago, in a shell history full of `ccswap switch 3`.
func TestNextNumberDoesNotReuseAFreedSlot(t *testing.T) {
	f := newFixture(t)
	roster := newRoster(f.now)
	for _, num := range []string{"1", "2", "3"} {
		roster.Insert(num, &Account{Email: num + "@example.com"}, f.now)
	}
	roster.Remove("2", f.now)

	if got := roster.NextNumber(); got != 4 {
		t.Errorf("NextNumber = %d, want 4 — never the freed 2", got)
	}
	// And an empty roster starts at one.
	if got := newRoster(f.now).NextNumber(); got != 1 {
		t.Errorf("NextNumber on an empty roster = %d, want 1", got)
	}
}

func TestInsertKeepsTheSequenceOrdered(t *testing.T) {
	f := newFixture(t)
	roster := newRoster(f.now)
	for _, num := range []string{"3", "1", "10", "2"} {
		roster.Insert(num, &Account{Email: num + "@example.com"}, f.now)
	}
	if want := []int{1, 2, 3, 10}; !reflect.DeepEqual(roster.Sequence, want) {
		t.Errorf("Sequence = %v, want %v", roster.Sequence, want)
	}
	// Re-inserting the same slot does not duplicate it.
	roster.Insert("2", &Account{Email: "new@example.com"}, f.now)
	if want := []int{1, 2, 3, 10}; !reflect.DeepEqual(roster.Sequence, want) {
		t.Errorf("Sequence = %v after a re-insert, want %v", roster.Sequence, want)
	}
}

// The active pointer must not outlive the slot it names, or the next read
// reports an account that no longer exists.
func TestRemovingTheActiveSlotClearsThePointer(t *testing.T) {
	f := newFixture(t)
	roster := newRoster(f.now)
	roster.Insert("1", &Account{Email: "a@example.com"}, f.now)
	roster.Insert("2", &Account{Email: "b@example.com"}, f.now)
	roster.SetActive("2", f.now)

	roster.Remove("2", f.now)
	if num, ok := roster.Active(); ok {
		t.Errorf("Active = %q, want nothing after the slot was removed", num)
	}
	// Removing a different slot leaves the pointer alone.
	roster.SetActive("1", f.now)
	roster.Remove("9", f.now)
	if num, ok := roster.Active(); !ok || num != "1" {
		t.Errorf("Active = (%q, %v), want slot 1", num, ok)
	}
}

// A pointer at a slot that vanished behind the roster's back reports nothing,
// rather than sending every caller looking for an account that is gone.
func TestActiveIgnoresAPointerAtAMissingSlot(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(f.RosterPath(), []byte(
		`{"activeAccountNumber":7,"sequence":[1],"accounts":{"1":{"email":"a@example.com"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if num, ok := f.roster().Active(); ok {
		t.Errorf("Active = %q, want nothing", num)
	}
}

// Email alone is not identity: the same address exists across a personal
// account and its organizations, and those are different accounts with
// different quotas.
func TestFindSlotMatchesOnTheWholeComposite(t *testing.T) {
	f := newFixture(t)
	roster := newRoster(f.now)
	roster.Insert("1", &Account{Email: "a@example.com"}, f.now)
	roster.Insert("2", &Account{Email: "a@example.com", OrganizationUUID: "org-2"}, f.now)

	tests := []struct {
		identity Identity
		want     string
		wantOK   bool
	}{
		{Identity{Email: "a@example.com"}, "1", true},
		{Identity{Email: "a@example.com", OrganizationUUID: "org-2"}, "2", true},
		{Identity{Email: "a@example.com", OrganizationUUID: "org-9"}, "", false},
		{Identity{Email: "b@example.com"}, "", false},
	}
	for _, tt := range tests {
		got, ok := roster.FindSlot(tt.identity)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("FindSlot(%+v) = (%q, %v), want (%q, %v)", tt.identity, got, ok, tt.want, tt.wantOK)
		}
	}
}

func TestAuthKindDefaultsToOAuth(t *testing.T) {
	tests := []struct {
		name    string
		account *Account
		want    Kind
	}{
		{"an explicit API key", &Account{Kind: KindAPIKey}, KindAPIKey},
		{"an explicit OAuth record", &Account{Kind: KindOAuth}, KindOAuth},
		// Every record written before kinds existed.
		{"no kind at all", &Account{}, KindOAuth},
		{"a nil record", nil, KindOAuth},
		// A value from a future release reads as OAuth: treating an
		// unrecognized kind as an API key would suppress the usage the slot
		// actually reports.
		{"a kind this version does not know", &Account{Kind: "future_kind"}, KindOAuth},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.account.AuthKind(); got != tt.want {
				t.Errorf("AuthKind = %q, want %q", got, tt.want)
			}
		})
	}
}

// The file's shape is a cross-implementation contract.
func TestTheRosterOnDiskShape(t *testing.T) {
	f := newFixture(t)
	roster := newRoster(f.now)
	roster.Insert("1", &Account{
		Email:            "a@example.com",
		UUID:             "acct-1",
		OrganizationUUID: "org-1",
		OrganizationName: "Example",
		Added:            Timestamp(f.now),
		Alias:            "work",
	}, f.now)
	roster.SetActive("1", f.now)
	f.seedRoster(roster)

	raw := f.rawRoster()
	if raw["activeAccountNumber"] != 1.0 {
		t.Errorf("activeAccountNumber = %v, want the number 1", raw["activeAccountNumber"])
	}
	if raw["lastUpdated"] != "2026-06-15T12:00:00Z" {
		t.Errorf("lastUpdated = %v", raw["lastUpdated"])
	}
	if !reflect.DeepEqual(raw["sequence"], []any{1.0}) {
		t.Errorf("sequence = %v", raw["sequence"])
	}
	one := raw["accounts"].(map[string]any)["1"].(map[string]any)
	for key, want := range map[string]any{
		"email": "a@example.com", "uuid": "acct-1",
		"organizationUuid": "org-1", "organizationName": "Example",
		"added": "2026-06-15T12:00:00Z", "alias": "work",
	} {
		if one[key] != want {
			t.Errorf("accounts.1.%s = %v, want %v", key, one[key], want)
		}
	}
	// A slot that is neither disabled nor an API key writes neither key —
	// matching how the record is written elsewhere.
	for _, key := range []string{"disabled", "kind"} {
		if _, present := one[key]; present {
			t.Errorf("%q was written for a default record: %v", key, one)
		}
	}
}

// An empty roster writes empty arrays and maps, not nulls: the Python reader
// indexes them.
func TestAnEmptyRosterWritesEmptyContainers(t *testing.T) {
	f := newFixture(t)
	f.seedRoster(&Roster{})

	raw := f.rawRoster()
	if !reflect.DeepEqual(raw["sequence"], []any{}) {
		t.Errorf("sequence = %v, want an empty array", raw["sequence"])
	}
	if !reflect.DeepEqual(raw["accounts"], map[string]any{}) {
		t.Errorf("accounts = %v, want an empty object", raw["accounts"])
	}
	if raw["activeAccountNumber"] != nil {
		t.Errorf("activeAccountNumber = %v, want null", raw["activeAccountNumber"])
	}
}

// The roster is a file people open in an editor.
func TestTheRosterIsIndentedAndOwnerOnly(t *testing.T) {
	f := newFixture(t)
	f.seedAccounts(map[string]*Account{"1": {Email: "a@example.com"}})

	data, err := os.ReadFile(f.RosterPath())
	if err != nil {
		t.Fatal(err)
	}
	var check map[string]any
	if err := json.Unmarshal(data, &check); err != nil {
		t.Fatalf("sequence.json is not a valid JSON object: %v\n%s", err, data)
	}
	text := string(data)
	if !hasLine(text, `  "sequence": [`) {
		t.Errorf("sequence.json is not indented two spaces:\n%s", text)
	}
	if text[len(text)-1] != '\n' {
		t.Error("sequence.json has no trailing newline")
	}

	info, err := os.Stat(f.RosterPath())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 0600", perm)
	}
}

func hasLine(text, line string) bool {
	return slices.Contains(splitLines(text), line)
}

func splitLines(text string) []string {
	var out []string
	start := 0
	for i := range len(text) {
		if text[i] == '\n' {
			out = append(out, text[start:i])
			start = i + 1
		}
	}
	if start < len(text) {
		out = append(out, text[start:])
	}
	return out
}
