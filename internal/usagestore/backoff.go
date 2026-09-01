package usagestore

import (
	"time"

	"github.com/d0lim/ccswap/internal/pollpolicy"
	"github.com/d0lim/ccswap/internal/usage"
)

// earliestReset is when the soonest window that gates this account rolls over,
// reporting false when no window states one.
//
// The SOONEST is what matters: once it resets, usage there is zeroed and the
// whole measurement is obsolete, so a later window's reset cannot rescue it. A
// window carrying no reset contributes nothing rather than a guess, so partial
// metadata can only tighten the bound, never loosen it.
func earliestReset(result *usage.Result, models []string) (time.Time, bool) {
	var earliest time.Time
	found := false
	for _, w := range result.Windows(models) {
		ts, ok := w.ResetTime()
		if !ok {
			continue
		}
		if !found || ts.Before(earliest) {
			earliest, found = ts, true
		}
	}
	return earliest, found
}

// rateLimitedTrustOK reports whether a measurement frozen by a 429 is still
// trustworthy for decisions.
//
// Usage rises monotonically within a window, so a frozen measurement is a valid
// lower bound until its window resets — but only up to a client-side ceiling,
// so a far-future or malformed reset can never grant unbounded trust. The bound
// is the earlier of:
//
//   - the earliest future window reset, after which the measurement is simply
//     obsolete; if the earliest known reset is already past, the value is
//     untrusted outright regardless of any farther-future window;
//   - [RateLimitTrustMaxAge] past the measurement, which applies whether or not
//     any reset is known.
//
// models selects the per-model scoped windows that also gate the account, so
// their resets are considered too — matching the scheduler's window view.
func rateLimitedTrustOK(result *usage.Result, age time.Duration, hasAge bool, now time.Time, models []string) bool {
	if !hasAge {
		return false
	}
	bound := now.Add(RateLimitTrustMaxAge - age)
	if soonest, ok := earliestReset(result, models); ok && soonest.Before(bound) {
		bound = soonest
	}
	return now.Before(bound)
}

// failureBackoff is how long to hold an account off after a failed fetch.
//
// The base is a plain exponential curve on the consecutive-failure count. A
// Retry-After overrides it, but how depends on which rule the server is
// applying, and the two are told apart by the value itself:
//
//   - Retry-After: 0 on a 429 is the saturated-budget edge. The trailing hour's
//     budget is spent and frees only as old requests age out, so an immediate
//     retry mostly prolongs the oscillating state. Wait at least
//     pollpolicy.EdgeBackoff before probing again.
//   - Retry-After: N>0 is the burst rule — several rapid requests on one token
//     produce a hard block whose deadline the server reports accurately, counts
//     down, and does not extend when probed. Honored as the wait, plus
//     [RetryAfterMargin] for a long ask, and bounded so a pathological header
//     can never park an account for hours.
//
// rateLimited gates ONLY the margin — whether evidence measured on
// usage-endpoint 429 blocks applies at all. Retry-After is parsed for any HTTP
// status, and the usage endpoint sits behind a proxy that emits it on 503s as
// routine overload signaling, so a non-429 ask must not inherit a margin
// derived from hour-scale 429 blocks.
func failureBackoff(consecutiveFailures int, retryAfter *time.Duration, rateLimited bool) time.Duration {
	shift := min(max(consecutiveFailures-1, 0), backoffMaxShift)
	computed := min(BackoffBase<<shift, BackoffCap)

	if retryAfter == nil {
		return computed
	}
	asked := *retryAfter

	if asked == 0 {
		if !rateLimited {
			// Retry-After: 0 on a non-429 — a proxy saying "retry now" — is
			// not the saturated-budget edge; that rule was measured on this
			// endpoint's 429s alone. Fall through to the plain curve, as if
			// there were no header.
			return computed
		}
		return min(max(computed, pollpolicy.EdgeBackoff), BackoffCap)
	}

	// The margin applies only above the cap, because a short ask was separately
	// measured as accurate — not because the curve out-waits it. It does not:
	// at one failure the curve is 30s, so a 300s ask lands exactly on the
	// server's deadline.
	if asked > BackoffCap && rateLimited {
		asked += RetryAfterMargin
	}

	// Every ask is bounded, but by the ceiling ITS OWN arm's trust actually
	// uses. A 429-stale row stays decision-trusted up to RateLimitTrustMaxAge,
	// so RetryAfterFloorCap sits comfortably inside it. A non-429 row is read
	// unknown once TrustMaxAge elapses past the last success, so bounding it at
	// RetryAfterFloorCap would park it 900s past its own trust — blind, being
	// neither pollable nor usable, for that whole margin. One shared constant
	// was measurably wrong here.
	if rateLimited {
		asked = min(asked, RetryAfterFloorCap)
	} else {
		asked = min(asked, TrustMaxAge)
	}

	// No trim against the server's deadline. Cutting a long wait back to the
	// point where stored trust expires cannot salvage that trust — the floor
	// keeps the wait at or above the ask, so the row is untrusted at release
	// either way. It only lands the retry ON the deadline, where a re-block
	// earns a fresh hour. Measured over reset offsets against an hour-long
	// block, trimming cost 3.95 requests and 73,525 unknown-seconds against
	// 1.00 and 1,406 without it: worse on both axes, because this 429 is a
	// request-rate block that a quota-window reset cannot lift, while every
	// extra retry is itself a request inside the trailing hour it is retrying
	// against.
	return max(asked, computed)
}
