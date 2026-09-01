package pollpolicy

import (
	"testing"
	"time"

	"github.com/d0lim/ccswap/internal/usage"
)

var now = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// noJitter pins the jitter to its midpoint, so a plan's Interval and its
// NextPollAt agree exactly and the schedule is reproducible.
func noJitter() float64 { return 0.5 }

// at builds usage with a given binding percentage and no reset timestamps.
func at(pct float64) *usage.Result {
	return &usage.Result{FiveHour: &usage.Window{Pct: pct}}
}

// atWithReset builds usage whose seven-day window resets at the given time.
func atWithReset(pct float64, reset time.Time) *usage.Result {
	return &usage.Result{SevenDay: &usage.Window{
		Pct:      pct,
		ResetsAt: reset.Format(time.RFC3339),
	}}
}

func plan(in Input) Plan {
	in.Now = now
	if in.Rand == nil {
		in.Rand = noJitter
	}
	if in.Threshold == 0 {
		in.Threshold = 90
	}
	return AfterFetch(in)
}

// ---------------------------------------------------------------- Cadence

func TestIntervalFromMovement(t *testing.T) {
	tests := []struct {
		name     string
		in       Input
		wantIntv time.Duration
	}{
		{
			// A first fetch has no previous measurement, so there is nothing to
			// compare against and the default applies.
			name:     "a first fetch of the active account uses its default",
			in:       Input{IsActive: true, NewUsage: at(10)},
			wantIntv: MinInterval,
		},
		{
			name:     "a first fetch of a candidate uses the wider default",
			in:       Input{NewUsage: at(10)},
			wantIntv: CandidateInterval,
		},
		{
			// Movement halves the interval: something is consuming this
			// account, here or elsewhere.
			name:     "movement halves the interval",
			in:       Input{PrevInterval: 600 * time.Second, PrevUsage: at(10), NewUsage: at(20)},
			wantIntv: 300 * time.Second,
		},
		{
			name:     "halving is floored at the minimum",
			in:       Input{PrevInterval: MinInterval, PrevUsage: at(10), NewUsage: at(20)},
			wantIntv: MinInterval,
		},
		{
			// Below the delta it is noise, not consumption.
			name:     "a sub-delta wiggle is not movement",
			in:       Input{PrevInterval: 300 * time.Second, PrevUsage: at(10), NewUsage: at(10.5)},
			wantIntv: 450 * time.Second,
		},
		{
			name:     "no movement decays toward the candidate ceiling",
			in:       Input{PrevInterval: CandidateMaxInterval, PrevUsage: at(10), NewUsage: at(10)},
			wantIntv: CandidateMaxInterval,
		},
		{
			name:     "the active account decays to its narrower ceiling",
			in:       Input{IsActive: true, PrevInterval: 600 * time.Second, PrevUsage: at(10), NewUsage: at(10)},
			wantIntv: ActiveMaxInterval,
		},
		{
			// Unknown utilization is not movement and not stillness; it falls
			// back to the default rather than guessing.
			name:     "unknown utilization uses the default",
			in:       Input{PrevInterval: 600 * time.Second, PrevUsage: &usage.Result{}, NewUsage: at(10)},
			wantIntv: CandidateInterval,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := plan(tt.in); got.Interval != tt.wantIntv {
				t.Errorf("Interval = %v, want %v", got.Interval, tt.wantIntv)
			}
		})
	}
}

// ---------------------------------------------------------------- Urgent mode

func TestUrgentMode(t *testing.T) {
	// Threshold 90 with a 15-point margin: the band starts at 75.
	tests := []struct {
		name     string
		in       Input
		wantIntv time.Duration
	}{
		{
			name:     "the active account moving inside the band goes urgent",
			in:       Input{IsActive: true, PrevUsage: at(76), NewUsage: at(80)},
			wantIntv: UrgentInterval,
		},
		{
			// A candidate is not the account about to hit the limit.
			name:     "a candidate never goes urgent",
			in:       Input{PrevUsage: at(76), NewUsage: at(80)},
			wantIntv: max(MinInterval, CandidateInterval/2),
		},
		{
			// Sitting inside the band without burning is not urgent.
			name:     "no movement means no urgency",
			in:       Input{IsActive: true, PrevInterval: MinInterval, PrevUsage: at(80), NewUsage: at(80)},
			wantIntv: 270 * time.Second,
		},
		{
			name:     "below the band there is no urgency",
			in:       Input{IsActive: true, PrevUsage: at(50), NewUsage: at(60)},
			wantIntv: max(MinInterval, MinInterval/2),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := plan(tt.in); got.Interval != tt.wantIntv {
				t.Errorf("Interval = %v, want %v", got.Interval, tt.wantIntv)
			}
		})
	}
}

