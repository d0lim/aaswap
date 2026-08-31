// Package pollpolicy holds the cadence policy for the usage endpoint — every
// number in one place.
//
// # The budget being respected
//
// The endpoint enforces a rolling window of roughly 28-30 requests per hour per
// identity. It is NOT a bucket with a refill rate: capacity returns only as old
// requests age out of the trailing hour, so a burst saturates the identity for
// up to a full hour. Pausing does not restore headroom early, and earlier
// "refill rate" estimates were artifacts of measuring while already saturated.
//
// What that identity is depends on which rate-limit regime the org is on, and
// the two coexist across orgs. Under the fixed-deadline regime it is the
// account or org; under the saturated-edge regime it is the access token. The
// constants here plan for the account-scoped case because it is the
// conservative one: re-authenticating cannot be relied on to clear a block, and
// two machines holding different tokens for one account may share one budget —
// which is what the AIMD backoff below exists to converge.
//
// # What the numbers lean on
//
// The horizon is bracketed to roughly 55-64 minutes, the exact edge algorithm
// is undocumented, and it can be retuned at any time. So the constants lean
// only on the robust parts: a sustained rate safely under the cap, and an
// hour-long recovery horizon. The budget target is an average of at most one
// request every three minutes — 20/hour against the ~28-30/hour cap — leaving
// headroom for manual commands, wake-from-sleep catch-up, and the bounded
// urgent mode.
//
// The health invariant to watch in the logs is that steady state shows no 429s
// at all. If a 429 episode at modest rates ever outlasts an hour past the
// margin, this model needs revisiting.
//
// Plans computed here are persisted per account in the usage store by whichever
// collector fetched, so every surface — the list command, the TUI, the
// auto-switch engine — inherits the same cadence no matter how often it
// repaints.
package pollpolicy

import (
	"math/rand/v2"
	"time"

	"github.com/realiti4/claude-swap/internal/usage"
)

const (
	// ServeTTL is the freshness floor shared by every collector: an entry
	// younger than this is served from the store with no fetch at all, so the
	// maximum sustained rate on one token is one request per TTL regardless of
	// how many surfaces are open.
	ServeTTL = 180 * time.Second

	// MinInterval is the normal cadence floor. Movement can halve an interval
	// down to this, never below.
	MinInterval = 180 * time.Second

	// UrgentInterval is the cadence for the ACTIVE account when it is within
	// EscalationMarginPct of the switch threshold AND actually moving toward
	// it.
	//
	// Bounded by construction: either the threshold is crossed and the engine
	// switches away, or the movement stops and the next poll decays back to
	// MinInterval. The worst case is roughly fifteen polls per episode, inside
	// the measured hourly window, and any overshoot on top of steady traffic is
	// absorbed by the post-429 floor.
	UrgentInterval = 60 * time.Second

	// Decay ceilings for an account whose usage is not moving: the active
	// account stays reasonably fresh while an idle alternate drifts out to ten
	// minutes.
	ActiveMaxInterval    = 300 * time.Second
	CandidateInterval    = 300 * time.Second
	CandidateMaxInterval = 600 * time.Second

	// ExhaustedInterval keeps an at-limit account on a slow poll rather than
	// sleeping until its reported reset.
	//
	// Exhaustion is stable enough to poll slowly but not to stop: quota grants
	// and provider-side corrections can make an account usable before that
	// timestamp, and decision-grade status must not age into "unknown" while
	// the scheduler is deliberately waiting. Six requests an hour stays well
	// under the budget and still detects recovery promptly.
	ExhaustedInterval = 600 * time.Second

	// MovementDeltaPct is how far a window's binding percentage must move
	// between polls to count as being consumed — by this machine, another
	// machine, or a session. A window that moved this much tightens the
	// cadence; an unmoved one backs off toward its ceiling.
	MovementDeltaPct = 1.0

	// JitterFrac is the fraction of noise applied to each scheduled interval so
	// independent processes drift apart instead of fetching in lockstep.
	JitterFrac = 0.1

	// EdgeBackoff is the reaction to a 429 at the saturated-window edge: probe
	// at most every five minutes, so that aging-out capacity outpaces the
	// probing.
	EdgeBackoff = 300 * time.Second

	// Post429MinInterval floors the planned cadence while any 429 was seen on
	// this token inside Recent429Window, so freed capacity accumulates instead
	// of being immediately re-spent.
	Post429MinInterval = 360 * time.Second
	// Recent429Window matches the saturation horizon: a full trailing hour
	// takes up to sixty minutes to age out.
	Recent429Window = 3600 * time.Second

	// AIMD on a contended budget.
	//
	// The budget is shared across every machine polling the same account, none
	// of them can see the others, and the endpoint exposes no remaining-request
	// count — only a Retry-After once already blocked. So while 429s recur,
	// each successful poll grows the interval multiplicatively toward a ceiling
	// wider than the normal candidate one, letting several machines each back
	// off far enough that their combined rate fits. Movement — a real success
	// run with no recent 429 — decays it back down.
	//
	// This is TCP-style congestion control: the budget gets fair-shared by
	// reaction alone, with no machine count or shared state to configure.
	Post429BackoffMult = 1.5
	Post429MaxInterval = 1800 * time.Second

	// EscalationMarginPct is how close to the threshold the active account must
	// be for the engine to escalate to a full candidate refresh. It is a
	// decision-policy number, but urgent-mode cadence keys on the same band, so
	// it lives with the cadence constants.
	EscalationMarginPct = 15.0

	// ResetSlack is the grace added when clamping a poll to a known window
	// reset. A poll is never scheduled past a reset, because stored usage is
	// obsolete the moment the window rolls over.
	ResetSlack = 60 * time.Second
)

