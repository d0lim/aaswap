package swap

import (
	"context"
	json "encoding/json/v2"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/realiti4/claude-swap/internal/claudeapi"
	"github.com/realiti4/claude-swap/internal/jsonout"
	"github.com/realiti4/claude-swap/internal/usage"
	"github.com/realiti4/claude-swap/internal/usagestore"
)

// fakeFetcher answers usage fetches without a network.
//
// The counter is atomic because a collect pass fetches in parallel — the same
// concurrency the real fetcher meets.
type fakeFetcher struct {
	callCount atomic.Int64
	byNumber  map[string]claudeapi.UsageOutcome
}

func (f *fakeFetcher) FetchUsageForAccount(_ context.Context, req claudeapi.FetchRequest) claudeapi.UsageOutcome {
	f.callCount.Add(1)
	if outcome, ok := f.byNumber[req.AccountNum]; ok {
		return outcome
	}
	return claudeapi.UsageOutcome{Error: claudeapi.KindTimeout}
}

func (f *fakeFetcher) calls() int { return int(f.callCount.Load()) }

func measured(pct float64, resetsAt string) *usage.Result {
	return &usage.Result{
		FiveHour: &usage.Window{Pct: pct / 2, ResetsAt: resetsAt},
		SevenDay: &usage.Window{Pct: pct, ResetsAt: resetsAt},
	}
}

// measuring wires the fixture with a fetcher answering the given measurements.
func (f *fixture) measuring(byNumber map[string]*usage.Result) *fakeFetcher {
	outcomes := map[string]claudeapi.UsageOutcome{}
	for num, result := range byNumber {
		outcomes[num] = claudeapi.UsageOutcome{Usage: result}
	}
	fetcher := &fakeFetcher{byNumber: outcomes}
	f.Fetcher = fetcher
	return fetcher
}

func (f *fixture) snapshot() *Snapshot {
	f.t.Helper()
	snapshot, err := f.TakeSnapshot(f.t.Context(), CollectRequest{})
	if err != nil {
		f.t.Fatal(err)
	}
	return snapshot
}

func init() {
	// Collapse the request stagger: the STAGGERING is what the tests check, not
	// the wall clock it costs.
	fetchStagger = time.Millisecond
}

func TestCollectMeasuresEveryDueAccount(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	reset := f.now.Add(3 * time.Hour).Format(time.RFC3339)
	fetcher := f.measuring(map[string]*usage.Result{
		"1": measured(40, reset), "2": measured(10, reset),
	})

	snapshot := f.snapshot()
	if fetcher.calls() != 2 {
		t.Errorf("fetches = %d, want one per account", fetcher.calls())
	}
	for num, want := range map[string]float64{"1": 40, "2": 10} {
		decision, known := snapshot.Entries[num].DecisionValue()
		if !known || decision.Usage.SevenDay.Pct != want {
			t.Errorf("slot %s = %+v, want %v%%", num, decision, want)
		}
	}
}

// A measurement fresh enough to serve costs no request, however many surfaces
// repaint.
func TestASecondCollectServesFromTheStore(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	reset := f.now.Add(3 * time.Hour).Format(time.RFC3339)
	fetcher := f.measuring(map[string]*usage.Result{
		"1": measured(40, reset), "2": measured(10, reset),
	})

	f.snapshot()
	before := fetcher.calls()
	f.snapshot()
	if fetcher.calls() != before {
		t.Errorf("a repeat collect spent %d more requests", fetcher.calls()-before)
	}
}

