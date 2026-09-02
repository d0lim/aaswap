package settings

import "slices"

// Clamp brings every value back inside its spec's range, reverting an
// unsupported choice to its default. It is the typed counterpart of the
// per-field coercion the loader applies to raw JSON, and reads the same [Spec]
// bounds so the two cannot drift.
func Clamp(auto AutoSwitch) AutoSwitch {
	s := Settings{AutoSwitch: auto}
	for _, spec := range specs {
		if spec.Section != "autoswitch" {
			continue
		}
		switch spec.Kind {
		case KindFloat:
			spec.set(&s, min(max(spec.get(s).(float64), spec.Lo), spec.Hi))
		case KindChoice:
			if !slices.Contains(spec.Choices, spec.get(s).(string)) {
				spec.set(&s, spec.Default())
			}
		}
	}
	return s.AutoSwitch
}
