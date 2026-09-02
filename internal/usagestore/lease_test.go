package usagestore

import (
	"sync"
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/pollpolicy"
)

// Deciding eligibility on a lock-free read and then claiming separately lets
// two collectors both pass the check and both fetch. Reserve closes that window
// by doing both in one locked pass.
func TestReserveIsWonByExactlyOneCollector(t *testing.T) {
	f := newFixture(t)

	first, err := f.store.Reserve([]string{"1"}, f.ids, Mode{})
	if err != nil {
		t.Fatal(err)
	}
	if first["1"] == "" {
		t.Fatal("the first collector did not win an unfetched slot")
	}

	second, err := f.store.Reserve([]string{"1"}, f.ids, Mode{})
	if err != nil {
		t.Fatal(err)
	}
	if _, won := second["1"]; won {
		t.Error("a second collector won a slot already leased")
	}
}

// A crashed claimer's lease must age out rather than block the account forever.
func TestALeaseExpires(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.Reserve([]string{"1"}, f.ids, Mode{}); err != nil {
		t.Fatal(err)
	}

	f.advance(ClaimTTL - time.Second)
	if won, _ := f.store.Reserve([]string{"1"}, f.ids, Mode{}); len(won) != 0 {
		t.Error("a live lease was reclaimed early")
	}

	f.advance(2 * time.Second)
	won, err := f.store.Reserve([]string{"1"}, f.ids, Mode{})
	if err != nil {
		t.Fatal(err)
	}
	if won["1"] == "" {
		t.Error("an expired lease was not reclaimed")
	}
}