// One failed round trip must not blank every account.
func TestAFailedFetchKeepsTheOthersAndTheLastMeasurement(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	reset := f.now.Add(3 * time.Hour).Format(time.RFC3339)
	f.measuring(map[string]*usage.Result{"1": measured(40, reset), "2": measured(10, reset)})
	f.snapshot()

	// Now slot 1's fetch fails while slot 2's succeeds.
	f.advance(10 * time.Minute)
	f.Fetcher = &fakeFetcher{byNumber: map[string]claudeapi.UsageOutcome{
		"1": {Error: claudeapi.KindTimeout},
		"2": {Usage: measured(15, reset)},
	}}

	snapshot := f.snapshot()
	decision, known := snapshot.Entries["1"].DecisionValue()
	if !known || decision.Usage.SevenDay.Pct != 40 {
		t.Errorf("slot 1 = %+v, want its last measurement still served", decision)
	}
	if decision, known := snapshot.Entries["2"].DecisionValue(); !known || decision.Usage.SevenDay.Pct != 15 {
		t.Errorf("slot 2 = %+v, want the fresh measurement", decision)
	}
}

// Sentinels answer for an account instead of a measurement, and never reach the
// endpoint.
func TestStaticSentinels(t *testing.T) {
	tests := []struct {
		name  string
		view  AccountView
		want  string
		fetch bool
	}{
		{
			name: "a managed API key has no subscription quota",
			view: AccountView{Credentials: "sk-ant-api03-abc", Account: &Account{}},
			want: SentinelAPIKey,
		},
		{
			name: "an empty slot",
			view: AccountView{Account: &Account{}},
			want: SentinelNoCredentials,
		},
		{
			// "No credentials" would send the user to re-add a slot that has
			// one.
			name: "a credential that exists but cannot be read",
			view: AccountView{Unreadable: true, Account: &Account{}},
			want: SentinelKeychainUnavailable,
		},
		{
			name: "a credential with no access token",
			view: AccountView{Credentials: `{"claudeAiOauth":{"refreshToken":"r"}}`, Account: &Account{}},
			want: SentinelNoCredentials,
		},
		{
			// An expired ACTIVE token is deliberately not static: the fetch
			// path reaches it.
			name:  "an ordinary OAuth credential",
			view:  AccountView{Credentials: liveCreds, Account: &Account{}},
			want:  "",
			fetch: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if got := f.staticSentinel(tt.view); got != tt.want {
				t.Errorf("staticSentinel = %q, want %q", got, tt.want)
			}
		})
	}
}

// A sentinel account is never fetched: there is nothing to ask about.
func TestASentinelAccountSpendsNoRequest(t *testing.T) {
	f := newFixture(t)
	roster := f.seedAccounts(map[string]*Account{"1": {Email: "one@example.com"}})
	f.seedRoster(roster)
	if err := f.Creds.WriteAccount("1", "one@example.com", "sk-ant-api03-abc"); err != nil {
		t.Fatal(err)
	}
	fetcher := f.measuring(map[string]*usage.Result{})

	snapshot := f.snapshot()
	if fetcher.calls() != 0 {
		t.Errorf("an API-key slot spent %d requests", fetcher.calls())
	}
	if got := snapshot.Entries["1"].Sentinel; got != SentinelAPIKey {
		t.Errorf("sentinel = %q, want %q", got, SentinelAPIKey)
	}
}

// A dead refresh lineage quarantines the slot: no more fetches, and the state
// says only a re-login helps.
func TestAQuarantinedAccountIsNotFetched(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	stored, _ := f.Creds.ReadAccount("2", "two@example.com")
	ids := map[string]usagestore.Identity{"2": {Email: "two@example.com"}}
	if _, err := f.Usage.Record(map[string]usagestore.FetchRecord{
		"2": {Error: claudeapi.KindInvalidGrant, StruckFP: claudeapi.Fingerprint(stored)},
	}, ids, nil, nil); err != nil {
		t.Fatal(err)
	}

	fetcher := f.measuring(map[string]*usage.Result{"1": measured(40, "")})
	snapshot := f.snapshot()

	if got := snapshot.Entries["2"].Sentinel; got != SentinelReloginRequired {
		t.Errorf("sentinel = %q, want %q", got, SentinelReloginRequired)
	}
	// Only slot 1 was asked about.
	if fetcher.calls() != 1 {
		t.Errorf("fetches = %d, want only the healthy account", fetcher.calls())
	}
}