// The urgent band must not be entered while the token is already blocked;
// spending the freed capacity faster is exactly wrong there.
func TestRecent429SuppressesUrgency(t *testing.T) {
	got := plan(Input{IsActive: true, PrevUsage: at(76), NewUsage: at(80), Recent429: true})
	if got.Interval == UrgentInterval {
		t.Error("urgent mode fired despite a recent 429")
	}
	if got.Interval < Post429MinInterval {
		t.Errorf("Interval = %v, want at least the post-429 floor %v", got.Interval, Post429MinInterval)
	}
}

// The decay is floored so urgent mode's sub-floor interval snaps straight back
// to the normal cadence, rather than decaying through 90- and 135-second polls
// the budget never intended.
func TestUrgentThenUnmovedSnapsBackToTheFloor(t *testing.T) {
	got := plan(Input{IsActive: true, PrevInterval: UrgentInterval, PrevUsage: at(80), NewUsage: at(80)})
	if got.Interval != MinInterval {
		t.Errorf("Interval = %v, want it to snap back to %v", got.Interval, MinInterval)
	}
}

// ---------------------------------------------------------------- 429 backoff

func TestRecent429Backoff(t *testing.T) {
	tests := []struct {
		name     string
		in       Input
		wantIntv time.Duration
	}{
		{
			// From the tightest base the AIMD increase alone lands under the
			// floor, so the floor is what binds.
			name:     "a 429 from the tightest cadence lands on the floor",
			in:       Input{IsActive: true, PrevInterval: MinInterval, PrevUsage: at(10), NewUsage: at(10), Recent429: true},
			wantIntv: Post429MinInterval,
		},
		{
			// AIMD increase: grow multiplicatively from the last interval, so
			// machines sharing a contended token each retreat until their
			// combined rate fits.
			name:     "a later 429 grows multiplicatively from the previous interval",
			in:       Input{PrevInterval: 600 * time.Second, PrevUsage: at(10), NewUsage: at(10), Recent429: true},
			wantIntv: 900 * time.Second,
		},
		{
			// A cadence already slower than the floor is not pulled back down.
			name:     "an already-slower learned cadence survives the floor",
			in:       Input{PrevInterval: 1200 * time.Second, PrevUsage: at(10), NewUsage: at(10), Recent429: true},
			wantIntv: 1800 * time.Second,
		},
		{
			name:     "sustained 429s stop at the wide ceiling",
			in:       Input{PrevInterval: Post429MaxInterval, PrevUsage: at(10), NewUsage: at(10), Recent429: true},
			wantIntv: Post429MaxInterval,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := plan(tt.in); got.Interval != tt.wantIntv {
				t.Errorf("Interval = %v, want %v", got.Interval, tt.wantIntv)
			}
		})
	}
}

// Whatever the base, a recent 429 never leaves the cadence below the floor.
func TestRecent429AlwaysClearsTheFloor(t *testing.T) {
	for _, base := range []time.Duration{0, MinInterval, CandidateInterval, CandidateMaxInterval} {
		got := plan(Input{PrevInterval: base, PrevUsage: at(10), NewUsage: at(10), Recent429: true})
		if got.Interval < Post429MinInterval {
			t.Errorf("base %v: Interval = %v, below the floor %v", base, got.Interval, Post429MinInterval)
		}
	}
}

// The 429 ceiling has to exceed the normal candidate ceiling, or several
// machines could never back off far enough for their combined rate to fit.
func TestThe429CeilingIsWiderThanTheNormalOne(t *testing.T) {
	if Post429MaxInterval <= CandidateMaxInterval {
		t.Errorf("Post429MaxInterval %v does not exceed CandidateMaxInterval %v",
			Post429MaxInterval, CandidateMaxInterval)
	}
}

// Without recency the wide ceiling must not apply, or one old 429 would slow
// the account down forever.
func TestWithoutRecencyTheNarrowCeilingApplies(t *testing.T) {
	got := plan(Input{PrevInterval: Post429MaxInterval, PrevUsage: at(10), NewUsage: at(10)})
	if got.Interval != CandidateMaxInterval {
		t.Errorf("Interval = %v, want the narrow ceiling %v", got.Interval, CandidateMaxInterval)
	}
}

// ---------------------------------------------------------------- Resets

// Stored usage is obsolete the moment a window rolls over.
func TestAPollIsNeverScheduledPastAReset(t *testing.T) {
	reset := now.Add(2 * time.Minute)
	got := plan(Input{PrevUsage: atWithReset(10, reset), NewUsage: atWithReset(10, reset)})

	want := reset.Add(ResetSlack)
	if !got.NextPollAt.Equal(want) {
		t.Errorf("NextPollAt = %v, want it pulled to the reset at %v", got.NextPollAt, want)
	}
}

// A reset already in the past says nothing about when to poll next.
func TestAPastResetDoesNotPullThePollForward(t *testing.T) {
	reset := now.Add(-time.Hour)
	got := plan(Input{PrevUsage: atWithReset(10, reset), NewUsage: atWithReset(10, reset)})

	if !got.NextPollAt.After(now) {
		t.Errorf("NextPollAt = %v, want it in the future", got.NextPollAt)
	}
}

