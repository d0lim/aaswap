package swap

import (
	"context"

	"github.com/d0lim/aaswap/internal/claudeapi"
	providerpkg "github.com/d0lim/aaswap/internal/provider"
)

// codexFetcher reports a Codex account's rate limits from what Codex already
// recorded on this machine.
//
// # Why it makes no request
//
// Codex receives its rate limits in the response to every turn and writes them
// into the session rollout it is already keeping. Reading that costs nothing
// and consumes no quota, where asking OpenAI would do both — and its usage
// endpoint sits behind a challenge a plain HTTP client does not pass.
//
// # Why only the active account gets a number
//
// The rollout does not record which account was signed in, so a measurement
// can only be attributed to whoever is live now. An idle Codex account reports
// no measurement rather than someone else's, and rather than its own stale one:
// the windows it last had have probably reset.
//
// Reporting nothing is safe by construction. An account with no measurement
// reads as unknown, never as exhausted, so it is neither switched away from as
// spent nor chosen as a target on numbers that are not there.
type codexFetcher struct {
	codexHome string
	// activeAccount is the account the recorded numbers belong to, empty when
	// nothing is logged in.
	activeAccount string
}

func (c codexFetcher) FetchUsageForAccount(_ context.Context, req claudeapi.FetchRequest) claudeapi.UsageOutcome {
	if c.activeAccount == "" || req.AccountNum != c.activeAccount {
		return claudeapi.UsageOutcome{}
	}
	result, _, ok := providerpkg.CodexUsage(c.codexHome)
	if !ok {
		return claudeapi.UsageOutcome{}
	}
	return claudeapi.UsageOutcome{Usage: result}
}

// liveOnlyUsageFetcher builds the fetcher for a provider that can only speak
// for whoever is logged in now.
func (s *Switcher) liveOnlyUsageFetcher() UsageFetcher {
	active := ""
	if identity, ok := s.LiveIdentity(); ok {
		if roster, err := s.RosterOrEmpty(); err == nil {
			if name, found := roster.FindName(identity.Identity()); found {
				active = name
			}
		}
	}
	return codexFetcher{codexHome: s.Paths.CodexHome(), activeAccount: active}
}
