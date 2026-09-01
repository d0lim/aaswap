package provider

import "github.com/d0lim/aaswap/internal/procdetect"

// This file is the whole of what aaswap knows about the tools it manages.
//
// Adding a provider means adding a function here. The required part is three
// fields; everything below them is a capability whose absence is reported
// rather than worked around. See docs/PROVIDERS.md.

// claudeSpec declares Claude Code.
//
// The awkward one, and the reason the contract exists. It is the only provider
// whose auth reaches outside its own home directory, and the only one that
// keeps a credential somewhere other than a file — which for years looked like
// the normal shape because it was the only shape implemented.
func claudeSpec() Spec {
	return Spec{
		Name:  Claude,
		Label: "Claude Code",
		Home: Home{
			Env:     "CLAUDE_CONFIG_DIR",
			Default: ".claude",
			// ~/.claude.json sits beside the home rather than inside it, and
			// holds the account's address. No other provider does this.
			Outside: []File{{Path: ".claude.json", Role: RoleIdentity}},
		},
		Files: []File{
			{Path: ".credentials.json", Role: RoleSecret},
			{Path: "settings.json", Role: RoleMachine, Optional: true},
		},
		Login:    &Login{Argv: []string{"claude", "/login"}},
		Identity: claudeIdentity{},
		// The only tool that keeps its live credential anywhere but a file.
		Keychain: true,
		// The only provider that can renew a credential without a new browser
		// round trip: Anthropic publishes an OAuth token endpoint for it.
		Refreshable: true,
		Usage:       perAccountUsage{},
		Session: &Session{
			HomeEnv: "CLAUDE_CONFIG_DIR",
			Argv:    []string{"claude"},
			Share: ShareSet{
				Customizations: []string{
					"settings.json", "keybindings.json", "CLAUDE.md",
					"skills", "commands", "agents",
				},
				History: []string{"projects", "history.jsonl"},
			},
			// Claude Code writes a record per process under sessions/ and a
			// lockfile per editor connection under ide/, for its own use.
			// Reading those is what makes a Claude profile safe to reseed.
			Liveness: procdetectLiveness{},
		},
		Hazards: []Hazard{{
			// The Agent View daemon outlives a session and keeps using the
			// account it started with, so the swap is not complete without it.
			Env: []string{"CLAUDE_CODE_DISABLE_AGENT_VIEW=1"},
		}},
	}
}

// codexSpec declares the OpenAI Codex CLI.
//
// The shape the contract was designed against: one home, one env var, one
// plaintext file that is both the credential and the identity document. No
// Keychain on any platform, which makes it simpler than Claude everywhere
// rather than harder.
func codexSpec() Spec {
	return Spec{
		Name:  Codex,
		Label: "Codex",
		Home:  Home{Env: "CODEX_HOME", Default: ".codex"},
		Files: []File{
			// One file, two roles: the id_token inside it IS the identity.
			{Path: "auth.json", Role: RoleSecret | RoleIdentity},
			// Holds the model, the service tier and the MCP servers. Belongs
			// to the machine, so a switch must leave it alone.
			{Path: "config.toml", Role: RoleMachine, Optional: true},
		},
		Login:    &Login{Argv: []string{"codex", "login"}},
		Identity: codexIdentity{},
		// No refresher. An expired Codex token's only answer is a new login,
		// and pointing Anthropic's refresh endpoint at an OpenAI token would
		// ask the wrong server about the wrong credential.
		Refreshable: false,
		// Codex receives its rate limits in the response to every turn and
		// writes them into the session rollout it already keeps. Reading that
		// costs no request and no quota — but the rollout does not record who
		// was signed in, so only the live account can be spoken for.
		Usage: liveOnlyUsage{},
		Session: &Session{
			HomeEnv: "CODEX_HOME",
			Argv:    []string{"codex"},
			Share: ShareSet{
				Customizations: []string{
					"config.toml", "skills", "rules", "plugins", "hooks.json",
				},
				History: []string{"sessions", "history.jsonl", "session_index.jsonl"},
			},
			// Not yet characterised. ~/.codex/thread-writer-locks/ and the
			// app-server are the candidates. Nil means MayReseed is false, so
			// a Codex profile is never refreshed out from under a session
			// aaswap cannot see — see Session.MayReseed.
			Liveness: nil,
		},
		// The app-server and its SQLite state survive a swap the way Claude's
		// Agent View does, but nobody has confirmed it uses the stale
		// credential. Declaring a guess would be worse than declaring nothing:
		// a wrong Purge entry deletes a user's state.
		Hazards: nil,
	}
}

// claudeIdentity reads an address out of Claude Code's config.
type claudeIdentity struct{}

func (claudeIdentity) Tier() Tier { return TierParse }

func (claudeIdentity) Identify(files map[string]string) (Identity, bool) {
	return ClaudeIdentity(files[".claude.json"])
}

// codexIdentity reads an address out of the id_token in Codex's credential.
type codexIdentity struct{}

func (codexIdentity) Tier() Tier { return TierParse }

func (codexIdentity) Identify(files map[string]string) (Identity, bool) {
	return CodexIdentity(files["auth.json"])
}

// perAccountUsage measures any stored account.
type perAccountUsage struct{}

func (perAccountUsage) Scope() UsageScope { return UsagePerAccount }

// liveOnlyUsage measures only whoever is logged in now.
type liveOnlyUsage struct{}

func (liveOnlyUsage) Scope() UsageScope { return UsageLiveOnly }

// procdetectLiveness reads the process records Claude Code keeps for itself.
//
// Not a guess at what is running: these are the same files Claude Code writes
// and reads, a JSON record per process under sessions/ and a lockfile per
// editor connection under ide/.
type procdetectLiveness struct{}

func (procdetectLiveness) PIDs(profileDir string) ([]int, bool) {
	_, unreadable := procdetect.Scan(profileDir)
	// An unreadable record is not evidence of absence, so it is reported as
	// incomplete and every caller treats that as "something is running".
	return procdetect.PIDs(profileDir), unreadable == 0
}
