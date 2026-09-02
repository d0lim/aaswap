package cli

import (
	"fmt"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/swap"
	"github.com/spf13/cobra"
)

func (a *App) adoptCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "adopt",
		Short: "Take over an account store left by an earlier version of this tool",
		Long: "aaswap keeps its own account store, separate from the projects it\n" +
			"succeeded — ccswap, which it was renamed from, and claude-swap, which it\n" +
			"was ported from. Each stamps its own schema versions into the same file\n" +
			"names, and none can tell another's version numbers from its own, so\n" +
			"sharing a store would let any of them discard the others' state the\n" +
			"first time a version was bumped.\n\n" +
			"This moves a predecessor's store over, once, on request. The directory\n" +
			"is MOVED rather than copied: two stores holding the same refresh tokens\n" +
			"would fight, because refreshing rotates the token and whichever tool got\n" +
			"there first would leave the other reporting a live account as dead.\n\n" +
			"macOS Keychain items are copied, not moved, so putting the directory\n" +
			"back restores a working predecessor install.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runAdopt()
		},
	}
}

func (a *App) runAdopt() error {
	// Always Claude's, whatever --provider says. A predecessor store predates
	// providers, so every account it holds is a Claude Code login — and reading
	// the roster through another provider's section found none of them, so the
	// Keychain adoption below copied nothing and was never retried, stranding
	// every credential the predecessor kept there.
	//
	// The resolver comes from the switcher rather than the environment: a
	// command that reads os.Environ itself reaches the developer's real store
	// from a test binary, which is what the paths guard exists to stop.
	s, err := a.switcherFor(swap.ProviderClaude)
	if err != nil {
		return err
	}
	resolver := s.Paths
	found, ok := resolver.FindPredecessor()
	if !ok {
		var looked []string
		for _, candidate := range resolver.Predecessors() {
			looked = append(looked, strings.Join(candidate.Roots, ", "))
		}
		return fmt.Errorf("%w: no predecessor store found. Looked in: %s",
			apperr.ErrConfig, strings.Join(looked, "; "))
	}
	source := found.Root

	target := resolver.BackupRoot()
	if !a.confirm(fmt.Sprintf(
		"Move the %s store at %s into %s? %s will no longer see these accounts.",
		found.Name, source, target, found.Name)) {
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

	report, adoptErr := s.Creds.AdoptKeychain(found.KeychainService, slots)
	if report.Copied > 0 {
		a.printer.Printf("Adopted %d Keychain backup(s)\n", report.Copied)
	}
	for _, failure := range report.Failed {
		a.errs.Warning("could not adopt account " + failure)
	}
	if adoptErr != nil {
		return adoptErr
	}

	a.printer.Printf("%d account(s) are now aaswap's. Run `aaswap list` to check them.\n", len(slots))
	a.printer.Println(a.printer.Dimmed(fmt.Sprintf(
		"The %s Keychain items were left in place, so moving the directory back "+
			"restores %s. Remove them once you are satisfied.", found.Name, found.Name)))
	return nil
}