// A strike condemns a generation, not a slot. Replacing the credential heals
// it, and the stale row must be cleared too, or display and fetch eligibility
// disagree and the slot silently freezes.
func TestAHealedStrikeIsClearedFromTheStore(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	ids := map[string]usagestore.Identity{"2": {Email: "two@example.com"}}
	if _, err := f.Usage.Record(map[string]usagestore.FetchRecord{
		"2": {Error: claudeapi.KindInvalidGrant, StruckFP: "sha256:a-generation-long-gone"},
	}, ids, nil, nil); err != nil {
		t.Fatal(err)
	}

	f.measuring(map[string]*usage.Result{"1": measured(40, ""), "2": measured(10, "")})
	snapshot := f.snapshot()

	if got := snapshot.Entries["2"].Sentinel; got == SentinelReloginRequired {
		t.Error("a healed strike still reports the quarantine")
	}
	if f.Usage.Entries(ids, nil)["2"].AuthDeadStrikes != 0 {
		t.Error("the stale strike row survived, so fetch eligibility still disagrees with the display")
	}
}

func TestListPayload(t *testing.T) {
	f := newFixture(t)
	roster := f.seedAccounts(map[string]*Account{
		"1": {Email: "one@example.com", OrganizationName: "Example", OrganizationUUID: "org-1", Alias: "work"},
		"2": {Email: "two@example.com", Disabled: true},
	})
	roster.SetActive("1", f.now)
	f.seedRoster(roster)
	f.setLiveIdentity("one@example.com", "org-1", "Example", "")
	if err := f.Creds.WriteActive(liveCreds); err != nil {
		t.Fatal(err)
	}
	reset := f.now.Add(3 * time.Hour).Format(time.RFC3339)
	f.measuring(map[string]*usage.Result{"1": measured(40, reset), "2": measured(10, reset)})

	payload := f.ListPayload(f.snapshot())
	if payload.SchemaVersion != jsonout.SchemaVersion {
		t.Errorf("schemaVersion = %d", payload.SchemaVersion)
	}
	if payload.ActiveAccountNumber == nil || *payload.ActiveAccountNumber != 1 {
		t.Errorf("activeAccountNumber = %v, want 1", payload.ActiveAccountNumber)
	}
	if len(payload.Accounts) != 2 {
		t.Fatalf("accounts = %d", len(payload.Accounts))
	}

	first := payload.Accounts[0]
	if first.Number != 1 || first.Email != "one@example.com" || !first.Active {
		t.Errorf("row = %+v", first)
	}
	if first.OrganizationName != "Example" || !first.IsOrganization {
		t.Errorf("organization fields = %+v", first)
	}
	if first.Alias != "work" || first.Disabled {
		t.Errorf("alias/disabled = %q/%v", first.Alias, first.Disabled)
	}
	if first.UsageStatus != jsonout.StatusOK || first.Usage == nil {
		t.Fatalf("usage = (%q, %+v)", first.UsageStatus, first.Usage)
	}
	if first.Usage.SevenDay.Pct != 40 {
		t.Errorf("sevenDay.pct = %v", first.Usage.SevenDay.Pct)
	}
	// The countdown is recomputed, not carried from the fetch.
	if first.Usage.SevenDay.Countdown != "3h 0m" {
		t.Errorf("countdown = %q, want it recomputed", first.Usage.SevenDay.Countdown)
	}
	if first.UsageFetchedAt == "" || first.UsageAgeSeconds == nil {
		t.Errorf("freshness fields = (%q, %v)", first.UsageFetchedAt, first.UsageAgeSeconds)
	}

	// A disabled slot says so, and only when it is.
	if !payload.Accounts[1].Disabled {
		t.Error("the disabled slot did not report it")
	}
}

