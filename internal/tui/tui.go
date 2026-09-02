// Package tui is aaswap's interactive dashboard.
//
// # Why a redesign rather than a port
//
// The Python implementation was Textual: screens, reactive attributes and
// workers. Bubble Tea has none of those — it has one Model, one Update and one
// View — so the structure here is its own rather than a translation. What
// carried over is the rule that made the original safe: the UI loop never
// performs blocking work. Every lock, Keychain call and network fetch is a
// [tea.Cmd], and its result arrives as a message.
//
// That rule earns its keep in this program specifically. A collect pass can
// wait seconds on the store lock while another aaswap or a live Claude Code
// holds it. A dashboard that froze there would look crashed at precisely the
// moment it is reporting on someone's credentials.
package tui

import (
	"context"
	"errors"
	"fmt"
	"io"

	tea "charm.land/bubbletea/v2"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/render"
	"github.com/d0lim/aaswap/internal/swap"
)

// Options configures one dashboard run.
type Options struct {
	// Switcher is the store the dashboard operates on. Required.
	Switcher *swap.Switcher
	// Theme selects the palette. Defaults to dark.
	Theme render.Theme
	// In and Out are the terminal. Both nil uses the process's own, which is
	// what a real run wants; a test supplies its own pair.
	In  io.Reader
	Out io.Writer
}

// Run drives the dashboard until the user quits.
//
// The context cancels the program, so an interrupt unwinds through Bubble Tea's
// own shutdown — restoring the terminal — instead of leaving a raw-mode
// terminal behind for the shell.
func Run(ctx context.Context, opts Options) error {
	if opts.Switcher == nil {
		return fmt.Errorf("%w: the dashboard needs a switcher", apperr.ErrConfig)
	}

	programOpts := []tea.ProgramOption{tea.WithContext(ctx)}
	if opts.In != nil {
		programOpts = append(programOpts, tea.WithInput(opts.In))
	}
	if opts.Out != nil {
		programOpts = append(programOpts, tea.WithOutput(opts.Out))
	}

	program := tea.NewProgram(NewModel(opts.Switcher, opts.Theme), programOpts...)
	_, err := program.Run()
	// Leaving is not failing. Bubble Tea reports both ways out as errors —
	// ErrInterrupted for its own Ctrl-C handler, ErrProgramKilled for the
	// cancelled context aaswap's signal handler produces — and passing either
	// upward would hand the shell a non-zero status for a normal quit.
	//
	// Matched on the sentinels rather than inferred from ctx.Err(), so the two
	// outcomes that mean "the user left" are named rather than guessed at.
	if errors.Is(err, tea.ErrInterrupted) || errors.Is(err, tea.ErrProgramKilled) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: running the dashboard: %w", apperr.ErrConfig, err)
	}
	return nil
}
