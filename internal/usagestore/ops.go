package usagestore

import (
	"maps"
	"slices"
	"time"

	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/pollpolicy"
	"github.com/d0lim/aaswap/internal/usage"
)

// FetchRecord is the outcome of one fetch attempt, as handed to [Store.Record].
//
// Exactly one of three shapes:
//   - success: Error and Sentinel both empty. Usage may still be nil, when the
//     response carried no window data.
//   - failure: Error set, optionally with RetryAfter.
//   - sentinel: Sentinel set. Recorded as a near-no-op, because sentinels are
//     re-derived every pass and never persisted.
type FetchRecord struct {
	Usage      *usage.Result
	Error      claudeapi.ErrorKind
	RetryAfter *time.Duration
	Sentinel   string

	// StruckFP fingerprints the credential whose refresh token was POSTed when
	// a permanent auth verdict came back. Strikes bind to it, so
	// [Entry.TokenDead] holds only while the stored credential still
	// fingerprints to the struck generation — which means any
	// credential-writing path heals the quarantine without a bespoke call.
	StruckFP string
}

// Entries returns an identity-guarded snapshot for the given slots.
//
// A slot whose row is missing, or whose row belongs to a different account, gets
// a zero [Entry] rather than being omitted, so callers can iterate their own
// slot set without checking for presence.
//
// models are the configured scoped-window model names. They let the 429-stale
// trust bound honor per-model window resets too, matching the scheduler's view.
// Callers that only read timestamps and the last-good measurement can pass nil.
func (s *Store) Entries(identities map[string]Identity, models []string) map[string]Entry {
	now := s.Now()
	rows := s.readRows()
	out := make(map[string]Entry, len(identities))

	for num, identity := range identities {
		r := rows[num]
		if !r.matches(identity) {
			out[num] = Entry{}
			continue
		}

		fetchedAt := fromEpoch(r.FetchedAt)
		hasAge := !fetchedAt.IsZero()
		var age time.Duration
		if hasAge {
			age = now.Sub(fetchedAt)
		}

		nextPollAt := fromEpoch(r.NextPollAt)
		claimUntil := fromEpoch(r.ClaimUntil)
		lastAttemptAt := fromEpoch(r.LastAttemptAt)

		// A 429 throttles polling without moving the account's real windows, so
		// its stale measurement stays a lower bound until the earliest window
		// resets. Any other failure is no evidence the measurement still holds
		// and uses the general ceiling.
		var withinCeiling bool
		if r.LastError == claudeapi.HTTPKind(429) {
			withinCeiling = rateLimitedTrustOK(r.LastGood, age, hasAge, now, models)
		} else {
			withinCeiling = hasAge && age <= TrustMaxAge
		}

		// Strict Before mirrors rowEligible: AT the scheduled poll the entry is
		// due, and its staleness is no longer scheduler-chosen. A live lease
		// keeps the bridge up — when another collector just won the fetch, this
		// reader must not flip trusted to unknown for the seconds the result is
		// in flight, or an unhealthy tick gets counted for nothing.
		liveLease := liveClaim(claimUntil, lastAttemptAt, now)
		deliberate := r.ConsecutiveFailures > 0 ||
			(!nextPollAt.IsZero() && now.Before(nextPollAt)) ||
			liveLease

		out[num] = Entry{
			LastGood:            r.LastGood,
			FetchedAt:           fetchedAt,
			Age:                 age,
			LastAttemptAt:       lastAttemptAt,
			ConsecutiveFailures: r.ConsecutiveFailures,
			LastError:           r.LastError,
			BackoffUntil:        fromEpoch(r.BackoffUntil),
			NextPollAt:          nextPollAt,
			PollInterval:        durationFromSeconds(r.PollIntervalS),
			Last429At:           fromEpoch(r.Last429At),
			AuthDeadStrikes:     r.AuthDeadStrikes,
			StruckFingerprint:   r.StruckFingerprint,
			TrustExtended:       withinCeiling && deliberate,
			ClaimUntil:          claimUntil,
		}
	}
	return out
}

