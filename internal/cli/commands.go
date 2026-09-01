package cli

import (
	"fmt"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/buildinfo"
	"github.com/d0lim/aaswap/internal/swap"
	"github.com/spf13/cobra"
)

// rootCommand assembles the whole command surface.
func (a *App) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "aaswap",
		Short: "Switch between multiple Claude Code accounts",
		Long: "aaswap manages several Claude Code logins on one machine: it stores each\n" +
			"account's credential, swaps the live login between them, and reports how\n" +
			"much rate-limit headroom each one has left.",
		Example: strings.Join([]string{
			"  aaswap add                       # capture the account you are logged in as",
			"  aaswap list                      # show every account and its usage",
			"  aaswap switch 2                  # activate account 2",
			"  aaswap switch --strategy best    # activate whichever has the most headroom",
			"  aaswap list --json               # the same listing, for a script",
		}, "\n"),
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       buildinfo.Version(),
		// No arguments and no subcommand is a request for help, not an error.
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}

	root.PersistentFlags().BoolVar(&a.json, "json", false,
		"emit machine-readable JSON on stdout instead of prose")
	root.PersistentFlags().BoolVarP(&a.assumeYes, "yes", "y", false,
		"answer every confirmation with yes")
	root.PersistentFlags().Bool("debug", false, "enable debug logging")

	root.AddCommand(
		a.listCommand(),
		a.statusCommand(),
		a.switchCommand(),
		a.addCommand(),
		a.removeCommand(),
		a.disableCommand(),
		a.enableCommand(),
		a.renameCommand(),
		a.purgeCommand(),
		a.configCommand(),
		a.unclaimedCommand(),
		a.runCommand(),
		a.mapCommand(),
		a.unmapCommand(),
		a.mappingsCommand(),
		a.exportCommand(),
		a.importCommand(),
		a.autoCommand(),
		a.addTokenCommand(),
		a.tuiCommand(),
		a.adoptCommand(),
		a.upgradeCommand(),
	)
	return root
}

func (a *App) listCommand() *cobra.Command {
	var tokenStatus bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "Show every managed account and its usage",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runList(cmd, tokenStatus)
		},
	}
	cmd.Flags().BoolVar(&tokenStatus, "token-status", false,
		"show source-labelled OAuth token diagnostics")
	silenceUsage(cmd)
	return cmd
}

func (a *App) statusCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show which account is currently logged in",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runStatus(cmd)
		},
	}
	silenceUsage(cmd)
	return cmd
}

func (a *App) switchCommand() *cobra.Command {
	var strategy, model string
	var force bool
	cmd := &cobra.Command{
		Use:   "switch [NUM|EMAIL|ALIAS]",
		Short: "Activate another account",
		Long: "With no argument, rotates to the next account in sequence.\n" +
			"With --strategy, picks the target by remaining rate-limit headroom.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			if model != "" {
				a.overrides.Model = &model
			}
			return a.runSwitch(cmd, target, strategy, force)
		},
	}
	cmd.Flags().StringVar(&strategy, "strategy", "",
		"pick the target by headroom: 'best' or 'next-available'")
	cmd.Flags().StringVar(&model, "model", "",
		"also count these models' per-model weekly limits (comma-separated, or 'all')")
	cmd.Flags().BoolVar(&force, "force", false,
		"activate the stored credential without backing up the current login first")
	silenceUsage(cmd)
	return cmd
}

func (a *App) addCommand() *cobra.Command {
	var name string
	var wait bool
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Capture the account you are currently logged in as",
		Long: "Stores the live login's credential and config against a slot, so aaswap\n" +
			"can switch back to it later.\n\n" +
			"aaswap cannot log you in — Claude Code owns that flow — so adding another\n" +
			"account means logging in with it first. With --wait, aaswap prints how to\n" +
			"do that and captures the account the moment the login finishes, instead of\n" +
			"making you come back and re-run this. In a terminal with no account logged\n" +
			"in at all, it waits without being asked: the alternative is an error whose\n" +
			"only advice is to go and log in.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runAdd(cmd, name, wait)
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "give the account a name")
	cmd.Flags().BoolVar(&wait, "wait", false,
		"wait for a /login in Claude Code, then capture that account")
	silenceUsage(cmd)
	return cmd
}

func (a *App) removeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "remove NUM|EMAIL|ALIAS",
		Aliases: []string{"rm"},
		Short:   "Forget a managed account",
		Long: "Removes aaswap's copy of the account: its stored credential, its stored\n" +
			"config, and its roster entry. It does NOT log you out — the live login is\n" +
			"Claude Code's, not aaswap's.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runRemove(cmd, args[0])
		},
	}
	silenceUsage(cmd)
	return cmd
}

func (a *App) disableCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "disable NUM|EMAIL|ALIAS",
		Short: "Hold an account out of automatic selection",
		Long: "The account stays managed and remains a valid explicit switch target; it\n" +
			"is only skipped by rotation, the headroom strategies, and auto-switch.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSetDisabled(cmd, args[0], true)
		},
	}
	silenceUsage(cmd)
	return cmd
}

func (a *App) enableCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "enable NUM|EMAIL|ALIAS",
		Short: "Return an account to automatic selection",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSetDisabled(cmd, args[0], false)
		},
	}
	silenceUsage(cmd)
	return cmd
}

func (a *App) renameCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rename ACCOUNT NAME",
		Short: "Give an account a different name",
		Long: "The name is where the account's credential and config are filed, not a\n" +
			"label on top of them, so this moves stored material. Nothing else changes.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runRename(cmd, args[0], args[1])
		},
	}
	silenceUsage(cmd)
	return cmd
}

func (a *App) purgeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Forget every managed account",
		Long:  "Removes aaswap's whole store. Your live login survives.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runPurge(cmd)
		},
	}
	silenceUsage(cmd)
	return cmd
}

func (a *App) unclaimedCommand() *cobra.Command {
	var purge string
	cmd := &cobra.Command{
		Use:   "unclaimed",
		Short: "Inspect credentials aaswap preserved but could not file",
		Long: "A switch that finds a live credential belonging to no managed slot keeps\n" +
			"it here rather than destroying it. These are the copies it kept.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runUnclaimed(cmd, purge)
		},
	}
	cmd.Flags().StringVar(&purge, "purge", "",
		"drop one preserved credential by id, or 'all' for every one")
	silenceUsage(cmd)
	return cmd
}

// resolveTarget maps what the user typed to a slot, with the error a person can
// act on.
func resolveTarget(s *swap.Switcher, roster *swap.Roster, identifier string) (string, error) {
	num, ok, err := s.ResolveIdentifier(roster, identifier)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: no account found with identifier %q",
			apperr.ErrAccountNotFound, identifier)
	}
	if _, exists := roster.Accounts[num]; !exists {
		return "", fmt.Errorf("%w: %s does not exist", apperr.ErrAccountNotFound, num)
	}
	return num, nil
}
