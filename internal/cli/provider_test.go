package cli

import (
	"os"
	"testing"

	"github.com/d0lim/aaswap/internal/swap"
)

// The dimension is a flag with a default, the way gh does hosts. Every command
// that addresses an account has to accept it, or a second provider's accounts
// are unreachable.
func TestTheProviderFlagReachesTheSwitcher(t *testing.T) {
	h := newHarness(t)
	var asked []string
	base := h.app.NewSwitcher
	h.app.NewSwitcher = func(provider string) (*swap.Switcher, error) {
		asked = append(asked, provider)
		return base(provider)
	}

	if code := h.run("--provider", "codex", "list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if len(asked) == 0 || asked[len(asked)-1] != swap.ProviderCodex {
		t.Errorf("switcher built for %v, want codex", asked)
	}

	// The provider has to reach the factory, not be assigned afterwards: the
	// credential store and the profile store are both scoped by it, and a
	// Switcher built for one and relabelled the other reads the right roster
	// section through the wrong provider's files.
	if code := h.run("list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if asked[len(asked)-1] != swap.ProviderClaude {
		t.Errorf("built for %q with no flag, want the default", asked[len(asked)-1])
	}
}

// An unknown provider must be refused before anything reads a store: a typo
// would otherwise create an empty section and report no accounts.
func TestAnUnknownProviderIsRefused(t *testing.T) {
	h := newHarness(t)
	if code := h.run("--provider", "nonsense", "list"); code != ExitError {
		t.Fatalf("exit = %d, want a refusal: %s", code, h.stdout())
	}
	wantContains(t, h.stderr(), "nonsense", "claude", "codex")
}

// The env var is the other half of the flag: it makes a shell session stick to
// one provider without repeating the flag.
func TestTheProviderEnvVarIsHonoured(t *testing.T) {
	t.Setenv(ProviderEnv, "codex")
	if got := providerFromEnv(); got != "codex" {
		t.Errorf("providerFromEnv() = %q, want codex", got)
	}
	if err := os.Unsetenv(ProviderEnv); err != nil {
		t.Fatal(err)
	}
	if got := providerFromEnv(); got != "" {
		t.Errorf("providerFromEnv() = %q with nothing set, want empty", got)
	}
}
