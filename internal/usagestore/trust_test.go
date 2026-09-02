package usagestore

import (
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/pollpolicy"
	"github.com/d0lim/aaswap/internal/usage"
)

// Trust is what a switch decision runs on. Plain staleness ends it; deliberate
// staleness — the server refusing fresher data, or the scheduler choosing the
// cadence — extends it, but never past a ceiling.
func TestDecisionTrust(t *testing.T) {
	tests := []struct {
		name string
		// setup ages the row and installs whatever state the case is about.
		setup     func(f *fixture)
		age       time.Duration
		wantKnown bool
	}{
		{
			name:      "fresh data is trusted",
			age:       time.Minute,
			wantKnown: true,
		},
		{
			name:      "at the staleness edge",
			age:       StaleOK,
			wantKnown: true,
		},
		{
			// No failure and no plan: nothing makes this staleness deliberate.
			name:      "plain staleness ends trust",
			age:       StaleOK + time.Second,
			wantKnown: false,
		},
		{
			// The server is refusing fresher data, so the stale value is the
			// best answer available and the account should not read as unknown
			// merely because an endpoint is down.
			name: "a failure extends trust",
			setup: func(f *fixture) {
				f.record("1", FetchRecord{Error: claudeapi.KindTimeout})
			},
			age:       StaleOK + time.Minute,
			wantKnown: true,
		},
		{
			// But never past the ceiling: a forever-failing account has to
			// eventually read as unknown so the unknown-path machinery takes
			// back over.
			name: "a failure cannot extend trust past the ceiling",
			setup: func(f *fixture) {
				f.record("1", FetchRecord{Error: claudeapi.KindTimeout})
			},
			age:       TrustMaxAge + time.Second,
			wantKnown: false,
		},
		{
			name: "a scheduler-chosen cadence extends trust",
			setup: func(f *fixture) {
				_ = f.store.SetPollPlan(map[string]pollpolicy.Plan{
					"1": {NextPollAt: f.now.Add(2 * time.Hour), Interval: 10 * time.Minute},
				}, f.ids)
			},
			age:       StaleOK + time.Minute,
			wantKnown: true,
		},
		{
			// A live lease keeps the bridge up: another collector just won the
			// fetch, and flipping trusted to unknown for the seconds it is in
			// flight would count an unhealthy tick for nothing.
			name: "a live lease extends trust",
			setup: func(f *fixture) {
				f.advance(StaleOK + time.Minute)
				_, _ = f.store.Reserve([]string{"1"}, f.ids, Mode{})
				f.advance(-(StaleOK + time.Minute))
			},
			age:       StaleOK + time.Minute,
			wantKnown: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.record("1", FetchRecord{Usage: measurement(40)})
			if tt.setup != nil {
				tt.setup(f)
			}
			f.now = base.Add(tt.age)

			decision, known := f.entry("1").DecisionValue()
			if known != tt.wantKnown {
				t.Fatalf("known = %v, want %v", known, tt.wantKnown)
			}
			if known && decision.Usage == nil {
				t.Error("the decision carried no measurement")
			}
		})
	}
}