// The `usage` field is decision-grade: a measurement too old to act on appears
// under lastGoodUsage instead, so a script keying on "ok" never acts on it.
func TestAStaleMeasurementMovesToLastGood(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	reset := f.now.Add(24 * time.Hour).Format(time.RFC3339)
	f.measuring(map[string]*usage.Result{"1": measured(40, reset), "2": measured(10, reset)})
	f.snapshot()

	// Age past the decision horizon with nothing making the staleness
	// deliberate, and no fetcher to refresh it.
	f.advance(usagestore.TrustMaxAge + time.Hour)
	f.Fetcher = nil

	payload := f.ListPayload(f.snapshot())
	row := payload.Accounts[0]
	if row.UsageStatus != jsonout.StatusUnavailable || row.Usage != nil {
		t.Errorf("usage = (%q, %+v), want it withheld", row.UsageStatus, row.Usage)
	}
	if row.LastGoodUsage == nil || row.LastGoodUsage.SevenDay.Pct != 40 {
		t.Errorf("lastGoodUsage = %+v, want the old measurement for display", row.LastGoodUsage)
	}
	if row.LastGoodFetchedAt == "" || row.LastGoodAgeSeconds == nil {
		t.Error("the last-good freshness fields are missing")
	}
	// The two must never both be filled: that is the confusion the split exists
	// to prevent.
	if row.UsageFetchedAt != "" {
		t.Error("a withheld measurement still carried its own freshness fields")
	}
}

func TestStatusPayload(t *testing.T) {
	t.Run("no live login at all", func(t *testing.T) {
		f := newFixture(t)
		f.twoAccounts()
		f.clearLiveIdentity()
		payload := f.StatusPayload(f.snapshot())
		if payload.Active != nil {
			t.Errorf("active = %+v, want null", payload.Active)
		}
	})

	t.Run("a live login cswap does not manage", func(t *testing.T) {
		f := newFixture(t)
		f.twoAccounts()
		f.setLiveIdentity("stranger@example.com", "", "", "")
		payload := f.StatusPayload(f.snapshot())
		if payload.Active == nil || payload.Active.Managed {
			t.Fatalf("active = %+v, want an unmanaged account", payload.Active)
		}
		if payload.Active.Email != "stranger@example.com" {
			t.Errorf("email = %q", payload.Active.Email)
		}
		if payload.Active.Number != nil {
			t.Error("an unmanaged account was given a slot number")
		}
	})

	t.Run("a managed live login", func(t *testing.T) {
		f := newFixture(t)
		f.twoAccounts()
		reset := f.now.Add(3 * time.Hour).Format(time.RFC3339)
		f.measuring(map[string]*usage.Result{"1": measured(40, reset), "2": measured(10, reset)})

		payload := f.StatusPayload(f.snapshot())
		if payload.Active == nil || !payload.Active.Managed {
			t.Fatalf("active = %+v", payload.Active)
		}
		if payload.Active.Number == nil || *payload.Active.Number != 1 {
			t.Errorf("number = %v", payload.Active.Number)
		}
		if payload.Active.UsageStatus != jsonout.StatusOK || payload.Active.Usage == nil {
			t.Errorf("usage = (%q, %+v)", payload.Active.UsageStatus, payload.Active.Usage)
		}
		if payload.TotalManagedAccounts == nil || *payload.TotalManagedAccounts != 2 {
			t.Errorf("totalManagedAccounts = %v", payload.TotalManagedAccounts)
		}
	})
}

