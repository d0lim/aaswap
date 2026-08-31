package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/realiti4/claude-swap/internal/autoswitch"
	"github.com/spf13/cobra"
)

// autoCommand watches the active account and moves off it when it runs low.
func (a *App) autoCommand() *cobra.Command {
	var once, dryRun bool
	var threshold, interval, cooldown float64
	var strategy, model string
	var includeAPIKeys bool

	cmd := &cobra.Command{
		Use:   "auto",
		Short: "Watch the active account and switch off it before it runs out",
		Long: "Runs until interrupted, or once with --once for a cron job.\n\n" +
			"The margins that stop it from flapping — the hysteresis and the\n" +
			"unhealthy-tick count — live in settings.json rather than in flags:\n" +
			"they are policy, not something to vary per invocation.\n\n" +
			"A single tick's exit code says what it decided, so a wrapper can branch:\n" +
			"  0  switched\n" +
			"  1  the tick failed\n" +
			"  2  nothing needed doing\n" +
			"  3  it wanted to switch and had nowhere to go",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			flags := cmd.Flags()
			if flags.Changed("threshold") {
				a.overrides.Threshold = &threshold
			}
			if flags.Changed("interval") {
				a.overrides.IntervalSeconds = &interval
			}
			if flags.Changed("cooldown") {
				a.overrides.CooldownSeconds = &cooldown
			}
			if flags.Changed("strategy") {
				a.overrides.Strategy = &strategy
			}
			if flags.Changed("model") {
				a.overrides.Model = &model
			}
			if flags.Changed("include-api-key-accounts") {
				a.overrides.IncludeAPIKeyAccounts = &includeAPIKeys
			}
			return a.runAuto(cmd.Context(), once, dryRun)
		},
	}

	cmd.Flags().BoolVar(&once, "once", false, "evaluate a single tick and exit with its outcome")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "decide and report, but change nothing")
	cmd.Flags().Float64Var(&threshold, "threshold", 0,
		"switch when the binding window reaches this percentage")
	cmd.Flags().Float64Var(&interval, "interval", 0, "seconds between ticks")
	cmd.Flags().Float64Var(&cooldown, "cooldown", 0, "seconds to wait between proactive switches")
	cmd.Flags().StringVar(&strategy, "strategy", "",
		"'best' for the most headroom, or 'consume-first' to spend the most perishable quota")
	cmd.Flags().StringVar(&model, "model", "",
		"also count these models' per-model weekly limits (comma-separated, or 'all')")
	cmd.Flags().BoolVar(&includeAPIKeys, "include-api-key-accounts", false,
		"allow API-key accounts as switch targets")
	silenceUsage(cmd)
	return cmd
}

func (a *App) runAuto(ctx context.Context, once, dryRun bool) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}

	engine := &autoswitch.Engine{
		Switcher: s,
		State:    autoswitch.NewStore(s.BackupRoot()),
		Events:   a.eventEmitter(),
		Settings: s.Settings.AutoSwitch,
		DryRun:   dryRun,
		Now:      s.Now,
	}

	if once {
		outcome := engine.Tick(ctx)
		if outcome != autoswitch.Switched {
			// The outcome IS the exit code, so it travels as one rather than as
			// a message: a cron wrapper branches on it.
			return exitCode(int(outcome))
		}
		return nil
	}

	if !a.json {
		a.printer.Println(a.printer.Dimmed(fmt.Sprintf(
			"Watching the active account every %s. Press Ctrl-C to stop.",
			time.Duration(s.Settings.AutoSwitch.IntervalSeconds*float64(time.Second)).Round(time.Second))))
	}
	return engine.Run(ctx)
}

// eventEmitter renders the engine's events in whichever shape was asked for.
func (a *App) eventEmitter() autoswitch.Emitter {
	if a.json {
		// One JSON object per line, flushed as it happens: a consumer tailing
		// the stream sees each decision when it is made, not when the process
		// ends.
		return autoswitch.EmitterFunc(func(event autoswitch.Event) {
			line, err := event.JSONLine()
			if err != nil {
				a.errs.Error(fmt.Sprintf("could not encode an event: %v", err))
				return
			}
			_, _ = a.Out.Write(line)
		})
	}
	return autoswitch.EmitterFunc(func(event autoswitch.Event) {
		a.printer.Println(a.printer.Dimmed(event.Timestamp), "  ", a.eventStyle(event))
	})
}

// eventStyle colors an event line by what it means.
func (a *App) eventStyle(event autoswitch.Event) string {
	text := event.Human()
	switch event.Kind {
	case autoswitch.KindSwitch:
		return a.printer.Accent(text)
	case autoswitch.KindError:
		return a.printer.Red(text)
	case autoswitch.KindQuarantine, autoswitch.KindAllExhausted, autoswitch.KindConfigWarning:
		return a.printer.Yellow(text)
	case autoswitch.KindUnquarantine:
		return a.printer.Accent(text)
	}
	return a.printer.Muted(text)
}

// exitCode carries an outcome out of a command as an exit status.
//
// A type rather than an error message, because the number IS the interface for
// this command: a wrapper branches on it, and printing it as prose would be
// noise on a stream a script is reading.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
