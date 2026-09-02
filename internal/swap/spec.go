package swap

import (
	"path/filepath"

	providerpkg "github.com/d0lim/aaswap/internal/provider"
)

// spec is this switcher's provider declaration.
//
// Every question that used to be answered by comparing the provider name goes
// through here instead: what files hold the login, whether tokens can be
// refreshed, whether sessions can be isolated. A provider this build does not
// know falls back to Claude's declaration rather than to a zero one, because a
// zero Spec declares no secret and would make a swap copy nothing and report
// success.
func (s *Switcher) spec() providerpkg.Spec {
	return providerpkg.MustLookup(s.provider())
}

// Spec is this switcher's provider declaration, for the packages above that
// have to ask what shape the provider is — transfer, and the command layer.
func (s *Switcher) Spec() providerpkg.Spec { return s.spec() }

// liveFileLocations maps each declared file to where it actually is on this
// machine, for the LIVE login rather than a stored copy.
//
// Keyed by declared path so a resolver can find a file by the name its
// declaration uses without knowing where the provider keeps it.
func (s *Switcher) liveFileLocations(spec providerpkg.Spec) map[string]string {
	home := s.Paths.ProviderHome(spec.Home.Env, spec.Home.Default)
	out := make(map[string]string, len(spec.Files)+len(spec.Home.Outside))
	for _, file := range spec.Files {
		out[file.Path] = filepath.Join(home, file.Path)
	}
	for _, file := range spec.Home.Outside {
		out[file.Path] = s.outsideLocation(spec, file)
	}
	return out
}

// outsideLocation is where a file declared outside the provider's home lives.
//
// Claude's config is the only such file, and it does not sit at a fixed path:
// an older install keeps it inside the config home as .config.json, and
// CLAUDE_CONFIG_DIR moves it. GlobalConfigPath already encodes both rules and
// is what every other read of that file goes through, so reproducing the join
// here would give one reader a different answer than the rest.
func (s *Switcher) outsideLocation(spec providerpkg.Spec, file providerpkg.File) string {
	if spec.Name == ProviderClaude && file.Path == claudeConfigFileName {
		return s.Paths.GlobalConfigPath()
	}
	return filepath.Join(s.Paths.Home, file.Path)
}

// claudeConfigFileName is the declared path of Claude's out-of-home config.
const claudeConfigFileName = ".claude.json"

// configFileFor is where this provider's account-scoped config lives, and
// whether it has one at all.
func (s *Switcher) configFileFor(spec providerpkg.Spec) (string, bool) {
	file, ok := spec.ConfigFile()
	if !ok {
		return "", false
	}
	location, found := s.liveFileLocations(spec)[file.Path]
	return location, found
}
