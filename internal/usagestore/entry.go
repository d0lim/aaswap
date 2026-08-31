package usagestore

import (
	"time"

	"github.com/realiti4/claude-swap/internal/claudeapi"
	"github.com/realiti4/claude-swap/internal/pollpolicy"
	"github.com/realiti4/claude-swap/internal/usage"
)

// Entry is one account's usage state at collect time.
//
// Every time field is a zero [time.Time] when the table holds no value, which
// is what "unknown" means throughout this package — never a zero epoch and
// never a sentinel number.
type Entry struct {
	// Sentinel is the collector's live overlay — "api key", "token expired" —
	// and is never persisted. See [WithSentinel].
	Sentinel string

	// LastGood is the last successful measurement, and FetchedAt when it was
	// taken. Age is their difference at snapshot time, meaningful only when
	// FetchedAt is set.
	LastGood  *usage.Result
	FetchedAt time.Time
	Age       time.Duration

	LastAttemptAt       time.Time
	ConsecutiveFailures int
	LastError           claudeapi.ErrorKind
	BackoffUntil        time.Time

	NextPollAt   time.Time
	PollInterval time.Duration

	// Last429At is when this token last answered 429, at any Retry-After.
	// Deliberately NOT cleared by a later success: the planner keeps the
	// cadence floored until the saturated rolling window has had time to age
	// out.
	Last429At time.Time

	// AuthDeadStrikes counts consecutive permanent auth verdicts, and
	// StruckFingerprint names the credential generation they condemned. An
	// absent fingerprint — a row written before they were recorded — binds
	// unconditionally.
	AuthDeadStrikes   int
	StruckFingerprint string

	// TrustExtended marks staleness past [StaleOK] that is nonetheless
	// deliberate: the server is refusing fresher data, or the scheduler itself
	// chose the cadence. Computed by [Store.Entries] and capped at the relevant
	// trust ceiling.
	TrustExtended bool

	// ClaimUntil is when another collector's fetch lease expires.
	ClaimUntil time.Time
}

// Fresh reports whether the measurement is young enough to serve without
// fetching at all.
func (e Entry) Fresh(now time.Time, ttl time.Duration) bool {
	return !e.FetchedAt.IsZero() && now.Sub(e.FetchedAt) <= ttl
}

// InBackoff reports whether a failed fetch is still holding this account off.
func (e Entry) InBackoff(now time.Time) bool {
	return !e.BackoffUntil.IsZero() && now.Before(e.BackoffUntil)
}

// Recent429 reports whether this token 429'd recently enough to keep the
// post-429 cadence floored.
//
// Recency is measured from when the 429's honored backoff LIFTS, not from the
// 429 itself. An hour-scale Retry-After is honored as one long backoff during
// which no attempt runs, so the only stamp is at the block's start; measuring
// from the stamp would make the first post-block success — which cannot happen
// until the backoff lifts — see a window that has already fully elapsed. The
// AIMD growth and the post-429 floor would then never engage, and machines
// sharing the token could not converge.
//
// The anchor moves to the backoff only while the LIVE backoff is a 429 backoff.
// Last429At is never cleared, but BackoffUntil and LastError are rewritten by
// any later failure, so without the error guard an unrelated timeout on a token
// that 429'd long ago would install a fresh backoff and spuriously re-arm the
// post-429 cadence.
func (e Entry) Recent429(now time.Time) bool {
	if e.Last429At.IsZero() {
		return false
	}
	anchor := e.Last429At
	if e.LastError == claudeapi.HTTPKind(429) && e.BackoffUntil.After(anchor) {
		anchor = e.BackoffUntil
	}
	return now.Before(anchor.Add(pollpolicy.Recent429Window))
}

// Claimed reports whether another collector's bounded fetch lease is live.
func (e Entry) Claimed(now time.Time) bool {
	return liveClaim(e.ClaimUntil, e.LastAttemptAt, now)
}

// TokenDead reports whether the stored credential's refresh-token lineage is
// provably dead, so the account is quarantined and surfaced as needing a
// re-login.
//
// Strikes condemn the GENERATION that was POSTed, not the slot. When storedFP
// names the credential the slot currently holds and it differs from the struck
// one, the credential has been replaced since the verdict and the strike no
// longer applies — so any path that writes a credential heals the quarantine
// with no bespoke clear call. Passing an empty storedFP asks the unconditional
// question, and a row struck before fingerprints were recorded binds
// unconditionally either way.
func (e Entry) TokenDead(storedFP string) bool {
	if e.AuthDeadStrikes < AuthDeadStrikes {
		return false
	}
	return storedFP == "" || e.StruckFingerprint == "" || storedFP == e.StruckFingerprint
}

// Decision is the value a switch decision runs on.
type Decision struct {
	// Sentinel, when set, decides on its own.
	Sentinel string
	// Usage is the trusted measurement when Sentinel is empty.
	Usage *usage.Result
}

// DecisionValue returns what a switch decision may act on, and whether anything
// is known at all.
//
// A sentinel wins. Otherwise the last-good measurement, while it is recent
// enough to trust — within [StaleOK], or deliberately stale per
// [Entry.TrustExtended]. Otherwise nothing, which callers must treat as
// "unknown" and never as a reason to skip the account.
//
// Display code reads LastGood and Age directly instead: it may show older data
// as long as it annotates the age.
func (e Entry) DecisionValue() (Decision, bool) {
	if e.Sentinel != "" {
		return Decision{Sentinel: e.Sentinel}, true
	}
	if e.LastGood != nil && !e.FetchedAt.IsZero() && (e.Age <= StaleOK || e.TrustExtended) {
		return Decision{Usage: e.LastGood}, true
	}
	return Decision{}, false
}

// WithSentinel overlays a derived sentinel state on a stored entry. Read model
// only — a sentinel is never written to the table.
func WithSentinel(entry Entry, sentinel string) Entry {
	if sentinel == "" {
		return entry
	}
	entry.Sentinel = sentinel
	return entry
}

// liveClaim reports whether a collector's fetch lease on a row is still live.
//
// The fenced lease wins when present; a row written by an older process falls
// back to its attempt time plus [LegacyClaimTTL]. This is the single source of
// truth for lease liveness — [Entry.Claimed], [Store.Entries] and rowEligible
// must all agree, or two collectors double-fetch.
func liveClaim(claimUntil, lastAttemptAt, now time.Time) bool {
	if !claimUntil.IsZero() {
		return now.Before(claimUntil)
	}
	return !lastAttemptAt.IsZero() && now.Sub(lastAttemptAt) < LegacyClaimTTL
}