// The shape is what scripts depend on, so it is asserted as JSON, not just as
// Go values.
func TestTheListPayloadJSONShape(t *testing.T) {
	f := newFixture(t)
	roster := f.seedAccounts(map[string]*Account{"1": {Email: "one@example.com"}})
	roster.SetActive("1", f.now)
	f.seedRoster(roster)
	f.setLiveIdentity("one@example.com", "", "", "")
	if err := f.Creds.WriteActive(liveCreds); err != nil {
		t.Fatal(err)
	}
	f.measuring(map[string]*usage.Result{"1": measured(40, f.now.Add(3*time.Hour).Format(time.RFC3339))})

	encoded, err := json.Marshal(f.ListPayload(f.snapshot()))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"schemaVersion", "activeAccountNumber", "accounts"} {
		if _, present := raw[key]; !present {
			t.Errorf("%q is missing: %s", key, encoded)
		}
	}
	row := raw["accounts"].([]any)[0].(map[string]any)
	for _, key := range []string{
		"number", "email", "organizationName", "organizationUuid",
		"isOrganization", "active", "usageStatus", "usage",
	} {
		if _, present := row[key]; !present {
			t.Errorf("the account row is missing %q: %v", key, row)
		}
	}
	// Absent fields must be absent, not null: a consumer distinguishes them.
	for _, key := range []string{"alias", "disabled", "lastGoodUsage"} {
		if _, present := row[key]; present {
			t.Errorf("%q was emitted for a row that has none: %v", key, row)
		}
	}
	usageJSON := row["usage"].(map[string]any)
	seven := usageJSON["sevenDay"].(map[string]any)
	for _, key := range []string{"pct", "resetsAt", "countdown", "clock"} {
		if _, present := seven[key]; !present {
			t.Errorf("the weekly window is missing %q: %v", key, seven)
		}
	}
	// Pace rides on the weekly window only. A five-hour window has no weekly
	// cycle to be ahead of.
	if _, present := seven["expectedPct"]; !present {
		t.Errorf("the weekly window carries no pace fields: %v", seven)
	}
	five := usageJSON["fiveHour"].(map[string]any)
	if _, present := five["expectedPct"]; present {
		t.Errorf("the five-hour window carries pace fields: %v", five)
	}
}

