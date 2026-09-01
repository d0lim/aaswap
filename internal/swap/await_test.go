package swap

import (
	"context"
	"errors"
	"testing"
	"time"
)

// fastAwait is the wait with its timing collapsed. The behavior under test is
// which observations count as a login, never how long the poll takes.
func fastAwait(onWaiting func(LiveState)) AwaitOptions {
	return AwaitOptions{Interval: time.Millisecond, Confirmations: 2, OnWaiting: onWaiting}
}

// The whole point of the wait: a person leaves, runs /login, and comes back to
// find the account already captured.
func TestAwaitReturnsTheLoginThatLandsWhileWaiting(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()

	// The first report happens before the first poll, so landing the login
	// from inside it is the whole schedule — no sleeping, no goroutine.
	landed := false
	identity, err := f.AwaitNewLogin(t.Context(), fastAwait(func(LiveState) {
		if landed {
			return
		}
		landed = true
		f.setLiveIdentity("three@example.com", "org-3", "Acme", "acct-3")
	}))
	if err != nil {
		t.Fatal(err)
	}
	if identity.Email != "three@example.com" || identity.OrganizationUUID != "org-3" {
		t.Errorf("awaited identity = %+v, want three@example.com in org-3", identity)
	}
}

// Re-logging in as an account ccswap already stores is a credential refresh,
// which is exactly as worth waiting for as a new account. Add sorts out which
// one it is.
func TestAwaitAcceptsALoginAsAnAlreadyStoredAccount(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts() // live as one@example.com

	landed := false
	identity, err := f.AwaitNewLogin(t.Context(), fastAwait(func(LiveState) {
		if landed {
			return
		}
		landed = true
		f.setLiveIdentity("two@example.com", "", "", "acct-2")
	}))
	if err != nil {
		t.Fatal(err)
	}
	if identity.Email != "two@example.com" {
		t.Errorf("awaited identity = %+v, want two@example.com", identity)
	}
}

// /login can clear the config before it writes the new account. Returning on
// the gap would hand the caller a logged-out machine and call it a login.
func TestAwaitIgnoresTheLoggedOutGapMidLogin(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()

	step := 0
	identity, err := f.AwaitNewLogin(t.Context(), fastAwait(func(state LiveState) {
		step++
		switch step {
		case 1:
			f.clearLiveIdentity()
		case 2:
			if state.LoggedIn {
				t.Errorf("report %d = %+v, want the logged-out gap", step, state)
			}
			f.setLiveIdentity("three@example.com", "", "", "acct-3")
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if identity.Email != "three@example.com" {
		t.Errorf("awaited identity = %+v, want three@example.com", identity)
	}
}

// The account that is already live is what the caller could have captured
// without waiting at all. Returning it would make `add --wait` a no-op that
// looks like it worked.
func TestAwaitNeverReturnsTheAccountThatWasAlreadyLive(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.AwaitNewLogin(ctx, fastAwait(nil)); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("AwaitNewLogin = %v, want it to still be waiting at the deadline", err)
	}
}

// Cancelling is the only way out, and it must change nothing.
func TestAwaitStopsOnCancellation(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := f.AwaitNewLogin(ctx, fastAwait(nil)); !errors.Is(err, context.Canceled) {
		t.Errorf("AwaitNewLogin = %v, want context.Canceled", err)
	}
}

// A caller has to be able to say what it is watching, or the wait is an
// unexplained hang. The slot label is what turns "logged in as one@example.com"
// into "that is the account you already have".
func TestAwaitReportsWhatIsLiveAndWhichSlotHoldsIt(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()

	var reports []LiveState
	landed := false
	if _, err := f.AwaitNewLogin(t.Context(), fastAwait(func(state LiveState) {
		reports = append(reports, state)
		if landed {
			return
		}
		landed = true
		f.setLiveIdentity("three@example.com", "", "", "acct-3")
	})); err != nil {
		t.Fatal(err)
	}

	if len(reports) != 2 {
		t.Fatalf("reports = %+v, want the starting login and the one that landed", reports)
	}
	if got := reports[0]; !got.LoggedIn || got.Identity.Email != "one@example.com" || got.Slot != "1" {
		t.Errorf("first report = %+v, want one@example.com in slot 1", got)
	}
	// The new account is in no slot yet — that is what makes it worth adding.
	if got := reports[1]; !got.LoggedIn || got.Identity.Email != "three@example.com" || got.Slot != "" {
		t.Errorf("second report = %+v, want three@example.com in no slot", got)
	}
}
