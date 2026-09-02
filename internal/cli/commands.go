package cli

import (
	"fmt"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/buildinfo"
	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/swap"
	"github.com/spf13/cobra"
)

// rootCommand assembles the whole command surface.
func (a *App) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:   "aaswap",
		Short: "Switch between multiple agent CLI accounts",
		Long: "aaswap manages several agent CLI logins on one machine: it stores each\n" +
			"account's credential, swaps the live login between them, and reports how\n" +
			"much rate-limit headroom each one has left.\n\n" +
			"No tool is the default. Where only one has accounts stored, that is\n" +
			"the one addressed; otherwise you are asked, unless --provider says.\n" +
			"Run `aaswap doctor` to see what is supported for each.",
		Example: strings.Join([]string{
			"  aaswap login                     # store the account you are logged in as",
			"  aaswap list                      # show every account and its usage",
			"  aaswap switch work               # activate the account called work",
			"  aaswap --provider codex list     # the same, for Codex",
			"  aaswap doctor                    # what works for which provider",
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
	// The dimension, as a flag with a default — the shape gh uses for hosts.
	// A namespace would double the help tree and make every shared command
	// exist twice.
	// The list comes from the registry, not from here: a provider is added by
	// declaring it, and a hardcoded list would leave the new one working but
	// undiscoverable.
	root.PersistentFlags().StringVar(&a.provider, "provider", "",
		fmt.Sprintf("the auth domain to address: %s (asked when unclear)",
			strings.Join(provider.Names(), ", ")))

	// What stays at the top level is what gets typed daily. What moves into a
	// group is what gets typed once a month — and grouping is what keeps a
	// second provider from doubling this list.
	root.AddCommand(
		a.listCommand(),
		a.statusCommand(),
		a.switchCommand(),
		a.loginCommand(),
		a.runCommand(),
		a.tuiCommand(),
		a.configCommand(),
		a.doctorCommand(),
		a.upgradeCommand(),

		a.accountCommand(),
		a.dirCommand(),
	)
	return root
}

// accountCommand groups what is done TO an account rather than with it.
func (a *App) accountCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "account",
		Short:   "Manage the accounts themselves",
		Long:    "Renaming, removing, holding out of rotation, and moving accounts\nbetween machines.",
		Aliases: []string{"acct"},
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		a.renameCommand(),
		a.disableCommand(),
		a.enableCommand(),
		a.removeCommand(),
		a.exportCommand(),
		a.importCommand(),
		a.unclaimedCommand(),
		a.adoptCommand(),
	)
	silenceUsage(cmd)
	return cmd
}

// dirCommand groups the directory-to-account routing `aaswap run` reads.
func (a *App) dirCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dir",
		Short: "Route a directory to an account",
		Long: "`aaswap run` in a mapped directory launches that account. Subfolders\n" +
			"inherit the nearest mapped ancestor.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		a.mapCommand(),
		a.unmapCommand(),
		a.mappingsCommand(),
	)
	silenceUsage(cmd)
	return cmd
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
	var force bool
	cmd := &cobra.Command{
		Use:   "switch [ACCOUNT]",
		Short: "Activate another account",
		Long: "With no argument, rotates to the next account in sequence.\n" +
			"Name an account to activate that one.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			target := ""
			if len(args) == 1 {
				target = args[0]
			}
			return a.runSwitch(cmd, target, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"activate the stored credential without backing up the current login first")
	silenceUsage(cmd)
	return cmd
}

func (a *App) removeCommand() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:     "remove [ACCOUNT]",
		Aliases: []string{"rm"},
		Short:   "Forget a managed account",
		Long: "Removes aaswap's copy of the account: its stored credential, its stored\n" +
			"config, and its roster entry. It does NOT log you out — the live login\n" +
			"belongs to the provider's own tool, not to aaswap.\n\n" +
			"--all forgets every account for this provider.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Erasing the whole store is what `purge` used to be. It is the
			// same operation as removing one account, so it is the same verb
			// with a flag rather than a second top-level command whose name
			// does not say what it removes.
			if all {
				if len(args) > 0 {
					return fmt.Errorf("%w: --all removes every account, so it cannot "+
						"also be given one to remove", apperr.ErrValidation)
				}
				return a.runPurge(cmd)
			}
			if len(args) == 0 {
				return fmt.Errorf("%w: name an account to remove, or pass --all to "+
					"remove every one", apperr.ErrValidation)
			}
			return a.runRemove(cmd, args[0])
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "forget every account for this provider")
	silenceUsage(cmd)
	return cmd
}

func (a *App) disableCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "disable NUM|EMAIL|ALIAS",
		Short: "Hold an account out of rotation",
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
		Short: "Return an account to rotation",
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

func (a *App) unclaimedCommand() *cobra.Command {
	var purge string
	cmd := &cobra.Command{
		Use:   "unclaimed",
		Short: "Inspect credentials aaswap preserved but could not file",
		Long: "A switch that finds a live credential belonging to no managed account keeps\n" +
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