// Recording releases the lease, so the next pass can fetch again without
// waiting out the TTL.
func TestRecordingReleasesTheLease(t *testing.T) {
	f := newFixture(t)
	claims, err := f.store.Reserve([]string{"1"}, f.ids, Mode{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Record(map[string]FetchRecord{"1": {Usage: measurement(10)}}, f.ids, claims, nil); err != nil {
		t.Fatal(err)
	}

	if f.entry("1").Claimed(f.now) {
		t.Error("the lease survived the outcome that produced it")
	}
	// A released lease is stored as an explicit zero, not omitted: an absent
	// value would send the liveness check down its legacy path and keep the row
	// looking claimed for LegacyClaimTTL.
	claimUntil, present := f.raw("1")["claimUntil"]
	if !present {
		t.Fatal("claimUntil was omitted on release; a legacy reader would see the row as claimed")
	}
	if claimUntil != 0.0 {
		t.Errorf("claimUntil = %v, want an explicit 0", claimUntil)
	}
}

// A late writer whose lease was replaced must not clobber the newer row.
func TestAStaleLeaseCannotWrite(t *testing.T) {
	f := newFixture(t)
	stale, err := f.store.Reserve([]string{"1"}, f.ids, Mode{})
	if err != nil {
		t.Fatal(err)
	}

	// The lease ages out and another collector takes over and records.
	f.advance(ClaimTTL + time.Second)
	fresh, err := f.store.Reserve([]string{"1"}, f.ids, Mode{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Record(map[string]FetchRecord{"1": {Usage: measurement(75)}}, f.ids, fresh, nil); err != nil {
		t.Fatal(err)
	}

	// The original collector's result finally lands.
	accepted, err := f.store.Record(map[string]FetchRecord{"1": {Usage: measurement(10)}}, f.ids, stale, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accepted["1"] {
		t.Error("a stale lease's write was accepted")
	}
	if got := f.entry("1"); got.LastGood.SevenDay.Pct != 75 {
		t.Errorf("lastGood = %v, want the newer collector's 75", got.LastGood.SevenDay.Pct)
	}
}

// A fenced write also has to still be the same account: a slot that changed
// hands must not receive the previous account's measurement.
func TestAFencedWriteIsAlsoGuardedByIdentity(t *testing.T) {
	f := newFixture(t)
	claims, err := f.store.Reserve([]string{"1"}, f.ids, Mode{})
	if err != nil {
		t.Fatal(err)
	}

	replaced := map[string]Identity{"1": {Email: "two@example.com"}}
	accepted, err := f.store.Record(map[string]FetchRecord{"1": {Usage: measurement(10)}}, replaced, claims, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accepted["1"] {
		t.Error("a measurement was written to a slot that changed hands")
	}
}

// An unfenced writer defers to a LIVE lease but never to an expired one, so a
// crashed claimer's leftover ticket ages out instead of blocking every later
// writer.
func TestAnUnfencedWriteDefersOnlyToALiveLease(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.Reserve([]string{"1"}, f.ids, Mode{}); err != nil {
		t.Fatal(err)
	}

	accepted, err := f.store.Record(map[string]FetchRecord{"1": {Usage: measurement(10)}}, f.ids, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if accepted["1"] {
		t.Error("an unfenced write ran over a live lease")
	}

	f.advance(ClaimTTL + time.Second)
	accepted, err = f.store.Record(map[string]FetchRecord{"1": {Usage: measurement(10)}}, f.ids, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted["1"] {
		t.Error("an unfenced write was blocked by a lease that had aged out")
	}
}

// A row written by a collector predating the fenced lease stamps only the
// attempt time; it is honored for a short window so the two generations do not
// double-fetch during an upgrade.
func TestALegacyLeaseIsHonoredBriefly(t *testing.T) {
	tests := []struct {
		name    string
		elapsed time.Duration
		want    bool
	}{
		{"just stamped", 0, true},
		{"inside the legacy window", LegacyClaimTTL - time.Second, true},
		{"at the edge", LegacyClaimTTL, false},
		{"past it", LegacyClaimTTL + time.Second, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			attempt := base
			now := base.Add(tt.elapsed)
			if got := liveClaim(time.Time{}, attempt, now); got != tt.want {
				t.Errorf("liveClaim = %v, want %v", got, tt.want)
			}
		})
	}

	// The fenced form wins whenever it is present, including when it says the
	// lease is over and the legacy fallback would still say it is live.
	t.Run("the fenced form wins", func(t *testing.T) {
		if liveClaim(time.UnixMilli(0), base, base) {
			t.Error("a released fenced lease fell back to the legacy window")
		}
		if !liveClaim(base.Add(time.Hour), time.Time{}, base) {
			t.Error("a live fenced lease was not honored")
		}
	})
}

func TestClaimLeasesUnconditionally(t *testing.T) {
	f := newFixture(t)
	// A slot that Reserve would refuse: freshly measured and not yet due.
	f.record("1", FetchRecord{Usage: measurement(10)})
	plan := pollpolicy.Plan{NextPollAt: f.now.Add(time.Hour), Interval: time.Hour}
	if err := f.store.SetPollPlan(map[string]pollpolicy.Plan{"1": plan}, f.ids); err != nil {
		t.Fatal(err)
	}

	if won, _ := f.store.Reserve([]string{"1"}, f.ids, Mode{RespectPlans: true}); len(won) != 0 {
		t.Fatal("Reserve won a fresh, not-due slot")
	}

	claims, err := f.store.Claim([]string{"1"}, f.ids)
	if err != nil {
		t.Fatal(err)
	}
	if claims["1"] == "" {
		t.Error("Claim refused a slot the caller had already decided to fetch")
	}
	if !f.entry("1").Claimed(f.now) {
		t.Error("Claim did not stamp a lease")
	}
}

// Concurrent collectors in one process must not both win a slot, and the table
// must survive the traffic.
func TestConcurrentReservesAreExclusive(t *testing.T) {
	f := newFixture(t)
	const collectors = 8

	var mu sync.Mutex
	var winners int
	var wg sync.WaitGroup
	for range collectors {
		wg.Go(func() {
			won, err := f.store.Reserve([]string{"1"}, f.ids, Mode{})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				t.Errorf("Reserve: %v", err)
				return
			}
			if len(won) > 0 {
				winners++
			}
		})
	}
	wg.Wait()

	if winners != 1 {
		t.Errorf("%d collectors won the same slot, want exactly 1", winners)
	}
}

// Every outcome releases the lease, sentinels included: the fetch is over
// either way, and a sentinel that held one would strand the slot for the TTL.
func TestEveryOutcomeShapeReleasesTheLease(t *testing.T) {
	tests := []struct {
		name   string
		record FetchRecord
	}{
		{"success", FetchRecord{Usage: measurement(10)}},
		{"failure", FetchRecord{Error: claudeapi.KindTimeout}},
		{"sentinel", FetchRecord{Sentinel: "api key"}},
		{"a success carrying no window data", FetchRecord{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			claims, err := f.store.Reserve([]string{"1"}, f.ids, Mode{})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.Record(map[string]FetchRecord{"1": tt.record}, f.ids, claims, nil); err != nil {
				t.Fatal(err)
			}
			if f.entry("1").Claimed(f.now) {
				t.Error("the lease survived the outcome")
			}
		})
	}
}

// A losing Reserve must not rewrite the file other collectors are reading.
func TestALosingReserveWritesNothing(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Usage: measurement(10)})
	if err := f.store.SetPollPlan(map[string]pollpolicy.Plan{
		"1": {NextPollAt: f.now.Add(time.Hour), Interval: time.Hour},
	}, f.ids); err != nil {
		t.Fatal(err)
	}
	before := f.raw("1")

	if won, _ := f.store.Reserve([]string{"1"}, f.ids, Mode{RespectPlans: true}); len(won) != 0 {
		t.Fatal("the reserve was expected to lose")
	}
	after := f.raw("1")

	for _, key := range []string{"lastAttemptAt", "claimId", "claimUntil", "nextPollAt"} {
		if before[key] != after[key] {
			t.Errorf("%q moved on a losing reserve: %v -> %v", key, before[key], after[key])
		}
	}
}
