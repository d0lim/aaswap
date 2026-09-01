package session

import (
	"context"
	"errors"
	"github.com/d0lim/aaswap/internal/provider"
	"os"
	"os/exec"
	"time"
)

// AuthStatusTimeout bounds the probe.
//
// The check itself is local — no API call — but it spawns the whole Claude Code
// CLI, and a cold start on a loaded machine is not instant. Ten seconds is
// generous enough that a timeout means something is genuinely wrong, and a
// timeout is Unknown rather than Invalid precisely because it sometimes is not.
const AuthStatusTimeout = 10 * time.Second

// ClaudeBinary is the command a session launches and probes.
const ClaudeBinary = "claude"

// ExecProber runs the real Claude Code binary.
type ExecProber struct {
	// Binary overrides what is run. Empty resolves ClaudeBinary on PATH.
	Binary string
	// Timeout overrides AuthStatusTimeout.
	Timeout time.Duration
	// Env is the base environment, defaulting to this process's.
	Env []string
	// Spec is the provider being probed, for the environment the probe runs
	// in. The zero value is Claude's, which is the only provider that has an
	// `auth status --json` to ask.
	Spec provider.Spec
}

// AuthStatus asks Claude Code whether a profile is logged in.
func (p ExecProber) AuthStatus(sessionDir string) (AuthStatus, Verdict) {
	binary := p.Binary
	if binary == "" {
		resolved, err := exec.LookPath(ClaudeBinary)
		if err != nil {
			return AuthStatus{}, Unreachable
		}
		binary = resolved
	}
	timeout := p.Timeout
	if timeout == 0 {
		timeout = AuthStatusTimeout
	}
	base := p.Env
	if base == nil {
		base = os.Environ()
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, binary, "auth", "status", "--json")
	// The probe runs in the session's own environment, with the auth overrides
	// dropped — the same environment the session will run in, or the answer
	// would be about a different login than the one about to launch.
	cmd.Env, _ = Environment(base, sessionDir, p.Spec)

	stdout, err := cmd.Output()
	switch {
	case ctx.Err() != nil:
		// The verdict layer reports what the PROBE established, and a timeout
		// established nothing. Deciding from local artifacts is a REUSE
		// judgement and belongs where its answer is consumed — putting it here
		// would make "the probe timed out" and "the profile is bad" the same
		// value again, which is the conflation this taxonomy exists to undo.
		return AuthStatus{}, Unknown
	case err != nil:
		if _, ok := errors.AsType[*exec.ExitError](err); ok {
			// It ran and said no.
			return AuthStatus{}, Invalid
		}
		// It could not be started at all.
		return AuthStatus{}, Unreachable
	}
	return parseAuthStatus(stdout)
}