func TestSelectBestSwitchable(t *testing.T) {
	reset := ""
	tests := []struct {
		name     string
		usage    map[string]*usage.Result
		current  string
		want     string
		wantNote SelectionNote
	}{
		{
			name:    "the account with more headroom wins",
			usage:   map[string]*usage.Result{"1": measured(80, reset), "2": measured(20, reset)},
			current: "1", want: "2",
		},
		{
			// Never move onto something worse than where the user already is.
			name:    "a worse account is not recommended",
			usage:   map[string]*usage.Result{"1": measured(20, reset), "2": measured(80, reset)},
			current: "1", wantNote: NoteStay,
		},
		{
			// Ties resolve in favor of staying put.
			name:    "a tie stays",
			usage:   map[string]*usage.Result{"1": measured(50, reset), "2": measured(50, reset)},
			current: "1", wantNote: NoteStay,
		},
		{
			name:    "every account at its limit",
			usage:   map[string]*usage.Result{"1": measured(100, reset), "2": measured(100, reset)},
			current: "1", wantNote: NoteExhausted,
		},
		{
			// Cannot measure where the user is, so no target can be proven
			// better.
			name:    "the current account is unmeasurable",
			usage:   map[string]*usage.Result{"2": measured(20, reset)},
			current: "1", wantNote: NoteCurrentUnavailable,
		},
		{
			name:    "no other account has known usage",
			usage:   map[string]*usage.Result{"1": measured(80, reset)},
			current: "1", wantNote: NoteNoComparison,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.twoAccounts()
			outcomes := map[string]claudeapi.UsageOutcome{}
			for num := range map[string]bool{"1": true, "2": true} {
				if result, ok := tt.usage[num]; ok {
					outcomes[num] = claudeapi.UsageOutcome{Usage: result}
				} else {
					outcomes[num] = claudeapi.UsageOutcome{Error: claudeapi.KindTimeout}
				}
			}
			f.Fetcher = &fakeFetcher{byNumber: outcomes}

			got, note := f.SelectBestSwitchable(f.snapshot(), tt.current)
			if got != tt.want || note != tt.wantNote {
				t.Errorf("SelectBestSwitchable = (%q, %q), want (%q, %q)",
					got, note, tt.want, tt.wantNote)
			}
		})
	}

	t.Run("no other switchable account", func(t *testing.T) {
		f := newFixture(t)
		roster := f.seedAccounts(map[string]*Account{"1": {Email: "one@example.com"}})
		f.seedRoster(roster)
		f.measuring(map[string]*usage.Result{"1": measured(80, "")})

		_, note := f.SelectBestSwitchable(f.snapshot(), "1")
		if note != NoteNone {
			t.Errorf("note = %q, want %q", note, NoteNone)
		}
	})

	// "Everything is exhausted" is only claimable when every candidate is
	// known. Otherwise the honest answer is that the comparison is incomplete.
	t.Run("an unknown candidate withholds the exhausted claim", func(t *testing.T) {
		f := newFixture(t)
		roster := f.seedAccounts(map[string]*Account{
			"1": {Email: "one@example.com"},
			"2": {Email: "two@example.com"},
			"3": {Email: "three@example.com"},
		})
		f.seedRoster(roster)
		f.Fetcher = &fakeFetcher{byNumber: map[string]claudeapi.UsageOutcome{
			"1": {Usage: measured(100, "")},
			"2": {Usage: measured(100, "")},
			"3": {Error: claudeapi.KindTimeout},
		}}

		_, note := f.SelectBestSwitchable(f.snapshot(), "1")
		if note != NoteIncompleteComparison {
			t.Errorf("note = %q, want %q", note, NoteIncompleteComparison)
		}
	})

	// A disabled slot is held out of automatic selection while staying a valid
	// explicit target.
	t.Run("a disabled account is not selected", func(t *testing.T) {
		f := newFixture(t)
		roster := f.seedAccounts(map[string]*Account{
			"1": {Email: "one@example.com"},
			"2": {Email: "two@example.com", Disabled: true},
		})
		f.seedRoster(roster)
		f.measuring(map[string]*usage.Result{"1": measured(80, ""), "2": measured(10, "")})

		// With the only alternate held out of rotation there is no candidate
		// at all, which is a different answer from "nothing measurable".
		got, note := f.SelectBestSwitchable(f.snapshot(), "1")
		if got != "" || note != NoteNone {
			t.Errorf("SelectBestSwitchable = (%q, %q), want the disabled slot skipped", got, note)
		}
	})
}

