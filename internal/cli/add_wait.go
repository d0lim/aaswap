package cli

import (
	"context"
	"fmt"

	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/swap"
)

// awaitLogin blocks until a different account is logged in, narrating the wait.
//
// --wait: the login happens in the tool's own profile, and aaswap says what to
// run and is watching when it lands. The default `login` runs the tool into a
// sandbox instead; this is for whoever wants the live profile to be the one
// that logs in.
func (a *App) awaitLogin(ctx context.Context, s *swap.Switcher) error {
	first := true
	opts := a.awaitTuning
	opts.OnWaiting = func(state swap.LiveState) {
		if first {
			first = false
			a.printWaitInstructions(s.Spec(), state)
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
func (a *App) printWaitInstructions(spec provider.Spec, state swap.LiveState) {
	a.printer.Blank()
	switch {
	case !state.LoggedIn:
		a.printer.Println(a.printer.Bold("No account is logged in."))
	case state.Slot != "":
		a.printer.Println(a.printer.Bold("Currently logged in as "), state.Identity.Email,
			a.printer.Muted(" ["+state.Identity.DisplayTag()+"]"),
			a.printer.Dimmed(fmt.Sprintf(" — already stored as %s.", state.Slot)))
	default:
		a.printer.Println(a.printer.Bold("Currently logged in as "), state.Identity.Email,
			a.printer.Muted(" ["+state.Identity.DisplayTag()+"]"))
	}
	a.printer.Blank()
	a.printer.Println(a.printer.Dimmed("  Log in with the account you want to add:"))
	a.printer.Blank()
	// The instruction is the declaration's, not Claude's: "claude, then /login"
	// told a Codex user to run a command Codex does not have.
	launch, then := spec.Login.Steps()
	switch {
	case launch == "":
		a.printer.Println("      ", a.printer.Dimmed("log in with "+spec.DisplayName()))
	case then == "":
		a.printer.Println("      ", a.printer.Accent(launch))
	default:
		a.printer.Println("      ", a.printer.Accent(launch),
			a.printer.Dimmed("   then run  "), a.printer.Accent(then))
	}
	a.printer.Blank()
	if state.LoggedIn {
		// The warning the README carries, said at the only moment it can still
		// be acted on. Current Claude Code revokes the refresh token on
		// /logout, and the account being left is one aaswap is storing.
		a.printer.Println(a.printer.Dimmed(
			"  Do not log out first — the tool may revoke the token stored for "),
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
