package cli

import (
	"fmt"
	"os"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/tui"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// errJSONUnsupported is what --json gets from a command that has no
// machine-readable form. Refusing beats emitting an empty envelope a script
// would then branch on as if it meant something.
var errJSONUnsupported = fmt.Errorf(
	"%w: the dashboard is interactive and has no --json form; use `aaswap list --json`",
	apperr.ErrConfig)

// errNotATerminal is what a redirected or piped invocation gets.
//
// Checked here rather than left to Bubble Tea, which fails on the /dev/tty
// open with a message about a device — accurate, and no help at all to someone
// who piped the command in a script and needs to be told which command to use
// instead.
var errNotATerminal = fmt.Errorf(
	"%w: the dashboard needs a terminal; use `aaswap list`, or `aaswap list --json` from a script",
	apperr.ErrConfig)

func (a *App) tuiCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "tui",
		Aliases: []string{"dashboard", "ui"},
		Short:   "Open the interactive dashboard",
		Long: "An interactive account list: usage bars, reset times, and switching\n" +
			"without retyping a slot number.\n\n" +
			"Needs a terminal. With output redirected, use `aaswap list` instead —\n" +
			"and `aaswap list --json` when a program is reading it.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runTUI(cmd)
		},
	}
}

func (a *App) runTUI(cmd *cobra.Command) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	// The dashboard owns the terminal, so --json has nothing to write to and
	// would be silently ignored. Saying so beats printing an empty object.
	if a.json {
		return errJSONUnsupported
	}
	if !isTerminal(a.Out) || !isTerminal(a.In) {
		return errNotATerminal
	}
	return tui.Run(cmd.Context(), tui.Options{
		Switcher: s,
		Theme:    a.printer.Theme,
	})
}

// isTerminal reports whether a stream is the process's own terminal.
//
// Only os.File can be one; a test's buffer or a pipe never is, which is the
// answer this wants in both cases.
func isTerminal(stream any) bool {
	file, ok := stream.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}
