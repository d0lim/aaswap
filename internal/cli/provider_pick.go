package cli

import (
	"cmp"
	"fmt"
	"strconv"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/swap"
)

// ProviderChoice is one provider a person can pick, with the one fact that
// tells the choices apart on a machine where several tools are managed.
type ProviderChoice struct {
	Name     string
	Label    string
	Accounts int
}

// resolveProvider decides which provider an invocation addresses.
//
// No tool is the default. The name comes from --provider, then the pinned
// environment, then from the store itself when exactly one provider has
// accounts in it — someone who has only ever stored Codex accounts means Codex
// when they say `aaswap login`. Past that there is nothing to go on, and the
// caller is handed the choices to put to a person: an empty name with a
// non-empty list.
//
// A hardcoded fallback is what this replaces. It made `aaswap login` on a
// fresh machine report "no active Claude Code account" to someone who had
// never installed Claude Code, and never named the flag that would fix it.
func (a *App) resolveProvider() (string, []ProviderChoice, error) {
	if name := cmp.Or(a.provider, providerFromEnv()); name != "" {
		// Refused before anything reads a store: a typo must not quietly
		// create an empty section and report no accounts.
		if !swap.KnownProvider(name) {
			return "", nil, fmt.Errorf("%w: %q is not a provider this build manages. Known: %s",
				apperr.ErrValidation, name, strings.Join(swap.Providers(), ", "))
		}
		return name, nil, nil
	}
	choices, err := a.providerCensus()
	if err != nil {
		return "", nil, err
	}
	var stored []ProviderChoice
	for _, choice := range choices {
		if choice.Accounts > 0 {
			stored = append(stored, choice)
		}
	}
	if len(stored) == 1 {
		return stored[0].Name, nil, nil
	}
	return "", choices, nil
}

// providerCensus counts the stored accounts of every provider this build
// manages, in registry order.
func (a *App) providerCensus() ([]ProviderChoice, error) {
	var out []ProviderChoice
	for _, name := range swap.Providers() {
		s, err := a.NewSwitcher(name)
		if err != nil {
			return nil, err
		}
		roster, err := s.RosterOrEmpty()
		if err != nil {
			return nil, err
		}
		out = append(out, ProviderChoice{
			Name:     name,
			Label:    provider.MustLookup(name).DisplayName(),
			Accounts: len(roster.Accounts),
		})
	}
	return out, nil
}

// pickProvider puts the choice to a person, or says why it cannot.
func (a *App) pickProvider(choices []ProviderChoice) (string, error) {
	if !a.interactive() {
		return "", errProviderUnspecified(choices)
	}
	options := make([]Choice, 0, len(choices)+1)
	for i, choice := range choices {
		options = append(options, Choice{
			Key:   strconv.Itoa(i + 1),
			Label: choice.Label + "  " + accountsWord(choice.Accounts),
		})
	}
	options = append(options, Choice{Key: "q", Label: "cancel"})
	answer := a.choose("Which tool is this for?", options)
	index, err := strconv.Atoi(answer)
	if err != nil || index < 1 || index > len(choices) {
		return "", fmt.Errorf("%w: cancelled, no provider chosen", apperr.ErrValidation)
	}
	return choices[index-1].Name, nil
}

// errProviderUnspecified is the answer where no one can be asked. It names
// both ways to say which, because the reader is a script's author.
func errProviderUnspecified(choices []ProviderChoice) error {
	names := make([]string, len(choices))
	for i, choice := range choices {
		names[i] = choice.Name
	}
	return fmt.Errorf("%w: say which tool this is for with --provider <name> or %s=<name>. Providers: %s",
		apperr.ErrValidation, ProviderEnv, strings.Join(names, ", "))
}

// accountsWord is the count as a person reads it.
func accountsWord(n int) string {
	switch n {
	case 0:
		return "no accounts stored"
	case 1:
		return "1 account stored"
	}
	return fmt.Sprintf("%d accounts stored", n)
}
