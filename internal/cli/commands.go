package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/realiti4/claude-swap/internal/apperr"
	"github.com/realiti4/claude-swap/internal/buildinfo"
	"github.com/realiti4/claude-swap/internal/swap"
	"github.com/spf13/cobra"
)

// rootCommand assembles the whole command surface.
func (a *App) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "cswap",
		Short: "Switch between multiple Claude Code accounts",
		Long: "cswap manages several Claude Code logins on one machine: it stores each\n" +
			"account's credential, swaps the live login between them, and reports how\n" +
			"much rate-limit headroom each one has left.",
		Example: strings.Join([]string{
			"  cswap add                       # capture the account you are logged in as",
			"  cswap list                      # show every account and its usage",
			"  cswap switch 2                  # activate account 2",
			"  cswap switch --strategy best    # activate whichever has the most headroom",
			"  cswap list --json               # the same listing, for a script",
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
		a.aliasCommand(),
		a.swapCommand(),
		a.moveCommand(),
		a.purgeCommand(),
		a.configCommand(),
		a.unclaimedCommand(),
		a.runCommand(),
		a.mapCommand(),
		a.unmapCommand(),
		a.mappingsCommand(),
		a.exportCommand(),
		a.importCommand(),
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
	var slot int
	var alias string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Capture the account you are currently logged in as",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runAdd(cmd, slot, alias)
		},
	}
	cmd.Flags().IntVar(&slot, "slot", 0, "store the account in a specific slot")
	cmd.Flags().StringVar(&alias, "alias", "", "give the account a short name")
	silenceUsage(cmd)
	return cmd
}

func (a *App) removeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "remove NUM|EMAIL|ALIAS",
		Aliases: []string{"rm"},
		Short:   "Forget a managed account",
		Long: "Removes cswap's copy of the account: its stored credential, its stored\n" +
			"config, and its roster entry. It does NOT log you out — the live login is\n" +
			"Claude Code's, not cswap's.",
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

func (a *App) aliasCommand() *cobra.Command {
	var unset bool
	cmd := &cobra.Command{
		Use:   "alias [NUM|EMAIL] [NAME]",
		Short: "Name an account, or list the names",
		Args:  cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runAlias(cmd, args, unset)
		},
	}
	cmd.Flags().BoolVar(&unset, "unset", false, "remove the account's alias")
	silenceUsage(cmd)
	return cmd
}

func (a *App) swapCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "swap NUM|EMAIL|ALIAS NUM|EMAIL|ALIAS",
		Short: "Exchange two accounts' slot numbers",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runSwapSlots(cmd, args[0], args[1])
		},
	}
	silenceUsage(cmd)
	return cmd
}

func (a *App) moveCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "move NUM|EMAIL|ALIAS SLOT",
		Short: "Move an account to another slot number",
		Long:  "When the destination is occupied, the two accounts exchange places.",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runMove(cmd, args[0], args[1])
		},
	}
	silenceUsage(cmd)
	return cmd
}

func (a *App) purgeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "purge",
		Short: "Forget every managed account",
		Long:  "Removes cswap's whole store. Your live login survives.",
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
		Short: "Inspect credentials cswap preserved but could not file",
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
		return "", fmt.Errorf("%w: account %s does not exist", apperr.ErrAccountNotFound, num)
	}
	return num, nil
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
