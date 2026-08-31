// Package pace answers whether a weekly usage window is being burned faster
// than the week itself is elapsing.
//
// A weekly window is "ahead of pace" when the account has used more of it than
// the fraction of the reset cycle that has passed — 40% used at the 20%-through
// mark. It applies only to weekly windows: the account-wide seven-day one and
// every per-model scoped window. The 5-hour window is excluded because it
// resets too fast for pace to mean anything — it reads as ahead almost by
// definition early on, and the store's poll floor of a few minutes is a large
// fraction of five hours while being negligible against a week.
//
// [Compute] is a pure function. It consumes an already-fetched window, never
// fetches anything itself, and has no influence on poll cadence — that stays
// entirely in the polling policy. Elapsed time is measured against the
// snapshot's fetch time rather than the wall clock, so last-known-good data
// re-served after a failed refetch is judged against the clock it was actually
// measured at.
package pace

import (
	"math"
	"time"
)

const (
	// WeeklyPeriod is the fixed cadence weekly windows reset on.
	WeeklyPeriod = 7 * 24 * time.Hour

	// SuppressAfterReset is how long after a weekly reset the marker stays
	// hidden.
	//
	// Right after a reset the elapsed fraction is tiny, so the expected
	// percentage is near zero and almost any usage reads as far ahead — a false
	// positive rather than a genuine warning. A plain "elapsed is zero" guard
	// would not do: a snapshot fetched shortly after a reset already has a
	// small but nonzero elapsed time.
	SuppressAfterReset = 24 * time.Hour

	// AheadThresholdPct is the minimum gap, in percentage points, between
	// actual and expected usage before the marker shows.
	//
	// Below this, "ahead of pace" is within normal variance and would only add
	// noise to already-dense usage rows. A flat gap also means the marker
	// cannot fire once expected passes (100 - threshold), the last day or so of
	// the week, since the percentage tops out at 100. That is deliberate: by
	// then the percentage itself tells the story, and a maxed window is the
	// switcher's problem anyway.
	AheadThresholdPct = 15.0
)

// Window is one usage window as the API reports it: a percentage used, and when
// it next resets.
type Window struct {
	Pct float64
	// ResetsAt is the NEXT reset, which is the only timestamp the usage API
	// provides — never the current window's start.
	ResetsAt time.Time
	// Valid reports whether the window carried usable values at all.
	Valid bool
}

// Result is one weekly window's pace at the moment its snapshot was fetched.
type Result struct {
	// ExpectedPct is where usage would sit if the week's budget were spent
	// evenly.
	ExpectedPct float64
	// ActualPct is the window's real percentage.
	ActualPct float64
	// Elapsed is the time since this window's current cycle started.
	Elapsed time.Duration
	// Period is the window's full cycle length.
	Period time.Duration
	// Ahead reports that actual exceeds expected by at least the threshold.
	Ahead bool
}

// Options tunes [Compute]. The zero value uses the package defaults.
type Options struct {
	Period             time.Duration
	SuppressAfterReset time.Duration
	AheadThresholdPct  float64
}

func (o Options) withDefaults() Options {
	if o.Period == 0 {
		o.Period = WeeklyPeriod
	}
	if o.SuppressAfterReset == 0 {
		o.SuppressAfterReset = SuppressAfterReset
	}
	if o.AheadThresholdPct == 0 {
		o.AheadThresholdPct = AheadThresholdPct
	}
	return o
}

// Compute returns the pace for one weekly window, and whether pace is
// computable and meaningful at all.
//
// The current window's start is derived by folding the next reset back by whole
// periods until it lands at or before the fetch time. That is correct however
// many cycles old the reset timestamp is, including a stale value that has not
// been rolled forward.
//
// It reports false when the window carries no usable values, when there is no
// fetch time to measure against, or when the elapsed time since the window
// started is still inside the post-reset suppression period.
func Compute(w Window, fetchedAt time.Time, opts Options) (Result, bool) {
	o := opts.withDefaults()
	if !w.Valid || w.ResetsAt.IsZero() || fetchedAt.IsZero() {
		return Result{}, false
	}

	// The time remaining until the next reset, folded into [0, period). The
	// period minus that is the elapsed time since the current window started,
	// regardless of how many whole cycles the reset is ahead of or behind the
	// fetch.
	remaining := modDuration(w.ResetsAt.Sub(fetchedAt), o.Period)
	elapsed := o.Period - remaining
	if remaining == 0 {
		elapsed = 0
	}

	if elapsed < o.SuppressAfterReset {
		return Result{}, false
	}

	expected := min(100.0, float64(elapsed)/float64(o.Period)*100.0)
	return Result{
		ExpectedPct: expected,
		ActualPct:   w.Pct,
		Elapsed:     elapsed,
		Period:      o.Period,
		Ahead:       w.Pct-expected >= o.AheadThresholdPct,
	}, true
}

// modDuration is Python's % for durations: the result always lands in
// [0, period), even when d is negative — which it is whenever the reset
// timestamp is already in the past.
func modDuration(d, period time.Duration) time.Duration {
	if period <= 0 {
		return 0
	}
	m := d % period
	if m < 0 {
		m += period
	}
	return m
}

// ProjectedExhaustion extrapolates the current burn rate into an ETA for
// hitting 100%.
//
// JSON output only: the projection assumes a constant rate, which has wide
// error bars against real, bursty usage and would look falsely precise in the
// UI. It reports false when there is no measurable rate — no elapsed time, or
// usage that is not climbing.
func ProjectedExhaustion(r Result, fetchedAt time.Time) (time.Time, bool) {
	if r.Elapsed <= 0 || r.ActualPct <= 0 {
		return time.Time{}, false
	}
	ratePerSecond := r.ActualPct / r.Elapsed.Seconds()
	if ratePerSecond <= 0 || math.IsInf(ratePerSecond, 0) || math.IsNaN(ratePerSecond) {
		return time.Time{}, false
	}
	remainingPct := 100.0 - r.ActualPct
	if remainingPct <= 0 {
		return fetchedAt, true
	}
	return fetchedAt.Add(time.Duration(remainingPct / ratePerSecond * float64(time.Second))), true
}

// WillLastToReset answers whether, at the current burn rate, usage stays under
// 100% through the reset.
//
// JSON output only, like [ProjectedExhaustion]: a yes-or-no answer to "should I
// worry" is safe to expose even though it rests on the same linear-rate
// assumption, but it is still a projection with the same wide error bars, so it
// stays out of every human-facing surface. It reports false when there is no
// measurable rate to extrapolate from.
//
// # Relationship to the ahead marker
//
// At a constant rate the projected total is 100 × actual/expected, so this is
// false exactly when the window is over expected AT ALL — the marker's signal
// with no threshold. A window can therefore report that it will not last while
// showing no marker, because the marker additionally requires being
// AheadThresholdPct over expected. That is deliberate: scripts get the
// sensitive signal, the UI gets the noise-gated one.
func WillLastToReset(r Result) (bool, bool) {
	if r.ActualPct <= 0 {
		return true, true // no usage yet; nothing to run out of before the reset
	}
	if r.Elapsed <= 0 {
		return false, false
	}
	ratePerSecond := r.ActualPct / r.Elapsed.Seconds()
	if ratePerSecond <= 0 || math.IsInf(ratePerSecond, 0) || math.IsNaN(ratePerSecond) {
		return false, false
	}
	projectedTotal := r.ActualPct + ratePerSecond*(r.Period-r.Elapsed).Seconds()
	return projectedTotal <= 100.0, true
}
