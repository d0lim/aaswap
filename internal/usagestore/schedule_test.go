package usagestore

import (
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/pollpolicy"
)

func TestDueCandidatePicksTheStalest(t *testing.T) {
	now := base
	entries := map[string]Entry{
		"1": {FetchedAt: now.Add(-10 * time.Minute), NextPollAt: now.Add(-time.Minute)},
		"2": {FetchedAt: now.Add(-time.Hour), NextPollAt: now.Add(-time.Minute)},
		"3": {FetchedAt: now.Add(-5 * time.Minute), NextPollAt: now.Add(-time.Minute)},
	}
	got, ok := DueCandidate([]string{"1", "2", "3"}, entries, now)
	if !ok || got != "2" {
		t.Errorf("DueCandidate = (%q, %v), want slot 2", got, ok)
	}
}

// Nothing is staler than nothing: an account never measured sorts ahead of one
// that merely has old data.
func TestDueCandidatePrefersAnAccountNeverFetched(t *testing.T) {
	now := base
	entries := map[string]Entry{
		"1": {FetchedAt: now.Add(-24 * time.Hour), NextPollAt: now.Add(-time.Minute)},
		"2": {NextPollAt: now.Add(-time.Minute)},
	}
	if got, _ := DueCandidate([]string{"1", "2"}, entries, now); got != "2" {
		t.Errorf("DueCandidate = %q, want the never-fetched slot 2", got)
	}
	// A slot with no row at all is the same case.
	if got, _ := DueCandidate([]string{"1", "9"}, entries, now); got != "9" {
		t.Errorf("DueCandidate = %q, want the slot with no row", got)
	}
}

func TestDueCandidateSkips(t *testing.T) {
	now := base
	due := Entry{FetchedAt: now.Add(-time.Hour), NextPollAt: now.Add(-time.Minute)}

	tests := []struct {
		name  string
		entry Entry
		why   string
	}{
		{
			name:  "a sentinel account",
			entry: Entry{Sentinel: "api key", FetchedAt: now.Add(-24 * time.Hour)},
			why:   "there is nothing to fetch",
		},
		{
			name:  "a quarantined account",
			entry: Entry{AuthDeadStrikes: AuthDeadStrikes, FetchedAt: now.Add(-24 * time.Hour)},
			why:   "a dead refresh lineage needs a re-login, not a request",
		},
		{
			name:  "an account in failure backoff",
			entry: Entry{BackoffUntil: now.Add(time.Minute), FetchedAt: now.Add(-24 * time.Hour)},
			why:   "its backoff removes it from the due set between attempts",
		},
		{
			name:  "an account not yet due",
			entry: Entry{NextPollAt: now.Add(time.Minute), FetchedAt: now.Add(-24 * time.Hour), PollInterval: 5 * time.Minute},
			why:   "the scheduler chose its cadence",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entries := map[string]Entry{"1": tt.entry, "2": due}
			got, ok := DueCandidate([]string{"1", "2"}, entries, now)
			if !ok || got != "2" {
				t.Errorf("DueCandidate = (%q, %v), want slot 2 because %s", got, ok, tt.why)
			}
			// And with nothing else to pick, none is due at all.
			if _, ok := DueCandidate([]string{"1"}, map[string]Entry{"1": tt.entry}, now); ok {
				t.Errorf("DueCandidate picked an account it should skip because %s", tt.why)
			}
		})
	}
}

// A perpetually failing account cannot monopolize the slot, but must come back
// once its backoff lifts.
func TestABackedOffAccountReturnsWhenItLifts(t *testing.T) {
	now := base
	entry := Entry{BackoffUntil: now.Add(time.Minute), FetchedAt: now.Add(-time.Hour), NextPollAt: now.Add(-time.Minute)}
	entries := map[string]Entry{"1": entry}

	if _, ok := DueCandidate([]string{"1"}, entries, now); ok {
		t.Fatal("a backed-off account was due")
	}
	if _, ok := DueCandidate([]string{"1"}, entries, now.Add(2*time.Minute)); !ok {
		t.Error("the account did not return once its backoff lifted")
	}
}

