// Package settings holds the user-tunable preferences persisted at
// <backup-root>/settings.json.
//
// One versioned JSON file. v1 carries the autoswitch and ui sections; other
// sections can be added additively, and unknown keys — future fields, another
// tool's experiments — survive a round trip untouched.
//
// Reading is deliberately forgiving: a missing or corrupt file yields defaults
// with a logged warning rather than an error, so a bad hand edit degrades to
// default behaviour instead of bricking the CLI. Writing is deliberately
// strict: `aaswap config set` rejects an out-of-range value loudly, so the user
// learns about the problem when setting it rather than through silently
// degraded behaviour at `aaswap auto` time. [Spec] is the single source of truth
// for the bounds both paths use, so the lenient and strict sides cannot drift.
package settings

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
)

const (
	// SchemaVersion is stamped into settings.json on first write and carried
	// forward unchanged afterwards.
	SchemaVersion = 1
	// FileName is the settings file's name inside the backup root.
	FileName = "settings.json"
)

// AutoSwitch holds the usage-reporting knobs.
//
// Named for the rotation engine it used to configure, which is gone. Only what
// the LISTING reads is left: the threshold it flags an account at, and the
// models whose weekly limits count toward the binding window. The six keys that
// configured the loop itself — interval, cooldown, hysteresis, strategy,
// includeApiKeyAccounts, unhealthyTicks — are no longer offered, because a key
// that reports success and changes nothing is worse than no key at all. An
// existing settings.json keeps them: they are simply not read, and an unrelated
// write leaves them where they are.
//
// The section keeps its name because renaming it means migrating every user's
// settings.json, which is not worth doing on its own.
//
// Threshold is binding-window utilization — the higher of the 5h and 7d
// percentages. At or above it the listing flags the account as worth leaving.
// It is 90 rather than 95 to leave margin for a heavy turn burning past the
// mark between two polls.
type AutoSwitch struct {
	Threshold float64
	// Model is a comma-separated list of model display names ("Fable",
	// "Fable,Opus"), or "all" for every scoped window an account reports. Each
	// named model's per-model weekly limit is folded into the binding window,
	// so the engine switches off an account whose model quota is spent even
	// while its 5h/7d windows still have headroom. Empty means account-wide
	// 5h/7d only, which is the default.
	Model string
}

// UI holds appearance preferences. Theme selects the TUI and CLI color theme;
// "auto" follows terminal-background detection.
type UI struct {
	Theme string
}

// Settings is the whole file's worth of known preferences.
type Settings struct {
	AutoSwitch AutoSwitch
	UI         UI
}

// Defaults returns the settings that apply when nothing is configured.
func Defaults() Settings {
	return Settings{
		AutoSwitch: AutoSwitch{Threshold: 90},
		UI:         UI{Theme: "auto"},
	}
}

// Kind is a setting's value type, which decides how it is parsed and clamped.
type Kind int

// Only the kinds a live key uses. An int and a bool kind existed for the
// rotation knobs; they went with them, because the parse and clamp branches
// behind an unreachable kind are code no test can exercise and no user can
// reach. A key that needs one brings it back along with its reader.
const (
	KindFloat Kind = iota
	KindChoice
	KindString
)

// Spec is the metadata for one user-tunable settings.json key.
//
// It is the single source of truth for bounds and choices: both the lenient
// clamp on load and the strict validation in `aaswap config set` read from here.
type Spec struct {
	// Section is the top-level JSON section ("autoswitch", "ui").
	Section string
	// JSONKey is the camelCase key inside that section. settings.json uses
	// camelCase, matching the repo's other JSON artifacts.
	JSONKey string
	Kind    Kind
	Lo, Hi  float64
	Choices []string
	Help    string

	// get reads this key's effective value out of a loaded Settings.
	get func(Settings) any
	// set writes a decoded value into a Settings. It is used both by the
	// lenient loader (with whatever JSON produced) and after strict parsing.
	set func(*Settings, any)
}

// Dotted is the key as users type it: "autoswitch.threshold".
func (s Spec) Dotted() string { return s.Section + "." + s.JSONKey }

// Default is this key's value when it is absent from settings.json.
func (s Spec) Default() any { return s.get(Defaults()) }

// specs is the registry, in the order `aaswap config` lists them.
var specs = []Spec{
	{
		Section: "autoswitch", JSONKey: "threshold", Kind: KindFloat, Lo: 50, Hi: 99.9,
		Help: "Switch when the binding 5h/7d window reaches this pct",
		get:  func(s Settings) any { return s.AutoSwitch.Threshold },
		set:  func(s *Settings, v any) { s.AutoSwitch.Threshold = v.(float64) },
	},
	{
		Section: "autoswitch", JSONKey: "model", Kind: KindString,
		Help: "Also switch on these models' weekly limits (e.g. Fable, Fable,Opus, or all)",
		get:  func(s Settings) any { return s.AutoSwitch.Model },
		set:  func(s *Settings, v any) { s.AutoSwitch.Model = v.(string) },
	},
	{
		Section: "ui", JSONKey: "theme", Kind: KindChoice,
		Choices: []string{"dark", "light", "auto"},
		Help:    "Color theme; auto follows the terminal background",
		get:     func(s Settings) any { return s.UI.Theme },
		set:     func(s *Settings, v any) { s.UI.Theme = v.(string) },
	},
}

// Specs returns the registry in display order.
func Specs() []Spec { return slices.Clone(specs) }

// SpecFor looks up a spec by dotted key. An unknown key is an error listing the
// valid ones, since that is what the user needs to see next.
func SpecFor(dotted string) (Spec, error) {
	for _, s := range specs {
		if s.Dotted() == dotted {
			return s, nil
		}
	}
	valid := make([]string, len(specs))
	for i, s := range specs {
		valid[i] = s.Dotted()
	}
	return Spec{}, fmt.Errorf(
		"unknown setting %q\nValid keys: %s: %w",
		dotted, strings.Join(valid, ", "), apperr.ErrConfig)
}

// Path returns where settings.json lives for a given backup root.
func Path(backupRoot string) string { return filepath.Join(backupRoot, FileName) }

// ParseModelNames splits a comma-separated model list, trimmed and
// case-insensitively deduped with the first spelling winning.
//
// Shared by the auto engine and the manual switch strategies so both read
// autoswitch.model identically.
func ParseModelNames(value string) []string {
	if value == "" {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for part := range strings.SplitSeq(value, ",") {
		name := strings.TrimSpace(part)
		if name == "" || seen[strings.ToLower(name)] {
			continue
		}
		seen[strings.ToLower(name)] = true
		out = append(out, name)
	}
	return out
}
