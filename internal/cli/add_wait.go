package cli

import (
	"context"
	"fmt"

	"github.com/d0lim/aaswap/internal/swap"
)

// awaitLogin blocks until a different account is logged in, narrating the wait.
//
// aaswap cannot log anyone in — Claude Code owns the OAuth flow — so adding a
// second account has always been two steps with a person in the middle: go run
// /login, come back, run `aaswap add`. This closes the gap from aaswap's side.
// It cannot drive the login, but it can tell the person exactly what to do and
// be watching when they finish.
func (a *App) awaitLogin(ctx context.Context, s *swap.Switcher) error {
	first := true
	opts := a.awaitTuning
	opts.OnWaiting = func(state swap.LiveState) {
		if first {
			first = false
			a.printWaitInstructions(state)
			return
		}
		a.printWaitProgress(state)
	}
	identity, err := s.AwaitNewLogin(ctx, opts)
	if err != nil {
		return err
	}
	a.printer.Println(a.printer.Accent("Logged in as "),
		identity.Email, a.printer.Muted(" ["+identity.DisplayTag()+"]"))
	return nil
}

// printWaitInstructions says what the person has to go and do.
func (a *App) printWaitInstructions(state swap.LiveState) {
	a.printer.Blank()
	switch {
	case !state.LoggedIn:
		a.printer.Println(a.printer.Bold("No account is logged in."))
	case state.Slot != "":
		a.printer.Println(a.printer.Bold("Currently logged in as "), state.Identity.Email,
			a.printer.Muted(" ["+state.Identity.DisplayTag()+"]"),
			a.printer.Dimmed(fmt.Sprintf(" — already stored as account %s.", state.Slot)))
	default:
		a.printer.Println(a.printer.Bold("Currently logged in as "), state.Identity.Email,
			a.printer.Muted(" ["+state.Identity.DisplayTag()+"]"))
	}
	a.printer.Blank()
	a.printer.Println(a.printer.Dimmed("  Log in with the account you want to add:"))
	a.printer.Blank()
	a.printer.Println("      ", a.printer.Accent("claude"),
		a.printer.Dimmed("   then run  "), a.printer.Accent("/login"))
	a.printer.Blank()
	if state.LoggedIn {
		// The warning the README carries, said at the only moment it can still
		// be acted on. Current Claude Code revokes the refresh token on
		// /logout, and the account being left is one aaswap is storing.
		a.printer.Println(a.printer.Dimmed(
			"  Do not run /logout first — it can revoke the token stored for "),
			a.printer.Dimmed(state.Identity.Email), a.printer.Dimmed("."))
		a.printer.Blank()
	}
	a.printer.Println(a.printer.Dimmed("  Waiting… "), a.printer.Muted("(Ctrl-C to stop)"))
}

// printWaitProgress narrates a change that is not yet the finished login.
//
// A login is not one write, so the wait deliberately sits through the moment
// the config is cleared. Saying so beats a cursor that stops moving.
func (a *App) printWaitProgress(state swap.LiveState) {
	if !state.LoggedIn {
		a.printer.Println(a.printer.Dimmed("  Logged out — waiting for the new login…"))
		return
	}
	a.printer.Println(a.printer.Dimmed("  Now logged in as "), state.Identity.Email,
		a.printer.Muted(" ["+state.Identity.DisplayTag()+"]"))
}

// shouldWaitForLogin reports whether an `add` with nothing to capture should
// wait instead of failing.
//
// Only when a person is there to act on it. With no live login `add` has
// exactly one outcome — an error telling the reader to go log in — and a
// terminal that can print that can equally well wait for them to do it. A
// script gets the error it has always got: --json is a machine asking, and a
// pipe has no one to instruct.
func (a *App) shouldWaitForLogin(s *swap.Switcher) bool {
	if a.json || !isTerminal(a.Out) {
		return false
	}
	_, loggedIn := s.LiveIdentity()
	return !loggedIn
}
