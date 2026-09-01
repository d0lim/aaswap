package provider

import "slices"

// Provider names. These are what `--provider` accepts and what a store's
// sections are keyed by, so they are on-disk identifiers and cannot be renamed.
const (
	Claude = "claude"
	Codex  = "codex"
)

// registry is every provider this build can manage, in the order they are
// shown.
//
// A slice rather than a map: the order is user visible in `--provider` help and
// in `doctor`, and map iteration would vary it between runs.
//
// A store may hold sections for providers not listed here — a newer release's —
// and those round-trip untouched. This list is what a person may ADDRESS.
var registry = []Spec{claudeSpec(), codexSpec()}

// Lookup returns a provider's declaration.
func Lookup(name string) (Spec, bool) {
	index := slices.IndexFunc(registry, func(s Spec) bool { return s.Name == name })
	if index < 0 {
		return Spec{}, false
	}
	return registry[index], true
}

// Names lists every addressable provider, in the order they are shown.
func Names() []string {
	out := make([]string, len(registry))
	for i, spec := range registry {
		out[i] = spec.Name
	}
	return out
}

// Known reports whether a name is one this build can manage.
func Known(name string) bool {
	_, ok := Lookup(name)
	return ok
}

// MustLookup returns a declaration for a provider that has already been
// validated, falling back to Claude's.
//
// The fallback is deliberate. An unrecognised provider reaching this far is a
// bug upstream, and the alternatives are worse than being wrong about which
// tool is being managed: a zero Spec has no secret file, so a swap would copy
// nothing and report success.
func MustLookup(name string) Spec {
	if spec, ok := Lookup(name); ok {
		return spec
	}
	spec, _ := Lookup(Claude)
	return spec
}
