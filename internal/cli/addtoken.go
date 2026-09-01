package cli

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/swap"
	"golang.org/x/term"
)

func (a *App) runAddToken(token, email, name string) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	// The refusal comes BEFORE the read. `--token -` takes a secret off stdin,
	// and a provider that cannot store one has no reason to be handed it.
	if err := a.requireCapability(s, provider.CapToken); err != nil {
		return err
	}

	token, err = a.readToken(token)
	if err != nil {
		return err
	}
	outcome, err := s.AddToken(swap.AddTokenRequest{
		Token: token, Email: email, Name: name,
		AssumeYes: a.assumeYes, Confirm: a.confirm,
	})
	if err != nil {
		return err
	}
	if outcome.Cancelled {
		a.printer.Println(a.printer.Dimmed("Cancelled"))
		return nil
	}

	if outcome.RenamedFrom != "" {
		a.printer.Println(a.printer.Dimmed(
			fmt.Sprintf("Renamed %s → %s", outcome.RenamedFrom, outcome.Name)))
	}
	verb := "Added"
	if outcome.Refreshed {
		verb = "Updated the token for"
	}
	a.printer.Println(a.printer.Accent(verb), " ",
		fmt.Sprintf("%s: %s", outcome.Name, outcome.Email),
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
