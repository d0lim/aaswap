package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/d0lim/ccswap/internal/apperr"
	"github.com/d0lim/ccswap/internal/swap"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// addTokenCommand registers a raw token as an account.
func (a *App) addTokenCommand() *cobra.Command {
	var slot int
	var email string
	cmd := &cobra.Command{
		Use:   "add-token [TOKEN]",
		Short: "Register a setup token or API key without logging in first",
		Long: "For a headless machine, or a token handed over from somewhere else:\n" +
			"there is no prior Claude Code login here to capture. The kind is detected\n" +
			"from the value, and nothing is sent anywhere to find out.\n\n" +
			"Give \"-\" to read the token from stdin, or nothing to be prompted without\n" +
			"it appearing in your shell history.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			token := ""
			if len(args) == 1 {
				token = args[0]
			}
			return a.runAddToken(token, email, slot)
		},
	}
	cmd.Flags().IntVar(&slot, "slot", 0, "store the account in a specific slot")
	cmd.Flags().StringVar(&email, "email", "",
		"label the account with an address (a placeholder is synthesized otherwise)")
	silenceUsage(cmd)
	return cmd
}

func (a *App) runAddToken(token, email string, slot int) error {
	token, err := a.readToken(token)
	if err != nil {
		return err
	}

	s, err := a.switcher()
	if err != nil {
		return err
	}
	outcome, err := s.AddToken(swap.AddTokenRequest{
		Token: token, Email: email, Slot: slot,
		AssumeYes: a.assumeYes, Confirm: a.confirm,
	})
	if err != nil {
		return err
	}
	if outcome.Cancelled {
		a.printer.Println(a.printer.Dimmed("Cancelled"))
		return nil
	}

	if outcome.MovedFrom != "" {
		a.printer.Println(a.printer.Dimmed(
			fmt.Sprintf("Moved from slot %s → %s", outcome.MovedFrom, outcome.Number)))
	}
	verb := "Added"
	if outcome.Refreshed {
		verb = "Updated the token for"
	}
	a.printer.Println(a.printer.Accent(verb), " ",
		fmt.Sprintf("Account %s: %s", outcome.Number, outcome.Email),
		a.printer.Muted(" ["+outcome.Tag+"]"))
	return nil
}

// readToken resolves where the token comes from.
//
// A token given on the command line lands in the shell history, so the two
// other forms exist and are worth reaching for: "-" reads a piped one, and an
// absent argument prompts without echoing.
func (a *App) readToken(token string) (string, error) {
	switch {
	case token == "-":
		reader := bufio.NewReader(a.In)
		line, err := reader.ReadString('\n')
		if err != nil && line == "" {
			return "", fmt.Errorf("%w: no token was read from stdin", apperr.ErrValidation)
		}
		return strings.TrimSpace(line), nil

	case token != "":
		return strings.TrimSpace(token), nil
	}

	// Prompted. Echo is suppressed when this is a real terminal; when it is
	// not, there is nothing to suppress and a piped value reads normally.
	if file, ok := a.In.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		a.printer.Printf("Token: ")
		secret, err := term.ReadPassword(int(file.Fd()))
		a.printer.Blank()
		if err != nil {
			return "", fmt.Errorf("%w: reading the token: %w", apperr.ErrValidation, err)
		}
		return strings.TrimSpace(string(secret)), nil
	}

	reader := bufio.NewReader(a.In)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("%w: no token was given. Pass one as an argument, pipe it "+
			"with `-`, or run this from a terminal to be prompted", apperr.ErrValidation)
	}
	return strings.TrimSpace(line), nil
}
