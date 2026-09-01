package cli

import (
	"fmt"

	"github.com/d0lim/aaswap/internal/provider"
	"github.com/spf13/cobra"
)

// allCapabilities is the report's column order.
//
// Fixed and complete: every provider reports every capability, supported or
// not. A missing entry and an unsupported one are different facts, and a reader
// that cannot tell them apart guesses — which is the failure this whole command
// exists to end.
var allCapabilities = []provider.Capability{
	provider.CapAccounts,
	provider.CapSwitch,
	provider.CapLogin,
	provider.CapTransfer,
	provider.CapSession,
	provider.CapUsage,
	provider.CapRefresh,
}

// doctorCommand reports what aaswap can do for each provider on this machine.
func (a *App) doctorCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Report what aaswap can do for each agent CLI",
		Long: "Every command either works for a provider or says why it cannot.\n" +
			"This is that table, read from the same declarations the commands\n" +
			"themselves consult — so it cannot drift from what actually happens.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runDoctor()
		},
	}
	silenceUsage(cmd)
	return cmd
}

// capabilityReport is one capability's answer for one provider.
type capabilityReport struct {
	Supported bool   `json:"supported"`
	Reason    string `json:"reason,omitzero"`
}

// providerReport is everything the doctor knows about one provider.
type providerReport struct {
	Name string `json:"name"`
	// Home is where the tool keeps its files on this machine.
	Home string `json:"home"`
	// HomeEnv relocates it, empty when the tool has no such variable.
	HomeEnv string `json:"homeEnv,omitzero"`
	// IdentityTier is how accounts get their names: parse, probe, or hash.
	IdentityTier string `json:"identityTier"`
	// DetectsRunningSessions reports whether aaswap can tell if something is
	// running against a profile. False is why a profile is never refreshed
	// automatically — see Spec.Session.
	DetectsRunningSessions bool `json:"detectsRunningSessions"`
	// UsageScope is whose rate limits can be measured: "per-account",
	// "live-account-only", or "none".
	UsageScope string `json:"usageScope"`
	// Accounts is how many are stored, and LoggedIn whether one is live.
	Accounts int  `json:"accounts"`
	LoggedIn bool `json:"loggedIn"`

	Capabilities map[string]capabilityReport `json:"capabilities"`
}

type doctorPayload struct {
	SchemaVersion int              `json:"schemaVersion"`
	Providers     []providerReport `json:"providers"`
}

func (a *App) runDoctor() error {
	reports := make([]providerReport, 0, len(provider.Names()))
	for _, name := range provider.Names() {
		reports = append(reports, a.reportProvider(name))
	}

	if a.json {
		a.emitJSON(doctorPayload{SchemaVersion: 1, Providers: reports})
		return nil
	}
	a.printDoctor(reports)
	return nil
}

// reportProvider builds one provider's row.
//
// A provider whose store cannot be opened still gets a row: its capabilities
// come from the declaration and are true regardless, and omitting it would hide
// exactly the provider a person is trying to diagnose.
func (a *App) reportProvider(name string) providerReport {
	spec := provider.MustLookup(name)
	report := providerReport{
		Name:                   name,
		HomeEnv:                spec.Home.Env,
		IdentityTier:           spec.IdentityTier().String(),
		DetectsRunningSessions: spec.Session != nil && spec.Session.MayReseed(),
		UsageScope:             usageScopeName(spec),
		Capabilities:           map[string]capabilityReport{},
	}
	for _, capability := range allCapabilities {
		report.Capabilities[string(capability)] = capabilityReport{
			Supported: spec.Can(capability),
			Reason:    spec.Why(capability),
		}
	}

	s, err := a.NewSwitcher(name)
	if err != nil {
		return report
	}
	report.Home = s.Paths.ProviderHome(spec.Home.Env, spec.Home.Default)
	if roster, err := s.RosterOrEmpty(); err == nil {
		report.Accounts = len(roster.Names())
	}
	_, report.LoggedIn = s.LiveIdentity()
	return report
}

func usageScopeName(spec provider.Spec) string {
	scope, ok := spec.UsageScope()
	switch {
	case !ok:
		return "none"
	case scope == provider.UsageLiveOnly:
		return "live-account-only"
	default:
		return "per-account"
	}
}

func (a *App) printDoctor(reports []providerReport) {
	for i, report := range reports {
		if i > 0 {
			a.printer.Println("")
		}
		a.printer.Println(a.printer.Bold(report.Name), " ", a.printer.Muted(report.Home))

		state := "not logged in"
		if report.LoggedIn {
			state = "logged in"
		}
		a.printer.Println("  ", a.printer.Muted(fmt.Sprintf(
			"%d stored, %s · names accounts by %s · usage: %s",
			report.Accounts, state, report.IdentityTier, report.UsageScope)))

		for _, capability := range allCapabilities {
			detail := report.Capabilities[string(capability)]
			if detail.Supported {
				a.printer.Println("  ", a.printer.Accent("✓"), " ", string(capability))
				continue
			}
			a.printer.Println("  ", a.printer.Dimmed("✗"), " ", string(capability))
			a.printer.Println("      ", a.printer.Muted(detail.Reason))
		}

		// The state that has consequences but is not a missing capability:
		// sessions work, and a profile is never refreshed on its own because
		// aaswap cannot prove nothing is using it.
		if report.Capabilities[string(provider.CapSession)].Supported &&
			!report.DetectsRunningSessions {
			a.printer.Println("  ", a.printer.Muted(
				"note: aaswap cannot tell whether a session is running against a "+
					"profile, so it never replaces a profile's credential on its own."))
		}
	}
}