// A ranking that depended on map iteration order would make the choice
// unpredictable across passes.
func TestDueCandidateIsReproducible(t *testing.T) {
	now := base
	entries := map[string]Entry{}
	var candidates []string
	for _, num := range []string{"1", "2", "3", "4", "5"} {
		entries[num] = Entry{FetchedAt: now.Add(-time.Hour), NextPollAt: now.Add(-time.Minute)}
		candidates = append(candidates, num)
	}
	first, _ := DueCandidate(candidates, entries, now)
	for range 20 {
		if got, _ := DueCandidate(candidates, entries, now); got != first {
			t.Fatalf("DueCandidate returned %q then %q for identical input", first, got)
		}
	}
}

// Reset-parking used to store a distant deadline while keeping the short
// learned interval, stranding an otherwise usable account. The shape is
// impossible for the bounded planner to produce, so it is detected structurally.
func TestPlanOversleeps(t *testing.T) {
	now := base
	tests := []struct {
		name       string
		nextPollAt time.Time
		interval   time.Duration
		want       bool
	}{
		{"no plan at all", time.Time{}, 0, false},
		{"a normal candidate plan", now.Add(5 * time.Minute), 5 * time.Minute, false},
		{"a plan at the widest the planner produces", now.Add(pollpolicy.CandidateMaxInterval), pollpolicy.CandidateMaxInterval, false},
		// A day out on a five-minute interval cannot have come from the planner.
		{"a reset-parked plan", now.Add(24 * time.Hour), 5 * time.Minute, true},
		// The interval floor means a plan with no recorded interval is still
		// judged against the widest decay ceiling, not against zero.
		{"a parked plan with no interval", now.Add(24 * time.Hour), 0, true},
		{"a plan just inside the jitter allowance", now.Add(pollpolicy.ExhaustedInterval), 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := Entry{NextPollAt: tt.nextPollAt, PollInterval: tt.interval}
			if got := PlanOversleeps(entry, now); got != tt.want {
				t.Errorf("PlanOversleeps = %v, want %v", got, tt.want)
			}
		})
	}
}

// A parked plan is due despite its deadline, so changing the scoped models
// cannot leave an account stranded until an old reset.
func TestAParkedPlanIsDueAgain(t *testing.T) {
	now := base
	entry := Entry{
		FetchedAt:    now.Add(-time.Hour),
		NextPollAt:   now.Add(24 * time.Hour),
		PollInterval: 5 * time.Minute,
	}
	if _, ok := DueCandidate([]string{"1"}, map[string]Entry{"1": entry}, now); !ok {
		t.Error("an account parked by an impossible plan was not due")
	}
}

