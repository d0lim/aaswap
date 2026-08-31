package cli

import (
	"fmt"

	"github.com/realiti4/claude-swap/internal/buildinfo"
	"github.com/realiti4/claude-swap/internal/updatecheck"
	"github.com/spf13/cobra"
)

// upgradeCommand reports whether a newer release exists and how to get it.
//
// It does not upgrade in place. A binary installed by a package manager belongs
// to that package manager, and replacing it from underneath would leave its
// records wrong and its own next upgrade confused.
func (a *App) upgradeCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "upgrade",
		Aliases: []string{"update"},
		Short:   "Check for a newer release and say how to install it",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runUpgrade(cmd)
		},
	}
	silenceUsage(cmd)
	return cmd
}

func (a *App) runUpgrade(cmd *cobra.Command) error {
	current := buildinfo.Version()
	method := updatecheck.DetectMethod()

	s, err := a.switcher()
	if err != nil {
		return err
	}
	checker := updatecheck.New(s.Paths.CacheDir())
	checker.Now = s.Now

	latest, url, ok := checker.Latest(cmd.Context())
	if !ok {
		a.printer.Println(a.printer.Dimmed(
			"Could not reach the releases page. You are on " + current + "."))
		return nil
	}

	if !updatecheck.Newer(latest, current) {
		a.printer.Println(a.printer.Accent("Up to date"), " ", current)
		return nil
	}

	a.printer.Println(a.printer.Accent("Update available"), " ",
		fmt.Sprintf("%s → %s", current, latest))
	if command := method.UpgradeCommand(); command != "" {
		a.printer.Println("  ", a.printer.Bold(command))
	} else {
		a.printer.Println(a.printer.Dimmed(
			"  cswap could not tell how it was installed. Install the build for your " +
				"platform from:"))
		a.printer.Println("  ", url)
	}
	return nil
}