// mutate reads, modifies and writes the named rows under the lock. A row whose
// stored identity mismatches is replaced with a fresh one first.
func (s *Store) mutate(identities map[string]Identity, nums []string, apply func(num string, r *row)) error {
	if len(nums) == 0 {
		return nil
	}
	return s.withLock(func() error {
		rows := s.readRows()
		for _, num := range nums {
			identity := identities[num]
			if !rows[num].matches(identity) {
				rows[num] = freshRow(identity)
			}
			apply(num, rows[num])
		}
		return s.writeRows(rows)
	})
}

// Claim leases the given slots unconditionally, returning each one's fencing
// token.
//
// Unconditional is the point: the caller has already decided to fetch these.
// Use [Store.Reserve] to decide and lease in one locked pass instead.
func (s *Store) Claim(nums []string, identities map[string]Identity) (map[string]string, error) {
	if len(nums) == 0 {
		return map[string]string{}, nil
	}
	now := s.Now()
	claims := make(map[string]string, len(nums))
	for _, num := range nums {
		claims[num] = s.NewClaimID()
	}

	err := s.mutate(identities, nums, func(num string, r *row) {
		r.LastAttemptAt = toEpoch(now)
		r.ClaimID = claims[num]
		r.ClaimUntil = toEpoch(now.Add(ClaimTTL))
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// Reserve atomically wins the right to fetch: it re-checks eligibility and
// stamps a bounded lease in one locked pass, returning the fencing token for
// each slot it won.
//
// A slot absent from the result was not won — another collector holds it, it is
// in backoff, it is quarantined, or its plan says it is not due. See [Mode] for
// how plans are weighed.
func (s *Store) Reserve(nums []string, identities map[string]Identity, mode Mode) (map[string]string, error) {
	if len(nums) == 0 {
		return map[string]string{}, nil
	}
	now := s.Now()
	won := map[string]string{}

	err := s.withLock(func() error {
		rows := s.readRows()
		for _, num := range nums {
			identity := identities[num]
			r := rows[num]
			if r.matches(identity) {
				if !rowEligible(r, now, mode) {
					continue
				}
			} else {
				// A slot that changed hands has no history to be ineligible
				// on: the new account has never been fetched.
				r = freshRow(identity)
				rows[num] = r
			}
			claimID := s.NewClaimID()
			r.LastAttemptAt = toEpoch(now)
			r.ClaimID = claimID
			r.ClaimUntil = toEpoch(now.Add(ClaimTTL))
			won[num] = claimID
		}
		if len(won) == 0 {
			// Nothing was won, so nothing changed. Skipping the write keeps a
			// losing collector from rewriting the file other collectors are
			// reading.
			return nil
		}
		return s.writeRows(rows)
	})
	if err != nil {
		return nil, err
	}
	return won, nil
}

// Record merges fetch outcomes, fenced by the leases that produced them, and
// returns the slots it accepted.
//
// A late writer whose lease or slot identity has been replaced is ignored
// without touching the newer row. Success and failure are mutually exclusive
// writers: a success resets the failure fields, and a failure never touches the
// last-good measurement or its timestamp — that is what makes a failing endpoint
// degrade into staleness rather than into blankness.
//
// A success plan commits in the same transaction as its measurement, so no
// collector can slip into a gap between recording and re-planning.
//
// claims fences the write. Passing nil defers to a LIVE lease but never to an
// expired one, so a crashed claimer's leftover ticket ages out instead of
// blocking every later writer.
func (s *Store) Record(
	outcomes map[string]FetchRecord,
	identities map[string]Identity,
	claims map[string]string,
	plans map[string]pollpolicy.Plan,
) (map[string]bool, error) {
	if len(outcomes) == 0 {
		return map[string]bool{}, nil
	}
	now := s.Now()
	accepted := map[string]bool{}

	err := s.withLock(func() error {
		rows := s.readRows()
		// Sorted so the transaction is reproducible and a test can predict the
		// order rows are touched in.
		for _, num := range slices.Sorted(maps.Keys(outcomes)) {
			identity := identities[num]
			r := rows[num]

			switch {
			case claims != nil:
				// Fenced: the row must still be this account's AND still hold
				// the exact ticket this outcome was fetched under.
				expected, held := claims[num]
				if !held || !r.matches(identity) || r.ClaimID != expected {
					continue
				}
			case r != nil && r.ClaimID != "" && now.Before(fromEpoch(r.ClaimUntil)):
				// Unfenced, but someone's lease is live: defer to it.
				continue
			case !r.matches(identity):
				r = freshRow(identity)
				rows[num] = r
			}

			accepted[num] = true
			plan, hasPlan := plans[num]
			s.applyOutcome(r, outcomes[num], plan, hasPlan, now)
		}
		if len(accepted) == 0 {
			return nil
		}
		return s.writeRows(rows)
	})
	if err != nil {
		return nil, err
	}
	return accepted, nil
}

// applyOutcome writes one outcome into a row.
func (s *Store) applyOutcome(r *row, record FetchRecord, plan pollpolicy.Plan, hasPlan bool, now time.Time) {
	// Releasing the lease is the one thing every outcome does, sentinels
	// included: the fetch is over either way.
	r.ClaimID = ""
	r.ClaimUntil = epochZero()

	if record.Sentinel != "" {
		// A sentinel is re-derived every pass. Persisting one would let it
		// outlive the condition that produced it, and it says nothing about
		// whether a fetch succeeded, so no other field moves.
		return
	}

	r.LastAttemptAt = toEpoch(now)

	if record.Error == "" {
		r.LastGood = record.Usage
		r.FetchedAt = toEpoch(now)
		if hasPlan {
			// Replacing the old, possibly-due plan inside the outcome
			// transaction is what stops a collector slipping into a
			// record-then-replan gap and fetching again immediately.
			r.NextPollAt = toEpoch(plan.NextPollAt)
			r.PollIntervalS = secondsFromDuration(plan.Interval)
		}
		r.ConsecutiveFailures = 0
		r.LastError = ""
		r.BackoffUntil = nil
		// A success proves the token is alive, whatever it was struck for. The
		// fingerprint goes with it — it is meaningless at zero strikes, and a
		// later strike always overwrites it anyway.
		r.AuthDeadStrikes = 0
		r.StruckFingerprint = ""
		return
	}

	r.ConsecutiveFailures++
	r.LastError = record.Error
	rateLimited := record.Error == claudeapi.HTTPKind(429)
	if rateLimited {
		// Kept across later successes: the planner floors the cadence while a
		// 429 is recent. See [Entry.Recent429].
		r.Last429At = toEpoch(now)
	}
	r.BackoffUntil = toEpoch(now.Add(failureBackoff(r.ConsecutiveFailures, record.RetryAfter, rateLimited)))

	// Only a permanent auth verdict advances the strike count. A transient
	// error is no evidence either way and must not reset a real tally.
	if permanentAuthError(record.Error) {
		r.AuthDeadStrikes++
		// Always overwritten, including with empty: a writer that supplies no
		// fingerprint means "bind unconditionally", and it must not inherit a
		// stale fingerprint from an earlier, already-healed strike.
		r.StruckFingerprint = record.StruckFP
	}
}

// SetPollPlan persists the scheduler's per-slot cadence.
//
// A zero [pollpolicy.Plan] clears the slot's plan, which makes it immediately
// due.
func (s *Store) SetPollPlan(plans map[string]pollpolicy.Plan, identities map[string]Identity) error {
	return s.mutate(identities, slices.Sorted(maps.Keys(plans)), func(num string, r *row) {
		plan := plans[num]
		r.NextPollAt = toEpoch(plan.NextPollAt)
		r.PollIntervalS = secondsFromDuration(plan.Interval)
	})
}

// ClearDeadToken lifts the quarantine on slots whose credential was rewritten.
//
// Called after a re-login or an add replaces a slot's stored credential: the
// strike count, and the failure state riding with it, no longer reflect reality,
// and the account has to become fetch-eligible again so the next pass can prove
// the new token good. A no-op for a row with no strikes.
func (s *Store) ClearDeadToken(nums []string, identities map[string]Identity) error {
	return s.mutate(identities, nums, func(_ string, r *row) {
		r.ClaimID = ""
		r.ClaimUntil = epochZero()
		r.AuthDeadStrikes = 0
		r.StruckFingerprint = ""
		r.ConsecutiveFailures = 0
		r.LastError = ""
		r.BackoffUntil = nil
	})
}
