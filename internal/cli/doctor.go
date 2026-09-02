package cli

import (
	"fmt"

	"github.com/d0lim/aaswap/internal/paths"
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
	provider.CapToken,
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
	// Unclaimed counts credentials a switch preserved but could not file
	// against any managed account. They are not lost, but nothing will use
	// them either, so they need saying out loud somewhere.
	Unclaimed int `json:"unclaimed"`

	Capabilities map[string]capabilityReport `json:"capabilities"`
}

type doctorPayload struct {
	SchemaVersion int              `json:"schemaVersion"`
	Providers     []providerReport `json:"providers"`
	// Predecessor names a store left by an earlier version of this tool, empty
	// when there is none. Reported rather than adopted: importing another
	// installation's credentials is a decision, not a diagnostic.
	Predecessor     string `json:"predecessor,omitzero"`
	PredecessorPath string `json:"predecessorPath,omitzero"`
}

func (a *App) runDoctor() error {
	reports := make([]providerReport, 0, len(provider.Names()))
	for _, name := range provider.Names() {
		reports = append(reports, a.reportProvider(name))
	}

	payload := doctorPayload{SchemaVersion: 1, Providers: reports}
	if resolver, err := a.resolver(); err == nil {
		if found, ok := resolver.FindPredecessor(); ok {
			payload.Predecessor, payload.PredecessorPath = found.Name, found.Root
		}
	}

	if a.json {
		a.emitJSON(payload)
		return nil
	}
	a.printDoctor(payload)
	return nil
}

// resolver is the path resolver, borrowed from any provider's switcher.
//
// Predecessor detection is about the machine rather than a provider, but only a
// switcher carries a resolver, so one is asked for it.
func (a *App) resolver() (*paths.Resolver, error) {
	s, err := a.NewSwitcher(provider.Claude)
	if err != nil {
		return nil, err
	}
	return s.Paths, nil
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
	if entries, _ := s.Creds.ListUnclaimed(); entries != nil {
		report.Unclaimed = len(entries)
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

func (a *App) printDoctor(payload doctorPayload) {
	for i, report := range payload.Providers {
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

		// Preserved-but-unfiled credentials are easy to never learn about:
		// nothing lists them unless you already know the command. Naming the
		// command here is the whole point — inspecting and dropping them stays
		// where it is, because one of those destroys a credential and this
		// command must not.
		if report.Unclaimed > 0 {
			a.printer.Println("  ", a.printer.Muted(fmt.Sprintf(
				"note: %d preserved credential(s) belong to no managed account. "+
					"Inspect with `aaswap --provider %s account unclaimed`.",
				report.Unclaimed, report.Name)))
		}
	}

	// A store left by an earlier name of this tool. Reported, never adopted on
	// its own: copying another installation's credentials is a decision.
	if payload.Predecessor != "" {
		a.printer.Println("")
		a.printer.Println(a.printer.Muted(fmt.Sprintf(
			"Found a %s store at %s — `aaswap account adopt` moves it over.",
			payload.Predecessor, payload.PredecessorPath)))
	}
}
