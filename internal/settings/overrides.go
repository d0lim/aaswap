package settings

import "slices"

// Overrides carries the policy flags a user passed on the command line.
// A nil field means the flag was absent, which is what lets a flag beat
// settings.json without a zero value silently doing the same.
type Overrides struct {
	Threshold             *float64
	IntervalSeconds       *float64
	CooldownSeconds       *float64
	IncludeAPIKeyAccounts *bool
	Model                 *string
	Strategy              *string
}

// Empty reports whether no override was given.
func (o Overrides) Empty() bool {
	return o == Overrides{}
}

// MergeCLI overlays the given overrides onto loaded settings and clamps the
// result, so a flag is subject to the same bounds as a file value. Flags win
// over settings.json; absent flags leave the file's value alone.
func MergeCLI(auto AutoSwitch, o Overrides) AutoSwitch {
	if o.Empty() {
		return auto
	}
	if o.Threshold != nil {
		auto.Threshold = *o.Threshold
	}
	if o.IntervalSeconds != nil {
		auto.IntervalSeconds = *o.IntervalSeconds
	}
	if o.CooldownSeconds != nil {
		auto.CooldownSeconds = *o.CooldownSeconds
	}
	if o.IncludeAPIKeyAccounts != nil {
		auto.IncludeAPIKeyAccounts = *o.IncludeAPIKeyAccounts
	}
	if o.Model != nil {
		auto.Model = *o.Model
	}
	if o.Strategy != nil {
		auto.Strategy = *o.Strategy
	}
	return Clamp(auto)
}

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
		case KindInt:
			spec.set(&s, int(min(max(float64(spec.get(s).(int)), spec.Lo), spec.Hi)))
		case KindChoice:
			if !slices.Contains(spec.Choices, spec.get(s).(string)) {
				spec.set(&s, spec.Default())
			}
		}
	}
	return s.AutoSwitch
}
