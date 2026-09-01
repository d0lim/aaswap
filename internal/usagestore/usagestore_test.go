package usagestore

import (
	json "encoding/json/v2"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/pollpolicy"
	"github.com/d0lim/aaswap/internal/usage"
)

var base = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// fixture is a store over a temp directory with a clock the test drives.
type fixture struct {
	// Not embedded: every call site names the store, so a test reads as
	// "the store does X" rather than "the fixture does X".
	store *Store
	t     *testing.T
	now   time.Time
	seq   int
	dir   string
	ids   map[string]Identity
	acct  Identity
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{t: t, now: base, dir: dir, acct: Identity{Email: "one@example.com"}}
	f.ids = map[string]Identity{"1": f.acct}
	f.store = New(dir)
	f.store.Now = func() time.Time { return f.now }
	// Sequential rather than random, so a failed assertion names a token a
	// human can find in the file.
	f.store.NewClaimID = func() string { f.seq++; return fmt.Sprintf("claim-%d", f.seq) }
	return f
}

func (f *fixture) advance(d time.Duration) { f.now = f.now.Add(d) }

func (f *fixture) entry(num string) Entry {
	f.t.Helper()
	return f.store.Entries(f.ids, nil)[num]
}

// record writes one outcome unfenced, failing the test if it is not accepted.
func (f *fixture) record(num string, rec FetchRecord) {
	f.t.Helper()
	accepted, err := f.store.Record(map[string]FetchRecord{num: rec}, f.ids, nil, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	if !accepted[num] {
		f.t.Fatalf("the outcome for slot %s was rejected", num)
	}
}

func measurement(pct float64) *usage.Result {
	return &usage.Result{SevenDay: &usage.Window{Pct: pct}}
}

// raw reads the stored row for a slot, so a test can assert on the on-disk
// shape rather than only on the read model.
func (f *fixture) raw(num string) map[string]any {
	f.t.Helper()
	data, err := os.ReadFile(f.store.Path())
	if err != nil {
		f.t.Fatal(err)
	}
	var parsed struct {
		SchemaVersion int                       `json:"schemaVersion"`
		Accounts      map[string]map[string]any `json:"accounts"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		f.t.Fatal(err)
	}
	if parsed.SchemaVersion != SchemaVersion {
		f.t.Fatalf("schemaVersion = %d, want %d", parsed.SchemaVersion, SchemaVersion)
	}
	return parsed.Accounts[num]
}

func TestAnEmptyStoreReportsEmptyEntries(t *testing.T) {
	f := newFixture(t)
	entries := f.store.Entries(f.ids, nil)
	if len(entries) != 1 {
		t.Fatalf("entries = %v, want one per requested slot", entries)
	}
	entry := entries["1"]
	if entry.LastGood != nil || !entry.FetchedAt.IsZero() {
		t.Errorf("entry = %+v, want the zero value", entry)
	}
	if _, known := entry.DecisionValue(); known {
		t.Error("an empty entry reported a decision value")
	}
}

// The cache is regenerable, so an unreadable file costs one round of fetching
// and nothing else. Refusing to start because a throwaway file is malformed
// would be strictly worse.
func TestAnUnusableFileReadsAsEmpty(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"malformed JSON", `{"schemaVersion":2,"accounts":`},
		{"not an object", `[1,2,3]`},
		// A version-less snapshot is the legacy 15-second cache; its data had
		// no shelf life anyway.
		{"no schema version", `{"accounts":{"1":{"email":"one@example.com","fetchedAt":1}}}`},
		{"a past version", `{"schemaVersion":1,"accounts":{"1":{"email":"one@example.com","fetchedAt":1}}}`},
		// A future version must not be reinterpreted by guesswork.
		{"a future version", `{"schemaVersion":99,"accounts":{"1":{"email":"one@example.com","fetchedAt":1}}}`},
		{"empty", ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			if err := os.WriteFile(f.store.Path(), []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := f.entry("1"); got.LastGood != nil {
				t.Errorf("entry = %+v, want empty", got)
			}
		})
	}
}

// A row whose stored identity differs is invisible, so reusing a slot number
// never serves the previous account's usage.
func TestARowIsGuardedByItsIdentity(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Usage: measurement(40)})
	if got := f.entry("1"); got.LastGood == nil {
		t.Fatal("the measurement was not stored")
	}

	tests := []struct {
		name string
		id   Identity
	}{
		{"a different email", Identity{Email: "two@example.com"}},
		{"a different organization", Identity{Email: "one@example.com", OrganizationUUID: "org-2"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := f.store.Entries(map[string]Identity{"1": tt.id}, nil)
			if entries["1"].LastGood != nil {
				t.Errorf("slot 1 served the previous account's usage: %+v", entries["1"])
			}
		})
	}
}

// A slot outside the caller's map is left alone: the status command operates on
// one account and must not disturb the rest of the table.
func TestSlotsOutsideTheMapAreUntouched(t *testing.T) {
	f := newFixture(t)
	two := Identity{Email: "two@example.com"}
	both := map[string]Identity{"1": f.acct, "2": two}

	if _, err := f.store.Record(map[string]FetchRecord{
		"1": {Usage: measurement(10)},
		"2": {Usage: measurement(90)},
	}, both, nil, nil); err != nil {
		t.Fatal(err)
	}

	// A write naming only slot 1.
	f.advance(time.Hour)
	f.record("1", FetchRecord{Error: claudeapi.KindTimeout})

	entries := f.store.Entries(both, nil)
	if entries["2"].LastGood == nil || entries["2"].LastGood.SevenDay.Pct != 90 {
		t.Errorf("slot 2 was disturbed by a write naming only slot 1: %+v", entries["2"])
	}
}

// Stale-on-error: one failed round trip must not blank the measurement. The
// whole reason this is a table and not a snapshot.
func TestAFailureNeverTouchesTheMeasurement(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Usage: measurement(40)})
	fetchedAt := f.entry("1").FetchedAt

	f.advance(time.Minute)
	f.record("1", FetchRecord{Error: claudeapi.KindTimeout})

	got := f.entry("1")
	if got.LastGood == nil || got.LastGood.SevenDay.Pct != 40 {
		t.Errorf("lastGood = %+v, want the measurement preserved", got.LastGood)
	}
	if !got.FetchedAt.Equal(fetchedAt) {
		t.Errorf("fetchedAt = %v, want it unmoved at %v", got.FetchedAt, fetchedAt)
	}
	if got.LastError != claudeapi.KindTimeout || got.ConsecutiveFailures != 1 {
		t.Errorf("failure state = (%q, %d)", got.LastError, got.ConsecutiveFailures)
	}
}

// A success is the other half of the same rule: it clears every failure field,
// so a recovered account carries no residue.
func TestASuccessClearsTheFailureState(t *testing.T) {
	f := newFixture(t)
	for range 3 {
		f.record("1", FetchRecord{Error: claudeapi.KindNetwork})
		f.advance(BackoffCap)
	}
	f.record("1", FetchRecord{Error: claudeapi.KindInvalidGrant, StruckFP: "sha256:dead"})
	f.advance(BackoffCap)

	before := f.entry("1")
	if before.ConsecutiveFailures == 0 || before.AuthDeadStrikes == 0 {
		t.Fatalf("the failure state was not built up: %+v", before)
	}

	f.record("1", FetchRecord{Usage: measurement(15)})
	got := f.entry("1")
	if got.ConsecutiveFailures != 0 || got.LastError != "" || !got.BackoffUntil.IsZero() {
		t.Errorf("failure state survived a success: %+v", got)
	}
	// A success proves the token is alive, whatever it was struck for.
	if got.AuthDeadStrikes != 0 || got.StruckFingerprint != "" {
		t.Errorf("the quarantine survived a success: strikes=%d fp=%q",
			got.AuthDeadStrikes, got.StruckFingerprint)
	}
	if got.TokenDead("") {
		t.Error("the account is still quarantined after a successful fetch")
	}
}

// A 429 stamp is deliberately NOT cleared by a later success: the planner keeps
// the cadence floored until the saturated rolling window has aged out.
func TestThe429StampSurvivesASuccess(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Error: claudeapi.HTTPKind(429)})
	stamped := f.entry("1").Last429At
	if stamped.IsZero() {
		t.Fatal("a 429 left no stamp")
	}

	f.advance(10 * time.Minute)
	f.record("1", FetchRecord{Usage: measurement(20)})

	got := f.entry("1")
	if !got.Last429At.Equal(stamped) {
		t.Errorf("last429At = %v, want it kept at %v", got.Last429At, stamped)
	}
	if !got.Recent429(f.now) {
		t.Error("Recent429 went false right after a success inside the window")
	}
}

// Sentinels are re-derived every pass. Persisting one would let it outlive the
// condition that produced it.
func TestASentinelIsNotPersisted(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Usage: measurement(40)})
	f.advance(time.Minute)

	f.record("1", FetchRecord{Sentinel: "api key"})

	got := f.entry("1")
	if got.Sentinel != "" {
		t.Errorf("sentinel = %q, want nothing stored", got.Sentinel)
	}
	// And it disturbs nothing else: it says nothing about whether a fetch
	// succeeded.
	if got.LastGood == nil || got.LastGood.SevenDay.Pct != 40 {
		t.Errorf("lastGood = %+v, want it untouched", got.LastGood)
	}
	if got.ConsecutiveFailures != 0 {
		t.Errorf("consecutiveFailures = %d, want a sentinel to count as neither", got.ConsecutiveFailures)
	}
	if _, present := f.raw("1")["sentinel"]; present {
		t.Error("a sentinel reached the file")
	}
}

func TestWithSentinelIsAReadModelOverlay(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Usage: measurement(40)})
	stored := f.entry("1")

	overlaid := WithSentinel(stored, "token expired")
	if overlaid.Sentinel != "token expired" {
		t.Errorf("sentinel = %q", overlaid.Sentinel)
	}
	// The sentinel decides on its own, ahead of any measurement.
	decision, known := overlaid.DecisionValue()
	if !known || decision.Sentinel != "token expired" || decision.Usage != nil {
		t.Errorf("decision = %+v (known=%v), want the sentinel alone", decision, known)
	}
	// An empty sentinel is not an overlay.
	if WithSentinel(stored, "").Sentinel != "" {
		t.Error("an empty sentinel was applied as an overlay")
	}
	// And the stored entry is unchanged.
	if stored.Sentinel != "" {
		t.Error("WithSentinel mutated the entry it was given")
	}
}

// The two implementations read each other's table during the migration, so the
// spelling of every field is a contract.
func TestTheOnDiskShapeIsStable(t *testing.T) {
	f := newFixture(t)
	claims, err := f.store.Reserve([]string{"1"}, f.ids, Mode{})
	if err != nil {
		t.Fatal(err)
	}
	plan := pollpolicy.Plan{NextPollAt: f.now.Add(5 * time.Minute), Interval: 300 * time.Second}
	if _, err := f.store.Record(
		map[string]FetchRecord{"1": {Usage: &usage.Result{
			FiveHour: &usage.Window{Pct: 12.5, ResetsAt: "2026-06-15T18:00:00Z"},
		}}},
		f.ids, claims, map[string]pollpolicy.Plan{"1": plan},
	); err != nil {
		t.Fatal(err)
	}

	row := f.raw("1")
	for _, key := range []string{"email", "lastGood", "fetchedAt", "lastAttemptAt", "nextPollAt", "pollIntervalS"} {
		if _, present := row[key]; !present {
			t.Errorf("%q is missing from the stored row: %v", key, row)
		}
	}
	if row["email"] != "one@example.com" {
		t.Errorf("email = %v", row["email"])
	}
	// Timestamps are fractional epoch seconds, not milliseconds and not a
	// formatted string.
	if got, want := row["fetchedAt"], float64(f.now.Unix()); got != want {
		t.Errorf("fetchedAt = %v, want %v", got, want)
	}
	if got, want := row["pollIntervalS"], 300.0; got != want {
		t.Errorf("pollIntervalS = %v, want %v", got, want)
	}
	lastGood := row["lastGood"].(map[string]any)
	five := lastGood["five_hour"].(map[string]any)
	if five["pct"] != 12.5 || five["resets_at"] != "2026-06-15T18:00:00Z" {
		t.Errorf("five_hour = %v", five)
	}
}

// The table lives in the cache directory, which is throwaway by construction.
func TestTheStoreLivesInTheCacheDirectory(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if got, want := s.Path(), filepath.Join(dir, "usage.json"); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}
