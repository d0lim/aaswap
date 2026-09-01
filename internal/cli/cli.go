// Package cli is aaswap's command surface.
//
// It is a package rather than living in main so it can be tested: every command
// writes through an [App]'s streams and returns an error, and nothing calls
// os.Exit below the top.
//
// # Two spellings of one interface
//
// Commands are verbs — `aaswap list`, `aaswap switch 2` — and every one of them
// also answers to the flag spelling it replaced (`aaswap --list`, `aaswap
// --switch-to 2`). The flags are hidden from help so the verbs are the one
// documented interface, but they keep working: they are in people's shell
// history and their scripts.
package cli

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/jsonout"
	"github.com/d0lim/aaswap/internal/paths"
	"github.com/d0lim/aaswap/internal/render"
	"github.com/d0lim/aaswap/internal/session"
	"github.com/d0lim/aaswap/internal/settings"
	"github.com/d0lim/aaswap/internal/swap"
	"github.com/spf13/cobra"
)

// Exit codes. Zero is success; everything else is documented because scripts
// branch on them.
const (
	// ExitOK is success.
	ExitOK = 0
	// ExitError is a handled failure. The message is on stderr, or in the JSON
	// envelope when --json was asked for.
	ExitError = 1
	// ExitInterrupted is a user interrupt.
	ExitInterrupted = 130
)

// App holds one invocation's environment.
//
// Streams and the switcher factory are fields so a test drives the whole
// surface without touching the developer's real store — the same discipline the
// packages below use, carried up to the command layer.
type App struct {
	Out io.Writer
	Err io.Writer
	In  io.Reader

	// NewSwitcher builds the switcher a command operates on. Injected so tests
	// substitute a fixture.
	NewSwitcher func() (*swap.Switcher, error)

	// Confirm asks a yes-or-no question. Nil falls back to reading In.
	Confirm func(prompt string) bool

	// Choose asks a multiple-choice question, returning the key chosen. Nil
	// falls back to reading In — and, because a non-nil Choose is by
	// definition someone to ask, setting it makes the App interactive.
	Choose func(prompt string, options []Choice) string

	printer *render.Printer
	errs    *render.Printer

	// json makes every command emit a machine-readable object instead of prose.
	json bool
	// assumeYes answers every confirmation with yes.
	assumeYes bool
	// overrides are the policy knobs a flag may override for this run.
	overrides settings.Overrides
	// awaitTuning collapses the login wait's polling cadence. Zero uses the
	// production one; a test sets it so the wait is not measured in seconds.
	// OnWaiting is always supplied by awaitLogin and never read from here.
	awaitTuning swap.AwaitOptions
}

// New returns an App wired to the real environment.
func New() *App {
	app := &App{
		Out:         os.Stdout,
		Err:         os.Stderr,
		In:          os.Stdin,
		NewSwitcher: defaultSwitcher,
	}
	app.printer = render.New(app.Out)
	app.errs = render.New(app.Err)
	return app
}

func defaultSwitcher() (*swap.Switcher, error) {
	resolver, err := paths.FromEnv()
	if err != nil {
		return nil, err
	}
	s := swap.New(resolver)
	s.Settings = settings.Load(resolver.BackupRoot())
	return s, nil
}

// Execute runs the command line and returns the process exit code.
//
// Errors do not escape: a handled failure is reported in whichever shape the
// caller asked for and becomes an exit code, because a traceback is not a user
// interface.
func (a *App) Execute(ctx context.Context, args []string) int {
	if a.printer == nil {
		a.printer = render.New(a.Out)
	}
	if a.errs == nil {
		a.errs = render.New(a.Err)
	}

	root := a.rootCommand()
	a.configureLogging(hasFlag(args, "--debug"), hasFlag(args, "--json"))
	root.SetArgs(args)
	root.SetOut(a.Out)
	root.SetErr(a.Err)

	if err := root.ExecuteContext(ctx); err != nil {
		// An outcome carried as an exit code is not a failure to report: the
		// command already said everything it had to say, in the shape the
		// caller asked for.
		if code, ok := errors.AsType[exitCode](err); ok {
			return int(code)
		}
		return a.reportError(err)
	}
	return ExitOK
}

// reportError renders a failure and returns its exit code.
func (a *App) reportError(err error) int {
	if errors.Is(err, context.Canceled) {
		return ExitInterrupted
	}
	if a.json {
		// One machine-readable object on stdout either way, so a consumer
		// parses one shape and branches on the presence of `error` rather than
		// on the exit code alone.
		a.emitJSON(jsonout.ErrorEnvelope{
			SchemaVersion: jsonout.SchemaVersion,
			Error:         jsonout.Error{Type: errorKind(err), Message: err.Error()},
		})
		return ExitError
	}
	a.errs.Error(err.Error())
	return ExitError
}