// A 429 throttles polling without moving the account's real windows. Usage only
// rises within a window, so a frozen measurement stays a valid lower bound right
// up to that window's reset — and treating a merely-throttled account as
// unknown made it an unusable switch target and drove failover flapping.
func TestRateLimitedTrustFollowsTheWindowNotAClock(t *testing.T) {
	// A window that resets well beyond the general ceiling.
	farFuture := base.Add(6 * time.Hour).Format(time.RFC3339)

	tests := []struct {
		name      string
		lastGood  *usage.Result
		lastError claudeapi.ErrorKind
		age       time.Duration
		wantKnown bool
	}{
		{
			// The general ceiling would have ended trust an hour ago.
			name:      "a 429 keeps trust past the general ceiling",
			lastGood:  &usage.Result{SevenDay: &usage.Window{Pct: 40, ResetsAt: farFuture}},
			lastError: claudeapi.HTTPKind(429),
			age:       TrustMaxAge + 30*time.Minute,
			wantKnown: true,
		},
		{
			// A timeout is no evidence the measurement still holds, so it gets
			// the general ceiling regardless of the window.
			name:      "a non-429 failure does not",
			lastGood:  &usage.Result{SevenDay: &usage.Window{Pct: 40, ResetsAt: farFuture}},
			lastError: claudeapi.KindTimeout,
			age:       TrustMaxAge + 30*time.Minute,
			wantKnown: false,
		},
		{
			// Once the window rolls over, usage there is zeroed and the whole
			// measurement is obsolete.
			name: "the window's reset ends trust",
			lastGood: &usage.Result{
				SevenDay: &usage.Window{Pct: 40, ResetsAt: base.Add(30 * time.Minute).Format(time.RFC3339)},
			},
			lastError: claudeapi.HTTPKind(429),
			age:       31 * time.Minute,
			wantKnown: false,
		},
		{
			// The SOONEST window is what binds: a later one cannot rescue a
			// measurement whose earlier window already rolled over.
			name: "the soonest window binds, not the latest",
			lastGood: &usage.Result{
				FiveHour: &usage.Window{Pct: 10, ResetsAt: base.Add(30 * time.Minute).Format(time.RFC3339)},
				SevenDay: &usage.Window{Pct: 40, ResetsAt: farFuture},
			},
			lastError: claudeapi.HTTPKind(429),
			age:       31 * time.Minute,
			wantKnown: false,
		},
		{
			// A far-future or malformed reset must never grant unbounded trust.
			name: "the client-side ceiling still bounds it",
			lastGood: &usage.Result{
				SevenDay: &usage.Window{Pct: 40, ResetsAt: base.Add(9999 * time.Hour).Format(time.RFC3339)},
			},
			lastError: claudeapi.HTTPKind(429),
			age:       RateLimitTrustMaxAge + time.Minute,
			wantKnown: false,
		},
		{
			// A row with no reset info at all falls back to the ceiling alone.
			name:      "no reset info falls back to the ceiling",
			lastGood:  measurement(40),
			lastError: claudeapi.HTTPKind(429),
			age:       RateLimitTrustMaxAge - time.Minute,
			wantKnown: true,
		},
		{
			name:      "and that fallback is bounded too",
			lastGood:  measurement(40),
			lastError: claudeapi.HTTPKind(429),
			age:       RateLimitTrustMaxAge + time.Minute,
			wantKnown: false,
		},
		{
			// A window carrying no reset contributes no timestamp, so partial
			// metadata can only tighten the bound, never loosen it.
			name: "a window with no reset does not extend anything",
			lastGood: &usage.Result{
				FiveHour: &usage.Window{Pct: 10},
				SevenDay: &usage.Window{Pct: 40, ResetsAt: base.Add(30 * time.Minute).Format(time.RFC3339)},
			},
			lastError: claudeapi.HTTPKind(429),
			age:       31 * time.Minute,
			wantKnown: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.record("1", FetchRecord{Usage: tt.lastGood})
			f.advance(time.Second)
			f.record("1", FetchRecord{Error: tt.lastError})
			f.now = base.Add(tt.age)

			_, known := f.entry("1").DecisionValue()
			if known != tt.wantKnown {
				t.Errorf("known = %v, want %v", known, tt.wantKnown)
			}
		})
	}
}

// The per-model windows gate the account too, so their resets have to bind the
// same trust the scheduler's view does.
func TestScopedWindowResetsBindRateLimitedTrust(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Usage: &usage.Result{
		SevenDay: &usage.Window{Pct: 40, ResetsAt: base.Add(6 * time.Hour).Format(time.RFC3339)},
		Scoped: []usage.Scoped{
			{Name: "Fable", Pct: 100, ResetsAt: base.Add(30 * time.Minute).Format(time.RFC3339)},
		},
	}})
	f.advance(time.Second)
	f.record("1", FetchRecord{Error: claudeapi.HTTPKind(429)})
	f.now = base.Add(31 * time.Minute)

	// With no model selected, the Fable window does not gate anything, so its
	// reset does not end trust.
	if _, known := f.store.Entries(f.ids, nil)["1"].DecisionValue(); !known {
		t.Error("an unselected model's reset ended trust")
	}
	// Pinned to Fable, that window binds — and its reset has passed.
	if _, known := f.store.Entries(f.ids, []string{"Fable"})["1"].DecisionValue(); known {
		t.Error("a selected model's reset did not end trust")
	}
}

// Unknown must never be confused with exhausted: callers treat it as "do not
// skip this account", and a zero would mean the opposite.
func TestAnUnknownEntryCarriesNoMeasurement(t *testing.T) {
	f := newFixture(t)
	f.record("1", FetchRecord{Usage: measurement(95)})
	f.now = base.Add(TrustMaxAge + time.Hour)

	entry := f.entry("1")
	decision, known := entry.DecisionValue()
	if known {
		t.Fatalf("decision = %+v, want unknown", decision)
	}
	// Display code still sees the old measurement — it may show it, annotated
	// with its age.
	if entry.LastGood == nil {
		t.Error("the measurement was destroyed rather than merely untrusted")
	}
	if entry.Age < TrustMaxAge {
		t.Errorf("Age = %v, want the true age for display", entry.Age)
	}
}