func TestReserveModes(t *testing.T) {
	tests := []struct {
		name string
		// age is how long ago the last successful measurement was.
		age     time.Duration
		planIn  time.Duration // when the plan says to poll next, relative to now
		hasPlan bool
		mode    Mode
		wantWon bool
	}{
		{
			// An on-demand surface repainting often must not out-vote the
			// scheduler's cadence: it needs BOTH stale and due.
			name: "on-demand: fresh and due does not win",
			age:  time.Minute, planIn: -time.Minute, hasPlan: true,
			mode: Mode{RespectPlans: true}, wantWon: false,
		},
		{
			name: "on-demand: stale but not due does not win",
			age:  time.Hour, planIn: time.Minute, hasPlan: true,
			mode: Mode{RespectPlans: true}, wantWon: false,
		},
		{
			name: "on-demand: stale and due wins",
			age:  time.Hour, planIn: -time.Minute, hasPlan: true,
			mode: Mode{RespectPlans: true}, wantWon: true,
		},
		{
			name: "on-demand: stale with no plan yet wins",
			age:  time.Hour, hasPlan: false,
			mode: Mode{RespectPlans: true}, wantWon: true,
		},
		{
			// The engine's schedule IS the deliberate one, so a due entry may
			// be re-fetched inside the serve TTL — that is how the bounded
			// urgent cadence beats the TTL.
			name: "the engine: due beats the serve TTL",
			age:  time.Minute, planIn: -time.Minute, hasPlan: true,
			mode: Mode{}, wantWon: true,
		},
		{
			// An escalation refresh may fetch a not-yet-due candidate.
			name: "the engine: stale wins even when not due",
			age:  time.Hour, planIn: time.Minute, hasPlan: true,
			mode: Mode{}, wantWon: true,
		},
		{
			name: "the engine: fresh and not due does not win",
			age:  time.Minute, planIn: time.Minute, hasPlan: true,
			mode: Mode{}, wantWon: false,
		},
		{
			// The non-escalating scheduler mode: due plans win, but a valid
			// future plan does not, even for a stale entry.
			name: "the scheduler: a valid future plan is respected",
			age:  time.Hour, planIn: time.Minute, hasPlan: true,
			mode: Mode{RepairOverslept: true}, wantWon: false,
		},
		{
			name: "the scheduler: a due plan wins",
			age:  time.Minute, planIn: -time.Minute, hasPlan: true,
			mode: Mode{RepairOverslept: true}, wantWon: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.record("1", FetchRecord{Usage: measurement(20)})
			f.advance(tt.age)
			if tt.hasPlan {
				plan := pollpolicy.Plan{NextPollAt: f.now.Add(tt.planIn), Interval: 5 * time.Minute}
				if err := f.store.SetPollPlan(map[string]pollpolicy.Plan{"1": plan}, f.ids); err != nil {
					t.Fatal(err)
				}
			}

			won, err := f.store.Reserve([]string{"1"}, f.ids, tt.mode)
			if err != nil {
				t.Fatal(err)
			}
			if _, got := won["1"]; got != tt.wantWon {
				t.Errorf("won = %v, want %v", got, tt.wantWon)
			}
		})
	}
}

// A structurally impossible plan is repaired under the write lock too, not just
// in the lock-free ranking.
func TestReserveRepairsAnOversleptPlan(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Usage: measurement(20)})
	f.advance(time.Hour)
	if err := f.store.SetPollPlan(map[string]pollpolicy.Plan{
		"1": {NextPollAt: f.now.Add(24 * time.Hour), Interval: 5 * time.Minute},
	}, f.ids); err != nil {
		t.Fatal(err)
	}

	if won, _ := f.store.Reserve([]string{"1"}, f.ids, Mode{RespectPlans: true}); len(won) != 0 {
		t.Error("a parked plan was honored when repair was not asked for")
	}
	won, err := f.store.Reserve([]string{"1"}, f.ids, Mode{RespectPlans: true, RepairOverslept: true})
	if err != nil {
		t.Fatal(err)
	}
	if won["1"] == "" {
		t.Error("a parked plan was not repaired")
	}
}

// A quarantined account is not fetched at all: each retry with a dead token
// just draws a fresh 401.
func TestReserveRefusesAQuarantinedAccount(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Error: claudeapi.KindInvalidGrant, StruckFP: "sha256:dead"})
	f.advance(2 * BackoffCap)

	if won, _ := f.store.Reserve([]string{"1"}, f.ids, Mode{}); len(won) != 0 {
		t.Error("a quarantined account was reserved for a fetch")
	}
	if !f.entry("1").TokenDead("") {
		t.Error("the account was not quarantined by a permanent verdict")
	}
}

// Strikes condemn the generation that was POSTed, not the slot. Any path that
// writes a credential therefore heals the quarantine with no bespoke call.
func TestAStrikeBindsToItsGeneration(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Error: claudeapi.KindInvalidGrant, StruckFP: "sha256:dead"})
	entry := f.entry("1")

	if !entry.TokenDead("sha256:dead") {
		t.Error("the struck generation is not reported dead")
	}
	if entry.TokenDead("sha256:replacement") {
		t.Error("a replacement credential inherited the strike")
	}
	// Asking the unconditional question — no credential in hand — still gets a
	// yes, which is what the scheduler asks.
	if !entry.TokenDead("") {
		t.Error("the unconditional question was answered no")
	}
}

