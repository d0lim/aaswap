package provider

import "slices"

// Environment is the process environment for running this provider's tool
// against a given home: the home variable set to it, every auth override the
// declaration names scrubbed, and every hazard's variables added.
//
// One function for both uses — a session profile and a login sandbox — because
// they are the same operation: point the tool somewhere else, and make sure
// nothing in the environment overrides where it then reads its credential
// from. The names scrubbed are returned so the caller can say so.
func Environment(base []string, home string, spec Spec) (env, scrubbed []string) {
	homeEnv := spec.Home.Env
	var overrides []string
	if s := spec.Session; s != nil {
		if s.HomeEnv != "" {
			homeEnv = s.HomeEnv
		}
		overrides = s.AuthOverrides
	}
	for _, entry := range base {
		name, _, _ := cutName(entry)
		if name == homeEnv {
			continue
		}
		if slices.Contains(overrides, name) {
			scrubbed = append(scrubbed, name)
			continue
		}
		env = append(env, entry)
	}
	env = append(env, homeEnv+"="+home)
	// Whatever this provider declares survives a swap and has to be disabled:
	// Claude Code's Agent View daemon outlives a session and keeps using the
	// account it started with.
	for _, hazard := range spec.Hazards {
		env = append(env, hazard.Env...)
	}
	return env, scrubbed
}

func cutName(entry string) (name, value string, found bool) {
	for i := range len(entry) {
		if entry[i] == '=' {
			return entry[:i], entry[i+1:], true
		}
	}
	return entry, "", false
}
