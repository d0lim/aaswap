package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/swap"
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
		Short: "Log in and store the account",
		Long: "Runs the tool's own login — the browser flow it would run anyway —\n" +
			"pointed at a sandbox, and stores what lands there as an account. The\n" +
			"login you already have is not read, not replaced and not logged out of;\n" +
			"on a machine with no login, the new account becomes the live one.\n\n" +
			"With no flags: a live login that is not stored yet is stored as it is,\n" +
			"and otherwise the tool is run to log in. The flags are the other ways\n" +
			"an account gets in:\n" +
			"  --capture   store the account logged in now, and nothing else\n" +
			"  --wait      wait for a login in the tool's own profile, then store it\n" +
			"  --token     store a token you already have (where the provider's\n" +
			"              format is known — `aaswap doctor` says which)\n\n" +
			"Without a terminal nothing is run and nothing waits: an unstored live\n" +
			"login is captured, a stored one is refreshed, and no login at all is\n" +
			"an error.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runLogin(cmd, req)
		},
	}
	cmd.Flags().StringVar(&req.name, "name", "", "give the account a name")
	cmd.Flags().BoolVar(&req.capture, "capture", false,
		"store the account logged in now, without running a login")
	cmd.Flags().BoolVar(&req.wait, "wait", false,
		"wait for a login in the provider's own tool, then store that account")
	cmd.Flags().StringVar(&req.token, "token", "",
		`a token to store as an account, or "-" to read one from stdin`)
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

	s, err := a.switcherForLogin()
	if err != nil {
		return err
	}
	switch {
	case req.wait:
		if err := a.awaitLogin(cmd.Context(), s); err != nil {
			return err
		}
	case req.capture || !a.interactive():
		// Nothing to run without a terminal: Add reports the unchanged error.
	default:
		state, err := s.LiveState()
		if err != nil {
			return err
		}
		// Live and unstored is the one state where running a login would be
		// asking someone to log in as an account they are logged in as.
		if !state.LoggedIn || state.Slot != "" {
			return a.loginViaTool(cmd.Context(), s, req.name)
		}
	}
	return a.captureInto(cmd.Context(), s, req.name)
}

// loginViaTool runs the provider's own login into a sandbox and stores what
// lands there.
func (a *App) loginViaTool(ctx context.Context, s *swap.Switcher, name string) error {
	spec := s.Spec()
	sandbox, err := s.BeginLogin()
	if err != nil {
		return err
	}
	argv := sandbox.Argv()

	a.printer.Blank()
	a.printer.Println(a.printer.Bold("Logging in with "+spec.DisplayName()+"."),
		a.printer.Dimmed(" Finish the login it opens with the account you want to add."))
	if _, live := s.LiveIdentity(); live {
		a.printer.Println(a.printer.Dimmed("  The account you are logged in as now is left as it is."))
	}
	a.printer.Blank()

	if err := a.runTool(ctx, argv, sandbox.Environment(os.Environ())); err != nil {
		sandbox.Discard()
		return fmt.Errorf("%w: `%s` did not complete: %w",
			apperr.ErrConfig, strings.Join(argv, " "), err)
	}

	outcome, err := s.FinishLogin(ctx, sandbox, swap.AddRequest{
		Name: name, AssumeYes: a.assumeYes, Confirm: a.confirm,
	})
	if err != nil {
		return err
	}
	if err := a.reportAdd(outcome); err != nil {
		return err
	}
	switch {
	case outcome.Cancelled:
	case outcome.Activated:
		a.printer.Println(a.printer.Dimmed("  Now the live login, since nothing was logged in."))
	case outcome.ActivationFailed != "":
		a.printer.Warning("stored, but could not make it the live login: " + outcome.ActivationFailed)
		a.printer.Println(a.printer.Dimmed("  Use it with:  "),
			a.printer.Accent("aaswap switch "+outcome.Name))
	default:
		a.printer.Println(a.printer.Dimmed("  Use it with:  "),
			a.printer.Accent("aaswap switch "+outcome.Name))
	}
	return nil
}

// runTool runs the provider's login command to completion, on the terminal.
func (a *App) runTool(ctx context.Context, argv, env []string) error {
	if a.RunTool != nil {
		return a.RunTool(ctx, argv, env)
	}
	binary, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("`%s` was not found on your PATH", argv[0])
	}
	tool := exec.CommandContext(ctx, binary, argv[1:]...)
	tool.Env = env
	tool.Stdin, tool.Stdout, tool.Stderr = a.In, a.Out, a.Err
	return tool.Run()
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