// A row struck before fingerprints were recorded binds unconditionally: there
// is no generation to compare against, and treating that as "healed" would
// silently lift every legacy quarantine.
func TestALegacyStrikeBindsUnconditionally(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Error: claudeapi.KindInvalidGrant})

	entry := f.entry("1")
	if entry.StruckFingerprint != "" {
		t.Fatalf("struckFingerprint = %q, want none recorded", entry.StruckFingerprint)
	}
	if !entry.TokenDead("sha256:anything") {
		t.Error("a strike with no recorded generation was treated as healed")
	}
}

// A strike must not inherit a stale fingerprint from an earlier, already-healed
// one.
func TestAFingerprintlessStrikeOverwritesAnOlderOne(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Error: claudeapi.KindInvalidGrant, StruckFP: "sha256:first"})
	f.advance(time.Hour)
	f.record("1", FetchRecord{Error: claudeapi.KindInvalidGrant})

	if got := f.entry("1").StruckFingerprint; got != "" {
		t.Errorf("struckFingerprint = %q, want it cleared by the unconditional strike", got)
	}
}

// Only a permanent verdict advances the strike count. A transient error is no
// evidence either way and must not reset a real tally.
func TestOnlyPermanentVerdictsStrike(t *testing.T) {
	tests := []struct {
		kind       claudeapi.ErrorKind
		wantStrike bool
	}{
		{claudeapi.KindInvalidGrant, true},
		{claudeapi.KindNoRefreshToken, true},
		{claudeapi.KindTransient, false},
		{claudeapi.KindInvalidClient, false},
		{claudeapi.KindTimeout, false},
		{claudeapi.KindNetwork, false},
		{claudeapi.HTTPKind(429), false},
		{claudeapi.HTTPKind(500), false},
		{claudeapi.KindConsumeBusy, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			f := newFixture(t)
			f.record("1", FetchRecord{Error: tt.kind})
			if got := f.entry("1").AuthDeadStrikes > 0; got != tt.wantStrike {
				t.Errorf("struck = %v, want %v", got, tt.wantStrike)
			}
		})
	}

	t.Run("a transient error does not reset a real tally", func(t *testing.T) {
		f := newFixture(t)
		f.record("1", FetchRecord{Error: claudeapi.KindInvalidGrant, StruckFP: "sha256:dead"})
		f.advance(2 * BackoffCap)
		f.record("1", FetchRecord{Error: claudeapi.KindTimeout})

		entry := f.entry("1")
		if entry.AuthDeadStrikes != 1 {
			t.Errorf("strikes = %d, want the tally kept at 1", entry.AuthDeadStrikes)
		}
		if entry.StruckFingerprint != "sha256:dead" {
			t.Errorf("struckFingerprint = %q, want it kept", entry.StruckFingerprint)
		}
	})
}

// After a re-login the strike count and the failure state riding with it no
// longer reflect reality, and the account has to become fetch-eligible so the
// next pass can prove the new token good.
func TestClearDeadTokenLiftsTheQuarantine(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Error: claudeapi.KindInvalidGrant, StruckFP: "sha256:dead"})

	if err := f.store.ClearDeadToken([]string{"1"}, f.ids); err != nil {
		t.Fatal(err)
	}

	entry := f.entry("1")
	if entry.TokenDead("") {
		t.Error("the quarantine survived a credential rewrite")
	}
	if entry.ConsecutiveFailures != 0 || entry.LastError != "" || !entry.BackoffUntil.IsZero() {
		t.Errorf("the failure state riding with the strike survived: %+v", entry)
	}
	if won, _ := f.store.Reserve([]string{"1"}, f.ids, Mode{}); won["1"] == "" {
		t.Error("the account did not become fetch-eligible again")
	}
}

// A no-op for a row with no strikes: clearing must not blank a healthy account's
// measurement.
func TestClearDeadTokenLeavesAHealthyRowAlone(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Usage: measurement(40)})
	before := f.entry("1")

	if err := f.store.ClearDeadToken([]string{"1"}, f.ids); err != nil {
		t.Fatal(err)
	}
	after := f.entry("1")

	if after.LastGood == nil || after.LastGood.SevenDay.Pct != 40 {
		t.Errorf("lastGood = %+v, want it untouched", after.LastGood)
	}
	if !after.FetchedAt.Equal(before.FetchedAt) {
		t.Errorf("fetchedAt moved: %v -> %v", before.FetchedAt, after.FetchedAt)
	}
}