// Two slots holding one credential is impossible by construction, so it means
// one was overwritten with the other's.
func TestDuplicateAccountWarnings(t *testing.T) {
	shared := `{"claudeAiOauth":{"accessToken":"a","refreshToken":"shared"}}`
	tests := []struct {
		name  string
		views []AccountView
		want  string
	}{
		{
			name: "the same credential lineage in two slots",
			views: []AccountView{
				{Number: "1", Account: &Account{Email: "one@example.com"}, Credentials: shared},
				{Number: "2", Account: &Account{Email: "two@example.com"}, Credentials: shared},
			},
			want: "hold the same credential",
		},
		{
			name: "the same account uuid recorded twice",
			views: []AccountView{
				{Number: "1", Account: &Account{Email: "one@example.com", UUID: "acct-1"}},
				{Number: "2", Account: &Account{Email: "one@example.com", UUID: "acct-1"}},
			},
			want: "both authenticate as",
		},
		{
			name: "different accounts are clean",
			views: []AccountView{
				{Number: "1", Account: &Account{Email: "one@example.com", UUID: "acct-1"},
					Credentials: `{"claudeAiOauth":{"refreshToken":"r1"}}`},
				{Number: "2", Account: &Account{Email: "two@example.com", UUID: "acct-2"},
					Credentials: `{"claudeAiOauth":{"refreshToken":"r2"}}`},
			},
		},
		{
			// Placeholders carry no uuid, and two absences are not a match.
			name: "two slots with no uuid do not collide",
			views: []AccountView{
				{Number: "1", Account: &Account{Email: "one@example.com"}},
				{Number: "2", Account: &Account{Email: "two@example.com"}},
			},
		},
		{
			// The same address across organizations is two real accounts.
			name: "the same uuid under different organizations",
			views: []AccountView{
				{Number: "1", Account: &Account{Email: "a@example.com", UUID: "acct-1"}},
				{Number: "2", Account: &Account{Email: "a@example.com", UUID: "acct-1", OrganizationUUID: "org-2"}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DuplicateAccountWarnings(tt.views)
			if tt.want == "" {
				if len(got) != 0 {
					t.Errorf("warnings = %v, want none", got)
				}
				return
			}
			if len(got) != 1 || !strings.Contains(got[0], tt.want) {
				t.Errorf("warnings = %v, want one mentioning %q", got, tt.want)
			}
		})
	}
}

// Two generations of one account carry different fingerprints and untouched
// roster identities, so only their usage gives them away.
func TestLockstepUsageWarnings(t *testing.T) {
	reset := "2026-06-20T00:00:00Z"
	identical := &usage.Result{
		FiveHour: &usage.Window{Pct: 20, ResetsAt: reset},
		SevenDay: &usage.Window{Pct: 61, ResetsAt: reset},
	}

	views := []AccountView{
		{Number: "1", Account: &Account{Email: "one@example.com"}},
		{Number: "2", Account: &Account{Email: "two@example.com"}},
	}
	entries := map[string]usagestore.Entry{
		"1": {LastGood: identical, FetchedAt: testNow, Age: time.Minute},
		"2": {LastGood: identical, FetchedAt: testNow, Age: time.Minute},
	}
	got := LockstepUsageWarnings(views, entries)
	if len(got) != 1 || !strings.Contains(got[0], "identical usage") {
		t.Errorf("warnings = %v, want one naming the lockstep", got)
	}

	// Two idle accounts with nothing scheduled are indistinguishable and must
	// never be flagged.
	t.Run("windows with no reset are not compared", func(t *testing.T) {
		idle := &usage.Result{
			FiveHour: &usage.Window{Pct: 0},
			SevenDay: &usage.Window{Pct: 0},
		}
		entries := map[string]usagestore.Entry{
			"1": {LastGood: idle, FetchedAt: testNow, Age: time.Minute},
			"2": {LastGood: idle, FetchedAt: testNow, Age: time.Minute},
		}
		if got := LockstepUsageWarnings(views, entries); len(got) != 0 {
			t.Errorf("warnings = %v, want none for two idle accounts", got)
		}
	})

	t.Run("different usage is clean", func(t *testing.T) {
		entries := map[string]usagestore.Entry{
			"1": {LastGood: identical, FetchedAt: testNow, Age: time.Minute},
			"2": {LastGood: &usage.Result{
				FiveHour: &usage.Window{Pct: 5, ResetsAt: reset},
				SevenDay: &usage.Window{Pct: 9, ResetsAt: reset},
			}, FetchedAt: testNow, Age: time.Minute},
		}
		if got := LockstepUsageWarnings(views, entries); len(got) != 0 {
			t.Errorf("warnings = %v, want none", got)
		}
	})
}

// The measurements every surface acts on come from one collect, so a listing
// and a switch decision made together cannot disagree.
func TestUsageByAccount(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	f.measuring(map[string]*usage.Result{"1": measured(40, ""), "2": measured(10, "")})

	snapshot := f.snapshot()
	got := UsageByAccount(snapshot.Entries)
	if len(got) != 2 {
		t.Fatalf("UsageByAccount = %v", got)
	}
	if got["1"].SevenDay.Pct != 40 || got["2"].SevenDay.Pct != 10 {
		t.Errorf("UsageByAccount = %+v", got)
	}
	if !reflect.DeepEqual(got["1"], snapshot.Entries["1"].LastGood) {
		t.Error("the decision value and the stored measurement diverged")
	}
}