// Plan is the schedule for one account after a successful fetch.
type Plan struct {
	// NextPollAt is when this account should next be fetched.
	NextPollAt time.Time
	// Interval is the un-jittered cadence that produced NextPollAt, and is what
	// the next plan grows or shrinks from.
	Interval time.Duration
}

// Input is everything AfterFetch needs about one account.
type Input struct {
	// PrevInterval is the interval the last plan chose, or zero on a first
	// fetch.
	PrevInterval time.Duration
	// PrevUsage and NewUsage bracket this poll; movement between them is what
	// tightens or relaxes the cadence.
	PrevUsage *usage.Result
	NewUsage  *usage.Result
	// IsActive selects the active account's tighter ceilings.
	IsActive bool
	// Threshold is the switch threshold the urgent band is measured against.
	Threshold float64
	// Models are the per-model windows folded into the binding calculation.
	Models []string
	// Recent429 reports that a 429 was seen on this token within
	// Recent429Window.
	Recent429 bool
	// Now is the reference time; Rand supplies the jitter. Both are injected so
	// the schedule is reproducible in tests.
	Now  time.Time
	Rand func() float64
}

// AfterFetch computes the next poll schedule for an account that was just
// fetched successfully.
//
// Movement — the binding percentage changing by at least MovementDeltaPct since
// the previous poll — halves the interval, floored at MinInterval, or drops
// straight to UrgentInterval when the active account is moving inside the
// escalation band. No movement backs off toward the account's ceiling, and
// unknown utilization falls back to the default.
//
// A recent 429 floors the cadence and suppresses urgent mode. The scheduled
// time carries jitter and is never later than the account's next window reset
// plus slack. An at-limit account keeps a bounded slow poll rather than
// sleeping until that reset, so an early provider-side quota grant is still
// observed and its status stays decision-grade.
func AfterFetch(in Input) Plan {
	randFn := in.Rand
	if randFn == nil {
		randFn = rand.Float64
	}

	defaultInterval := CandidateInterval
	ceiling := CandidateMaxInterval
	if in.IsActive {
		defaultInterval = MinInterval
		ceiling = ActiveMaxInterval
	}
	base := in.PrevInterval
	if base <= 0 {
		base = defaultInterval
	}

	prevPct, prevKnown := in.PrevUsage.BindingPct(in.Models)
	newPct, newKnown := in.NewUsage.BindingPct(in.Models)

	var interval time.Duration
	moving := false
	switch {
	case !prevKnown || !newKnown:
		interval = defaultInterval
	case abs(newPct-prevPct) >= MovementDeltaPct:
		moving = true
		interval = max(MinInterval, base/2)
	default:
		// Floored so a sub-floor base — urgent mode's 60 seconds — snaps
		// straight back to the normal cadence once movement stops, instead of
		// decaying through 90- and 135-second polls the budget never intended.
		interval = min(ceiling, max(MinInterval, time.Duration(float64(base)*1.5)))
	}

	if in.IsActive && moving && !in.Recent429 && newKnown && newPct >= in.Threshold-EscalationMarginPct {
		interval = UrgentInterval
	}

	if in.Recent429 {
		// AIMD increase: grow multiplicatively from the last interval toward
		// the wider 429 ceiling, so machines sharing a contended token each
		// retreat until their combined rate fits the budget. Floored at
		// Post429MinInterval for the first 429.
		increased := max(time.Duration(float64(base)*Post429BackoffMult), Post429MinInterval)
		interval = min(Post429MaxInterval, max(interval, increased))
	}

	headroom, headroomKnown := in.NewUsage.Headroom(in.Models)
	exhausted := headroomKnown && headroom <= 0
	if exhausted {
		// Keep probing: quota can be granted or reset before the advertised
		// timestamp. A wider post-429 interval that congestion control already
		// selected is preserved.
		interval = max(interval, ExhaustedInterval)
	}

	jittered := time.Duration(float64(interval) * (1.0 + JitterFrac*(2.0*randFn()-1.0)))
	nextPoll := in.Now.Add(jittered)

	// A poll is never scheduled past a reset: stored usage is obsolete the
	// moment the window rolls over.
	if exhausted {
		if reset, ok := limitingReset(in.NewUsage, in.Models); ok && reset.After(in.Now) {
			nextPoll = earlier(nextPoll, reset.Add(ResetSlack))
		}
	} else if reset, ok := earliestFutureReset(in.NewUsage, in.Now, in.Models); ok {
		nextPoll = earlier(nextPoll, reset.Add(ResetSlack))
	}

	return Plan{NextPollAt: nextPoll, Interval: interval}
}

// limitingReset returns when the last of the at-or-over-limit windows resets —
// the moment the account becomes usable again.
func limitingReset(u *usage.Result, models []string) (time.Time, bool) {
	var latest time.Time
	found := false
	for _, w := range u.Windows(models) {
		if w.Pct < 100.0 {
			continue
		}
		if ts, ok := w.ResetTime(); ok && (!found || ts.After(latest)) {
			latest, found = ts, true
		}
	}
	return latest, found
}

// earliestFutureReset returns the next relevant-window reset ahead of now, at
// any utilization.
func earliestFutureReset(u *usage.Result, now time.Time, models []string) (time.Time, bool) {
	var earliest time.Time
	found := false
	for _, w := range u.Windows(models) {
		ts, ok := w.ResetTime()
		if !ok || !ts.After(now) {
			continue
		}
		if !found || ts.Before(earliest) {
			earliest, found = ts, true
		}
	}
	return earliest, found
}

func earlier(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