// The plan commits in the same transaction as the measurement, so no collector
// can slip into a gap between recording and re-planning and fetch again at once.
func TestASuccessPlanCommitsWithItsMeasurement(t *testing.T) {
	f := newFixture(t)
	claims, err := f.store.Reserve([]string{"1"}, f.ids, Mode{})
	if err != nil {
		t.Fatal(err)
	}
	plan := pollpolicy.Plan{NextPollAt: f.now.Add(5 * time.Minute), Interval: 3 * time.Minute}
	if _, err := f.store.Record(
		map[string]FetchRecord{"1": {Usage: measurement(30)}},
		f.ids, claims, map[string]pollpolicy.Plan{"1": plan},
	); err != nil {
		t.Fatal(err)
	}

	entry := f.entry("1")
	if !entry.NextPollAt.Equal(plan.NextPollAt) || entry.PollInterval != plan.Interval {
		t.Errorf("plan = (%v, %v), want (%v, %v)",
			entry.NextPollAt, entry.PollInterval, plan.NextPollAt, plan.Interval)
	}
	// And the slot is not immediately due again.
	if won, _ := f.store.Reserve([]string{"1"}, f.ids, Mode{}); len(won) != 0 {
		t.Error("the slot was due again right after recording its plan")
	}
}

// A failure must not adopt a plan: the plan describes the cadence after a
// successful measurement, and the backoff governs a failure.
func TestAFailureIgnoresASuppliedPlan(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Usage: measurement(30)})
	before := f.entry("1")

	f.advance(pollpolicy.ServeTTL + time.Minute)
	claims, err := f.store.Reserve([]string{"1"}, f.ids, Mode{})
	if err != nil {
		t.Fatal(err)
	}
	if claims["1"] == "" {
		t.Fatal("the fetch was not reserved")
	}
	accepted, err := f.store.Record(
		map[string]FetchRecord{"1": {Error: claudeapi.KindTimeout}},
		f.ids, claims,
		map[string]pollpolicy.Plan{"1": {NextPollAt: f.now.Add(time.Hour), Interval: time.Hour}},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted["1"] {
		t.Fatal("the outcome was rejected")
	}

	after := f.entry("1")
	if !after.NextPollAt.Equal(before.NextPollAt) {
		t.Errorf("nextPollAt = %v, want it unmoved at %v", after.NextPollAt, before.NextPollAt)
	}
	if !after.InBackoff(f.now) {
		t.Error("the failure installed no backoff")
	}
}

// Omitting the plan map entirely leaves the stored cadence alone, which is what
// an on-demand caller with no opinion about scheduling needs.
func TestRecordingWithNoPlanLeavesTheCadence(t *testing.T) {
	f := newFixture(t)
	plan := pollpolicy.Plan{NextPollAt: base.Add(time.Hour), Interval: 10 * time.Minute}
	if err := f.store.SetPollPlan(map[string]pollpolicy.Plan{"1": plan}, f.ids); err != nil {
		t.Fatal(err)
	}

	f.record("1", FetchRecord{Usage: measurement(30)})

	entry := f.entry("1")
	if !entry.NextPollAt.Equal(plan.NextPollAt) || entry.PollInterval != plan.Interval {
		t.Errorf("plan = (%v, %v), want it preserved", entry.NextPollAt, entry.PollInterval)
	}
}

// A zero plan clears the cadence, which makes the slot immediately due.
func TestAZeroPlanClearsTheCadence(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Usage: measurement(30)})
	if err := f.store.SetPollPlan(map[string]pollpolicy.Plan{
		"1": {NextPollAt: base.Add(time.Hour), Interval: time.Hour},
	}, f.ids); err != nil {
		t.Fatal(err)
	}
	if err := f.store.SetPollPlan(map[string]pollpolicy.Plan{"1": {}}, f.ids); err != nil {
		t.Fatal(err)
	}

	entry := f.entry("1")
	if !entry.NextPollAt.IsZero() || entry.PollInterval != 0 {
		t.Errorf("plan = (%v, %v), want it cleared", entry.NextPollAt, entry.PollInterval)
	}
}
