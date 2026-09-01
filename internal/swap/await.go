package swap

import (
	"cmp"
	"context"
	"time"
)

// DefaultAwaitInterval is how often Claude Code's config is re-read while
// waiting for a login.
//
// Fast enough that finishing a browser login feels like it registered
// immediately, slow enough that the poll costs nothing: this reads one small
// JSON file per tick.
const DefaultAwaitInterval = 500 * time.Millisecond

// DefaultAwaitConfirmations is how many consecutive polls must agree before a
// newly seen login counts as finished.
//
// A login is not one write. Claude Code writes the config and the credential
// separately, and a capture that fires between them reads one account's address
// beside another account's token — which the ownership check then refuses,
// turning a successful login into an error the person cannot act on. Waiting
// for the identity to hold still costs a second and closes most of that window;
// the ownership check remains the actual guarantee.
//
// Counted in polls rather than measured as a duration so the wait is
// deterministic under a test's injected clock.
const DefaultAwaitConfirmations = 3

// LiveState is one observation of the machine's Claude Code login.
type LiveState struct {
	// Identity is who is logged in, meaningful only when LoggedIn.
	Identity LiveIdentity
	// LoggedIn is false when there is no active login to read.
	LoggedIn bool
	// Slot names the roster slot already holding this identity, and is empty
	// when the account is not one aaswap manages.
	Slot string
}

// LiveState reads who is logged in and which slot, if any, already holds them.
//
// One read of the config paired with one read of the roster: a caller that
// asked the two questions separately could pair an identity with a roster from
// either side of a concurrent add.
func (s *Switcher) LiveState() (LiveState, error) {
	roster, err := s.RosterOrEmpty()
	if err != nil {
		return LiveState{}, err
	}
	identity, loggedIn := s.LiveIdentity()
	return liveState(roster, identity, loggedIn), nil
}

func liveState(roster *Roster, identity LiveIdentity, loggedIn bool) LiveState {
	state := LiveState{Identity: identity, LoggedIn: loggedIn}
	if loggedIn {
		state.Slot, _ = roster.FindSlot(identity.Identity())
	}
	return state
}

// AwaitOptions tunes [Switcher.AwaitNewLogin].
type AwaitOptions struct {
	// Interval between reads of Claude Code's config. Zero uses
	// [DefaultAwaitInterval].
	Interval time.Duration
	// Confirmations is how many consecutive polls must agree before a newly
	// seen login is returned. Zero uses [DefaultAwaitConfirmations].
	Confirmations int
	// OnWaiting reports what is live — once before the first poll, and again
	// whenever it changes — so a caller can keep a person informed about what
	// it is watching. Nil waits silently.
	OnWaiting func(LiveState)
}

// AwaitNewLogin blocks until Claude Code's live login names a DIFFERENT account
// than it did when the wait began.
//
// This exists because aaswap cannot log anyone in. Claude Code owns the OAuth
// flow, so registering a second account means a person leaving aaswap, running
// /login, and coming back — and `aaswap add` refusing outright in the meantime
// is a dead end rather than an instruction. Waiting turns the two halves into
// one command.
//
// "Different" is measured against the login present at the start, not against
// the roster, so re-logging in as an account aaswap already stores also
// satisfies the wait: that is a credential refresh, and [Switcher.Add] handles
// it in place. What it will never do is return the account that was already
// live, because capturing that is what the caller could have done without
// waiting at all.
//
// The context is the only way out. Cancelling returns its error and changes
// nothing.
func (s *Switcher) AwaitNewLogin(ctx context.Context, opts AwaitOptions) (LiveIdentity, error) {
	interval := cmp.Or(opts.Interval, DefaultAwaitInterval)
	confirmations := cmp.Or(opts.Confirmations, DefaultAwaitConfirmations)

	// Read once, up front. The roster only supplies the slot label in the
	// progress report, and re-reading it every tick would put a store read on
	// a loop that has no reason to take one.
	roster, err := s.RosterOrEmpty()
	if err != nil {
		return LiveIdentity{}, err
	}
	report := func(identity LiveIdentity, loggedIn bool) {
		if opts.OnWaiting == nil {
			return
		}
		opts.OnWaiting(liveState(roster, identity, loggedIn))
	}

	baseline, loggedIn := s.LiveIdentity()
	report(baseline, loggedIn)
	// Compared as the composite that names an account, so a login that only
	// rewrites the organization NAME — a rename upstream — is not mistaken for
	// a different account.
	was := baseline.Identity()

	seen, seenLoggedIn := was, loggedIn
	streak := 0

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return LiveIdentity{}, ctx.Err()
		case <-ticker.C:
		}

		identity, loggedIn := s.LiveIdentity()
		if identity.Identity() != seen || loggedIn != seenLoggedIn {
			report(identity, loggedIn)
			seen, seenLoggedIn = identity.Identity(), loggedIn
			streak = 0
		}
		// A logged-out moment is part of a login, not the end of one: /login
		// can clear the config before it writes the new account.
		if !loggedIn || identity.Identity() == was {
			continue
		}
		streak++
		if streak >= confirmations {
			return identity, nil
		}
	}
}
