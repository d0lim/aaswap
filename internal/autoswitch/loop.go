package autoswitch

import (
	"context"
	"time"

	"github.com/realiti4/claude-swap/internal/jsonout"
	"github.com/realiti4/claude-swap/internal/pollpolicy"
	"github.com/realiti4/claude-swap/internal/usagestore"
)

// Run ticks until the context is cancelled.
//
// A failing tick never stops the loop: an engine that dies on one bad tick is
// worse than one that reports it and carries on, because the user finds out
// hours later that nothing has been watching.
func (e *Engine) Run(ctx context.Context) error {
	timer := time.NewTimer(0)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()

	for {
		if ctx.Err() != nil {
			return nil
		}
		outcome := e.Tick(ctx)
		delay := e.nextDelay(outcome)

		// Announced only when it is a real wait: a tick of ordinary jitter
		// would otherwise fill the log with sleep lines nobody reads.
		if delay > time.Duration(e.Settings.IntervalSeconds*1.5)*time.Second {
			seconds := delay.Seconds()
			event := e.event(KindSleep)
			event.Seconds = &seconds
			event.Until = jsonout.Timestamp(e.now().Add(delay))
			e.emit(event)
		}

		timer.Reset(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil
		case <-timer.C:
		}
	}
}

// nextDelay is how long to wait before the next tick.
func (e *Engine) nextDelay(outcome Outcome) time.Duration {
	interval := time.Duration(e.Settings.IntervalSeconds * float64(time.Second))

	switch {
	case outcome == Blocked && !e.sleepUntil.IsZero():
		// Waiting for a known reset: sleep to it, but never past the
		// exhausted-account cadence — a provider can grant quota early, and a
		// long sleep would suppress the fetch that discovers it.
		delay := e.sleepUntil.Sub(e.now())
		return min(max(delay, interval), MaxSleep)

	case outcome == Blocked && e.blockedWaitLong:
		// Genuinely stuck with nothing to wait for: no candidates, or no reset
		// time known. Re-polling at full cadence would spend the endpoint's
		// budget learning the same thing.
		return max(interval, NoResetFallback)

	case outcome == NoAction && e.idleHoldSlow:
		// Claude Code is idle on an expired token. Nothing changes until the
		// user comes back, so crawl; protection resumes one slow tick after
		// they do.
		return max(interval, NoResetFallback)
	}

	// Blocked on something that can resolve on any tick — a margin, an
	// unreadable row — keeps the normal cadence, so the at-limit escape is not
	// missed.
	jittered := time.Duration(float64(interval) * (0.9 + 0.2*e.jitter()))
	return e.respectPollPlan(jittered)
}

// respectPollPlan shortens a normal-cadence sleep to the store's own next-poll
// time.
//
// The planner tightens the active account's cadence while it burns toward the
// threshold, but a loop that always sleeps its configured interval runs the plan
// late. This makes the loop OBEY the plan rather than override it.
//
// Only ever shortens, and never below the planner's own floor: the request
// budget lives in the plan. The DEADLINE is clamped rather than the result —
// clamping the result would RAISE a delay that was already below the floor,
// flattening the lower half of the jitter at the default interval.
func (e *Engine) respectPollPlan(delay time.Duration) time.Duration {
	roster, err := e.Switcher.RosterOrEmpty()
	if err != nil {
		return delay
	}
	current, managed := e.Switcher.CurrentNumber(roster)
	if !managed {
		return delay
	}
	account := roster.Accounts[current]
	entries := e.Switcher.Usage.Entries(map[string]usagestore.Identity{
		current: {Email: account.Email, OrganizationUUID: account.OrganizationUUID},
	}, nil)
	entry, known := entries[current]
	if !known || entry.NextPollAt.IsZero() {
		return delay
	}
	dueIn := entry.NextPollAt.Sub(e.now())
	return min(delay, max(dueIn, pollpolicy.UrgentInterval))
}