// errorKind names a failure in the taxonomy's terms rather than by Go type.
//
// The spelling is the contract: renaming an internal type must not break a
// consumer's branch.
func errorKind(err error) string {
	for _, candidate := range []struct {
		sentinel error
		name     string
	}{
		{apperr.ErrCredentialRead, "CredentialReadError"},
		{apperr.ErrCredentialWrite, "CredentialWriteError"},
		{apperr.ErrCredential, "CredentialError"},
		{apperr.ErrClaudeCodeLockTimeout, "ClaudeCodeLockTimeout"},
		{apperr.ErrLock, "LockError"},
		{apperr.ErrAccountNotFound, "AccountNotFoundError"},
		{apperr.ErrValidation, "ValidationError"},
		{apperr.ErrSwitch, "SwitchError"},
		{apperr.ErrSession, "SessionError"},
		{apperr.ErrTransfer, "TransferError"},
		{apperr.ErrMigrationIncomplete, "MigrationIncomplete"},
		{apperr.ErrMigration, "MigrationError"},
		{apperr.ErrConfig, "ConfigError"},
		{apperr.Err, "ClaudeSwitchError"},
	} {
		if errors.Is(err, candidate.sentinel) {
			return candidate.name
		}
	}
	return "Error"
}

// emitJSON writes one payload as the whole of stdout.
func (a *App) emitJSON(payload any) {
	data, err := json.Marshal(payload, jsontext.WithIndent("  "))
	if err != nil {
		a.errs.Error(fmt.Sprintf("could not encode the JSON payload: %v", err))
		return
	}
	_, _ = a.Out.Write(append(data, '\n'))
}

// switcher builds the switcher for a command, applying this run's overrides.
func (a *App) switcher() (*swap.Switcher, error) {
	s, err := a.NewSwitcher()
	if err != nil {
		return nil, err
	}
	s.Settings.AutoSwitch = settings.Clamp(settings.MergeCLI(s.Settings.AutoSwitch, a.overrides))
	// A replaced backup credential leaves that slot's session profile holding
	// the previous generation, which still passes the local reuse check. The
	// switcher does not know about profiles, so the command layer — which does
	// — supplies the invalidation.
	s.OnBackupWritten = func(accountNum, email string) {
		a.invalidateSessionProfile(s, accountNum, email)
	}

	// Before any command touches the store. A table written by an older release
	// addresses accounts by slot number and files their credentials the same
	// way; every command below assumes names. Doing it here rather than inside
	// each command means there is exactly one place where the two shapes meet.
	moved, err := s.EnsureUpgraded()
	if err != nil {
		return nil, err
	}
	if moved > 0 && !a.json {
		a.printer.Println(a.printer.Dimmed(fmt.Sprintf(
			"Upgraded %d account(s) to the current store format. They are addressed "+
				"by name now — run `aaswap list` to see them.", moved)))
	}
	return s, nil
}

// invalidateSessionProfile re-points a slot's session profile after its stored
// credential changed.
//
// Reports rather than returns: this runs after a credential write that already
// succeeded, and the write is what the user asked for. Failing the command here
// would say the thing that happened did not.
func (a *App) invalidateSessionProfile(s *swap.Switcher, accountNum, email string) {
	outcome, err := a.sessionManager(s).InvalidateForSlot(accountNum, email)
	switch outcome {
	case session.MarkFailed:
		a.errs.Warning(fmt.Sprintf(
			"%s's credential changed but its session profile could not be "+
				"invalidated; `aaswap run %s` may keep using the superseded one until "+
				"you remove the profile: %v", accountNum, accountNum, err))
	case session.Marked:
		a.printer.Println(a.printer.Dimmed(
			"  a live session profile for this account will re-bootstrap when it next exits"))
	case session.Cleared:
		a.printer.Println(a.printer.Dimmed(
			"  its session profile will re-bootstrap from the new credential"))
	}
}

// confirm asks a yes-or-no question, defaulting to no.
//
// No is the default everywhere because every question this asks precedes
// something irreversible, and a stray newline must not be an answer.
func (a *App) confirm(prompt string) bool {
	if a.assumeYes {
		return true
	}
	if a.Confirm != nil {
		return a.Confirm(prompt)
	}
	if a.In == nil {
		// Nothing to ask with. Refusing is the safe answer.
		return false
	}
	a.printer.Printf("%s [y/N] ", prompt)
	var answer string
	if _, err := fmt.Fscanln(a.In, &answer); err != nil {
		a.printer.Blank()
		return false
	}
	return answer == "y" || answer == "Y" || answer == "yes"
}

// hasFlag reports whether a flag appears in the argument list.
//
// Read before cobra parses, because logging has to be configured before the
// first command runs — and a command that logs during setup would otherwise
// write through whatever the previous invocation left behind.
func hasFlag(args []string, name string) bool {
	for _, arg := range args {
		if arg == name || strings.HasPrefix(arg, name+"=") {
			return true
		}
		if arg == "--" {
			return false
		}
	}
	return false
}

// silenceUsage stops cobra printing its usage block for a runtime failure. A
// wrong flag deserves usage; a failed switch does not.
func silenceUsage(cmd *cobra.Command) {
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
}
