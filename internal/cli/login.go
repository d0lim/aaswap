package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/swap"
	"github.com/d0lim/aaswap/internal/usagestore"
	"github.com/spf13/cobra"
)

// Choice is one answer a prompt offers.
type Choice struct {
	// Key is what the person types. One character, lowercased.
	Key string
	// Label says what choosing it does.
	Label string
}

// loginRequest is what one `login` invocation asked for.
type loginRequest struct {
	name    string
	email   string
	token   string
	capture bool
	wait    bool
}

func (a *App) loginCommand() *cobra.Command {
	var req loginRequest
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Store an account, logging in first if you need to",
		Long: "aaswap cannot log you in — Claude Code owns that flow — so storing an\n" +
			"account means being logged in as it, or going and logging in.\n\n" +
			"With no flags this looks at what is live and says what it will do. The\n" +
			"one case it cannot decide alone is a live login already stored: refresh\n" +
			"that account, or wait for a different one? Both are ordinary, so it\n" +
			"asks rather than guessing.\n\n" +
			"The flags are three answers to that question, for scripts and for\n" +
			"people who already know:\n" +
			"  --capture   store the account logged in now\n" +
			"  --wait      wait for a /login elsewhere, then store that account\n" +
			"  --token     read a setup token or API key from stdin\n\n" +
			"Without a terminal, nothing is asked and nothing waits: an unstored\n" +
			"live login is captured, a stored one is refreshed, and no login at all\n" +
			"is an error.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runLogin(cmd, req)
		},
	}
	cmd.Flags().StringVar(&req.name, "name", "", "give the account a name")
	cmd.Flags().BoolVar(&req.capture, "capture", false,
		"store the account logged in now, without asking")
	cmd.Flags().BoolVar(&req.wait, "wait", false,
		"wait for a /login in Claude Code, then store that account")
	cmd.Flags().StringVar(&req.token, "token", "",
		`a setup token or managed API key, or "-" to read one from stdin`)
	cmd.Flags().StringVar(&req.email, "email", "",
		"with --token, label the account with an address (a placeholder is "+
			"synthesized otherwise)")
	silenceUsage(cmd)
	return cmd
}

func (a *App) runLogin(cmd *cobra.Command, req loginRequest) error {
	// Three answers to one question. Naming two is not a request that can be
	// satisfied, and picking one silently would do something unasked.
	named := 0
	for _, given := range []bool{req.capture, req.wait, req.token != ""} {
		if given {
			named++
		}
	}
	if named > 1 {
		return fmt.Errorf("%w: --capture, --wait and --token are three answers to "+
			"the same question; give at most one", apperr.ErrValidation)
	}

	if req.token != "" {
		return a.runAddToken(req.token, req.email, req.name)
	}

	s, err := a.switcher()
	if err != nil {
		return err
	}
	switch {
	case req.wait:
		if err := a.awaitLogin(cmd.Context(), s); err != nil {
			return err
		}
	case !req.capture:
		decision, err := a.decideLogin(s)
		if err != nil {
			return err
		}
		switch decision {
		case loginCancel:
			a.printer.Println(a.printer.Dimmed("Cancelled"))
			return nil
		case loginWait:
			if err := a.awaitLogin(cmd.Context(), s); err != nil {
				return err
			}
		}
	}
	return a.captureInto(cmd.Context(), s, req.name)
}

// loginDecision is what the interactive path settled on.
type loginDecision int

const (
	// loginCapture stores what is live now, which is also what a refresh is:
	// Add tells the two apart from the identity itself.
	loginCapture loginDecision = iota
	loginWait
	loginCancel
)

// decideLogin works out what `login` with no flags should do.
//
// Four states, and only one of them is a question. The rest have a single
// plausible answer, and asking a question whose answer is already known trains
// people to stop reading prompts.
func (a *App) decideLogin(s *swap.Switcher) (loginDecision, error) {
	state, err := s.LiveState()
	if err != nil {
		return loginCancel, err
	}

	// Nothing live. Capturing is impossible and there is nothing to ask about,
	// so the wait IS the answer — but only where someone can act on it.
	if !state.LoggedIn {
		if !a.interactive() {
			return loginCapture, nil // Add reports the unchanged error
		}
		return loginWait, nil
	}

	// Live but unstored: exactly what the person asked for.
	if state.Slot == "" {
		return loginCapture, nil
	}

	// Live and stored. The one genuinely ambiguous case — unless the stored
	// credential is dead, in which case being logged in as it again has one
	// plausible reason.
	if !a.interactive() || a.tokenIsDead(s, state) {
		return loginCapture, nil
	}

	answer := a.choose(fmt.Sprintf(
		"Logged in as %s [%s] — already stored as %s.",
		state.Identity.Email, state.Identity.DisplayTag(), state.Slot),
		[]Choice{
			{Key: "r", Label: "refresh that account's stored credential"},
			{Key: "w", Label: "wait for a different login, then add it"},
			{Key: "q", Label: "cancel"},
		})
	switch answer {
	case "r":
		return loginCapture, nil
	case "w":
		return loginWait, nil
	}
	return loginCancel, nil
}

// tokenIsDead reports whether the live account's stored credential has already
// been refused by the server.
//
// Best effort: an unreadable measurement table is not evidence either way, and
// the cost of being wrong is one extra prompt.
func (a *App) tokenIsDead(s *swap.Switcher, state swap.LiveState) bool {
	if s.Usage == nil || state.Slot == "" {
		return false
	}
	ids := map[string]usagestore.Identity{state.Slot: {
		Email:            state.Identity.Email,
		OrganizationUUID: state.Identity.OrganizationUUID,
	}}
	return s.Usage.Entries(ids, nil)[state.Slot].TokenDead("")
}

// interactive reports whether there is a person to ask.
func (a *App) interactive() bool {
	if a.json {
		return false
	}
	if a.Choose != nil {
		return true
	}
	return isTerminal(a.Out) && isTerminal(a.In)
}

// choose asks a multiple-choice question, returning the key chosen.
//
// An unreadable or unrecognized answer is the LAST option, which is cancel
// everywhere this is used: a stray newline must never be taken as consent to
// touch a credential.
func (a *App) choose(prompt string, options []Choice) string {
	if a.Choose != nil {
		return a.Choose(prompt, options)
	}
	a.printer.Blank()
	a.printer.Println("  ", a.printer.Bold(prompt))
	a.printer.Blank()
	for _, option := range options {
		a.printer.Println("    ", a.printer.Accent("["+option.Key+"]"), " ", option.Label)
	}
	a.printer.Blank()

	keys := make([]string, len(options))
	for i, option := range options {
		keys[i] = option.Key
	}
	a.printer.Printf("  [%s] ", strings.Join(keys, "/"))
	var answer string
	if _, err := fmt.Fscanln(a.In, &answer); err != nil {
		a.printer.Blank()
		return options[len(options)-1].Key
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	for _, option := range options {
		if answer == option.Key {
			return option.Key
		}
	}
	return options[len(options)-1].Key
}

// captureInto stores whatever is live now under an optional name.
func (a *App) captureInto(ctx context.Context, s *swap.Switcher, name string) error {
	outcome, err := s.Add(ctx, swap.AddRequest{
		Name: name, AssumeYes: a.assumeYes, Confirm: a.confirm,
	})
	if err != nil {
		return err
	}
	return a.reportAdd(outcome)
}
