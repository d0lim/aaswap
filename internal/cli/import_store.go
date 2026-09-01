package cli

import (
	"fmt"

	"github.com/d0lim/ccswap/internal/apperr"
	"github.com/spf13/cobra"
)

func (a *App) importStoreCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "import-store",
		Short: "Take over an account store left by the claude-swap project",
		Long: "ccswap keeps its own account store, separate from the claude-swap\n" +
			"project it was forked from. The two stamp the same schema versions into\n" +
			"the same file names, and neither can tell the other's version numbers\n" +
			"from its own — so sharing a store would let either one discard the\n" +
			"other's state the first time a version was bumped.\n\n" +
			"This moves a claude-swap store over, once, on request. The directory is\n" +
			"MOVED rather than copied: two stores holding the same refresh tokens\n" +
			"would fight, because refreshing rotates the token and whichever tool got\n" +
			"there first would leave the other reporting a live account as dead.\n\n" +
			"macOS Keychain items are copied, not moved, so putting the directory\n" +
			"back restores a working claude-swap install.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runImportStore()
		},
	}
}

func (a *App) runImportStore() error {
	// The resolver comes from the switcher rather than the environment: a
	// command that reads os.Environ itself reaches the developer's real store
	// from a test binary, which is what the paths guard exists to stop.
	s, err := a.switcher()
	if err != nil {
		return err
	}
	resolver := s.Paths
	source, found := resolver.FindClaudeSwapStore()
	if !found {
		return fmt.Errorf("%w: no claude-swap store found in %v",
			apperr.ErrConfig, resolver.ClaudeSwapRoots())
	}

	target := resolver.BackupRoot()
	if !a.confirm(fmt.Sprintf(
		"Move the claude-swap store at %s into %s? claude-swap will no longer see these accounts.",
		source, target)) {
		a.printer.Println("Nothing was moved.")
		return nil
	}

	if err := resolver.AdoptStore(source); err != nil {
		return err
	}
	a.printer.Printf("Moved %s to %s\n", source, target)

	// Only now is there a roster to read: it arrived with the directory.
	roster, err := s.RosterOrEmpty()
	if err != nil {
		return err
	}
	slots := make(map[string]string, len(roster.Accounts))
	for num, account := range roster.Accounts {
		slots[num] = account.Email
	}

	report, adoptErr := s.Creds.AdoptClaudeSwapKeychain(slots)
	if report.Copied > 0 {
		a.printer.Printf("Adopted %d Keychain backup(s)\n", report.Copied)
	}
	for _, failure := range report.Failed {
		a.errs.Warning("could not adopt account " + failure)
	}
	if adoptErr != nil {
		return adoptErr
	}

	a.printer.Printf("%d account(s) are now ccswap's. Run `ccswap list` to check them.\n", len(slots))
	a.printer.Println(a.printer.Dimmed(
		"The claude-swap Keychain items were left in place, so moving the directory " +
			"back restores claude-swap. Remove them once you are satisfied."))
	return nil
}