// Exhaustion is stable enough to poll slowly, but not to stop: quota can be
// granted before the advertised reset, and status must not age into unknown
// while the scheduler waits.
func TestAtLimitKeepsABoundedProbeBeforeADistantReset(t *testing.T) {
	reset := now.Add(6 * time.Hour)
	got := plan(Input{PrevUsage: atWithReset(100, reset), NewUsage: atWithReset(100, reset)})

	if got.Interval != ExhaustedInterval {
		t.Errorf("Interval = %v, want the exhausted probe %v", got.Interval, ExhaustedInterval)
	}
	if !got.NextPollAt.Before(reset) {
		t.Errorf("NextPollAt = %v, want a probe well before the reset at %v", got.NextPollAt, reset)
	}
}

func TestAtLimitIsPulledToAnImminentReset(t *testing.T) {
	reset := now.Add(time.Minute)
	got := plan(Input{PrevUsage: atWithReset(100, reset), NewUsage: atWithReset(100, reset)})

	if want := reset.Add(ResetSlack); !got.NextPollAt.Equal(want) {
		t.Errorf("NextPollAt = %v, want %v", got.NextPollAt, want)
	}
}

// A congestion-control interval already wider than the exhausted probe is
// preserved rather than pulled back down.
func TestAtLimitPreservesAWider429Interval(t *testing.T) {
	got := plan(Input{
		PrevInterval: Post429MaxInterval,
		PrevUsage:    at(100),
		NewUsage:     at(100),
		Recent429:    true,
	})
	if got.Interval != Post429MaxInterval {
		t.Errorf("Interval = %v, want the wider 429 interval preserved", got.Interval)
	}
}

// ---------------------------------------------------------------- Jitter

// Independent processes must drift apart instead of fetching in lockstep.
func TestJitterStaysWithinBounds(t *testing.T) {
	base := Input{PrevUsage: at(10), NewUsage: at(10), Now: now, Threshold: 90}

	for _, r := range []float64{0, 0.5, 1} {
		in := base
		in.Rand = func() float64 { return r }
		got := AfterFetch(in)

		delay := got.NextPollAt.Sub(now)
		lo := time.Duration(float64(got.Interval) * (1 - JitterFrac))
		hi := time.Duration(float64(got.Interval) * (1 + JitterFrac))
		if delay < lo || delay > hi {
			t.Errorf("rand=%v: delay %v outside [%v, %v]", r, delay, lo, hi)
		}
	}
}

func TestJitterDefaultsToRealRandomness(t *testing.T) {
	// A nil Rand must not panic; it falls back to the package source.
	got := AfterFetch(Input{PrevUsage: at(10), NewUsage: at(10), Now: now, Threshold: 90})
	if got.NextPollAt.Before(now) {
		t.Errorf("NextPollAt = %v, want it after now", got.NextPollAt)
	}
}

// ---------------------------------------------------------------- Budget

// These are the invariants the whole cadence model rests on. If one of them
// stops holding, the constants are no longer under the measured budget.
func TestBudgetInvariants(t *testing.T) {
	const measuredHourlyCap = 28.0

	t.Run("the sustained floor stays under the hourly cap", func(t *testing.T) {
		perHour := time.Hour.Seconds() / MinInterval.Seconds()
		if perHour >= measuredHourlyCap {
			t.Errorf("the %v floor is %.0f requests/hour, at or over the ~%.0f cap",
				MinInterval, perHour, measuredHourlyCap)
		}
	})

	t.Run("the serve TTL matches the floor", func(t *testing.T) {
		// The TTL is what bounds the rate across however many surfaces are
		// open; a TTL below the floor would let extra surfaces spend budget.
		if ServeTTL < MinInterval {
			t.Errorf("ServeTTL %v is below the cadence floor %v", ServeTTL, MinInterval)
		}
	})

	t.Run("edge backoff probes slower than capacity frees", func(t *testing.T) {
		perHour := time.Hour.Seconds() / EdgeBackoff.Seconds()
		if perHour > measuredHourlyCap/2 {
			t.Errorf("edge backoff probes %.0f/hour, too fast against a ~%.0f/hour cap",
				perHour, measuredHourlyCap)
		}
	})

	t.Run("the post-429 window covers the saturation horizon", func(t *testing.T) {
		// A full trailing hour takes up to sixty minutes to age out.
		if Recent429Window < time.Hour {
			t.Errorf("Recent429Window %v is shorter than the saturation horizon", Recent429Window)
		}
	})

	t.Run("an urgent episode alone fits inside the window", func(t *testing.T) {
		// Urgent mode is bounded by construction: each further urgent poll needs
		// at least MovementDeltaPct of movement, so the slowest qualifying burn
		// crosses the escalation band in margin/delta polls — inside the rolling
		// hourly window even before the post-429 floor, which absorbs any
		// overshoot, is considered.
		polls := EscalationMarginPct / MovementDeltaPct
		if polls >= measuredHourlyCap-1 {
			t.Errorf("an urgent episode is up to %.0f polls, which does not fit a ~%.0f/hour budget",
				polls, measuredHourlyCap)
		}
	})
}
