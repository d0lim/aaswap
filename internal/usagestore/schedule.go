package usagestore

import (
	"slices"
	"time"

	"github.com/d0lim/aaswap/internal/pollpolicy"
)

// planOversleeps reports whether a scheduled poll deadline could not have come
// from the bounded planner.
//
// Reset-parking used to store a distant reset deadline while keeping the much
// shorter learned interval, which left an otherwise usable account parked until
// that reset. Detecting the impossible shape structurally — a deadline further
// out than the widest interval the planner can produce — repairs such a row
// independently of the current model selection, so changing the scoped models
// cannot strand it.
func planOversleeps(nextPollAt time.Time, interval time.Duration, now time.Time) bool {
	if nextPollAt.IsZero() {
		return false
	}
	bounded := max(interval, pollpolicy.ExhaustedInterval)
	latest := now.
		Add(time.Duration(float64(bounded) * (1.0 + pollpolicy.JitterFrac))).
		Add(pollpolicy.ResetSlack)
	return nextPollAt.After(latest)
}

// PlanOversleeps reports whether an entry carries an obsolete reset-parked plan.
func PlanOversleeps(entry Entry, now time.Time) bool {
	return planOversleeps(entry.NextPollAt, entry.PollInterval, now)
}

// DueCandidate picks the due candidate with the stalest data, reporting false
// when none is due.
//
// Due means past its scheduled poll and not in failure backoff. An account with
// a sentinel has nothing to fetch, and a quarantined one needs a re-login
// rather than a request. A perpetually failing account cannot monopolize the
// slot: its backoff removes it from the due set between attempts.
//
// The auto engine and the watch view share this so both pick the same single
// alternate to poll per pass, and poll plans are written by whichever collector
// fetched, so every surface inherits one adaptive cadence.
func DueCandidate(candidates []string, entries map[string]Entry, now time.Time) (string, bool) {
	type due struct {
		// everFetched sorts accounts with no measurement at all to the front:
		// nothing is staler than nothing.
		everFetched bool
		fetchedAt   time.Time
		num         string
	}
	var pool []due

	for _, num := range candidates {
		entry, known := entries[num]
		if !known {
			pool = append(pool, due{num: num})
			continue
		}
		switch {
		case entry.Sentinel != "":
			continue
		case entry.TokenDead(""):
			continue
		case entry.InBackoff(now):
			continue
		case !entry.NextPollAt.IsZero() && now.Before(entry.NextPollAt) && !PlanOversleeps(entry, now):
			continue
		}
		pool = append(pool, due{
			everFetched: !entry.FetchedAt.IsZero(),
			fetchedAt:   entry.FetchedAt,
			num:         num,
		})
	}
	if len(pool) == 0 {
		return "", false
	}

	slices.SortFunc(pool, func(a, b due) int {
		if a.everFetched != b.everFetched {
			if !a.everFetched {
				return -1
			}
			return 1
		}
		if !a.fetchedAt.Equal(b.fetchedAt) {
			return a.fetchedAt.Compare(b.fetchedAt)
		}
		// Ties break on the slot number, so the choice is reproducible rather
		// than dependent on map iteration order.
		return cmpString(a.num, b.num)
	})
	return pool[0].num, true
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Mode selects how [Store.Reserve] weighs an account's poll plan.
type Mode struct {
	// RespectPlans is set by on-demand callers — list, status, switch, a
	// dashboard. The entry must be BOTH stale and poll-due: an on-demand
	// surface repainting often must not out-vote the scheduler's cadence.
	//
	// The auto engine clears it, because its schedule IS the deliberate one: a
	// due entry may be re-fetched inside the serve TTL, which is how the
	// bounded urgent cadence beats that TTL, and an escalation may fetch a
	// not-yet-due candidate.
	RespectPlans bool

	// RepairOverslept lets a structurally impossible reset-parked plan count as
	// due, re-checked under the write lock. With RespectPlans cleared it also
	// turns off escalation: due plans and stale impossible plans win, but a
	// valid future plan does not.
	RepairOverslept bool
}

// rowEligible answers fetch eligibility under the write lock.
//
// Deciding eligibility on a lock-free read and then claiming separately lets
// two collectors both pass the check and both fetch. Re-checking here closes
// that window.
func rowEligible(r *row, now time.Time, mode Mode) bool {
	if r.AuthDeadStrikes >= AuthDeadStrikes {
		return false
	}
	if backoff := fromEpoch(r.BackoffUntil); !backoff.IsZero() && now.Before(backoff) {
		return false
	}
	if liveClaim(fromEpoch(r.ClaimUntil), fromEpoch(r.LastAttemptAt), now) {
		return false
	}

	fetchedAt := fromEpoch(r.FetchedAt)
	stale := fetchedAt.IsZero() || now.Sub(fetchedAt) > pollpolicy.ServeTTL

	nextPollAt := fromEpoch(r.NextPollAt)
	pollDue := !nextPollAt.IsZero() && !now.Before(nextPollAt)
	unplanned := nextPollAt.IsZero()
	overslept := mode.RepairOverslept &&
		planOversleeps(nextPollAt, durationFromSeconds(r.PollIntervalS), now)

	if mode.RespectPlans {
		return stale && (pollDue || unplanned || overslept)
	}
	if mode.RepairOverslept {
		return pollDue || (stale && (unplanned || overslept))
	}
	return pollDue || stale
}