func TestFailureBackoff(t *testing.T) {
	tests := []struct {
		name        string
		failures    int
		retryAfter  *time.Duration
		rateLimited bool
		want        time.Duration
	}{
		{"the first failure", 1, nil, false, BackoffBase},
		{"the second doubles", 2, nil, false, 2 * BackoffBase},
		{"the third doubles again", 3, nil, false, 4 * BackoffBase},
		{"the curve saturates at the cap", 6, nil, false, BackoffCap},
		// A permanently failing account increments forever; the arithmetic must
		// not overflow into something absurd.
		{"a very long failure run stays capped", 10_000, nil, false, BackoffCap},

		{
			// The saturated-budget edge: capacity frees only as old requests
			// age out, so an immediate retry prolongs the oscillation.
			name:     "Retry-After 0 on a 429 waits out the edge",
			failures: 1, retryAfter: new(time.Duration(0)), rateLimited: true,
			want: pollpolicy.EdgeBackoff,
		},
		{
			// That rule was measured on this endpoint's 429s alone. A proxy
			// saying "retry now" on a 503 is a different animal.
			name:     "Retry-After 0 on a non-429 falls through to the curve",
			failures: 1, retryAfter: new(time.Duration(0)), rateLimited: false,
			want: BackoffBase,
		},
		{
			// A short ask was measured as accurate, so it is honored as-is —
			// no margin, because none is needed.
			name:     "a short ask is honored exactly",
			failures: 1, retryAfter: new(300 * time.Second), rateLimited: true,
			want: 300 * time.Second,
		},
		{
			// Landing ON the deadline is where a re-block earns a fresh hour.
			name:     "a long ask takes the margin",
			failures: 1, retryAfter: new(3600 * time.Second), rateLimited: true,
			want: 3600*time.Second + RetryAfterMargin,
		},
		{
			// A non-429 row is read unknown at TrustMaxAge, so taking a margin
			// measured on hour-scale 429 blocks would park it past its own
			// trust: blind, neither pollable nor usable.
			name:     "a long ask on a non-429 takes no margin",
			failures: 1, retryAfter: new(3000 * time.Second), rateLimited: false,
			want: 3000 * time.Second,
		},
		{
			name:     "a pathological ask is bounded on the 429 arm",
			failures: 1, retryAfter: new(86400 * time.Second), rateLimited: true,
			want: RetryAfterFloorCap,
		},
		{
			// Bounded by the ceiling ITS OWN arm's trust uses, not one shared
			// constant.
			name:     "and by a tighter bound on the non-429 arm",
			failures: 1, retryAfter: new(86400 * time.Second), rateLimited: false,
			want: TrustMaxAge,
		},
		{
			// Our own curve may still out-wait a short ask once it has
			// saturated.
			name:     "the curve wins when it is longer",
			failures: 20, retryAfter: new(60 * time.Second), rateLimited: true,
			want: BackoffCap,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := failureBackoff(tt.failures, tt.retryAfter, tt.rateLimited)
			if got != tt.want {
				t.Errorf("failureBackoff = %v, want %v", got, tt.want)
			}
		})
	}
}

// The cap has to sit inside the trust its own arm relies on, or a wait releases
// a row that is un-pollable and unknown at the same time.
func TestEachArmsCapSitsInsideItsOwnTrust(t *testing.T) {
	if RetryAfterFloorCap > RateLimitTrustMaxAge {
		t.Errorf("the 429 cap %v exceeds the trust %v it relies on", RetryAfterFloorCap, RateLimitTrustMaxAge)
	}
	// The 429 cap is sized to the measured shape: blocks open at an hour, and
	// the cap is that plus the margin. If real blocks ever open above an hour,
	// raise this with the shape rather than letting the cap silently eat the
	// margin.
	if want := time.Hour + RetryAfterMargin; RetryAfterFloorCap != want {
		t.Errorf("RetryAfterFloorCap = %v, want %v (an hour plus the margin)", RetryAfterFloorCap, want)
	}
	// A non-429 wait is capped at exactly the ceiling its trust uses.
	longAsk := new(24 * time.Hour)
	if got := failureBackoff(1, longAsk, false); got != TrustMaxAge {
		t.Errorf("a non-429 wait capped at %v, want the trust ceiling %v", got, TrustMaxAge)
	}
}

// The backoff a fetch failure installs is what the read model reports, so the
// two halves cannot drift apart.
func TestTheRecordedBackoffMatchesTheComputedOne(t *testing.T) {
	f := newFixture(t)
	retry := 300 * time.Second
	f.record("1", FetchRecord{Error: claudeapi.HTTPKind(429), RetryAfter: &retry})

	entry := f.entry("1")
	if !entry.InBackoff(f.now) {
		t.Fatal("a failed fetch installed no backoff")
	}
	deadline := base.Add(retry)
	if !entry.BackoffUntil.Equal(deadline) {
		t.Errorf("backoffUntil = %v, want %v", entry.BackoffUntil, deadline)
	}
	// AT the deadline the backoff is over, matching the strict comparison the
	// eligibility check uses.
	f.now = deadline
	if f.entry("1").InBackoff(f.now) {
		t.Error("the backoff outlived its own deadline")
	}
}
