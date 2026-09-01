package keychain

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// AllowRealKeychainEnv opts a test into running against the host's real
// Keychain. It exists for the one round-trip test that has to prove the wrapper
// works against the actual security(1) binary — the equivalent of the Python
// suite's no_keychain_fake marker — and must never be set for any other test.
const AllowRealKeychainEnv = "AASWAP_ALLOW_REAL_KEYCHAIN"

// execRunner runs the real security(1) binary.
type execRunner struct{}

// Run executes security(1) and reports its exit status.
//
// A non-zero exit is a *Result*, not an error: exit codes carry meaning here
// (44 is "not found"), and collapsing them into an error would lose the
// distinction between "no such item" and "the Keychain is unusable". An error
// is reserved for not being able to run the binary at all, or for the timeout.
func (execRunner) Run(ctx context.Context, args []string, stdin string) (Result, error) {
	guardRealKeychain()

	cmd := exec.CommandContext(ctx, SecurityBinary, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return res, nil
	}
	// A killed process reports a meaningless exit status, so the deadline is
	// checked before the exit code is trusted.
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	// The binary is missing or could not be started at all — on a non-macOS
	// host, or a stripped-down image.
	return res, err
}

// guardRealKeychain stops a test binary from touching the developer's real
// Keychain.
//
// aaswap's Keychain items are live Claude Code logins. A test that reached
// them could delete or overwrite the developer's own accounts, so the real
// binary is unreachable from a test unless a test explicitly opts in. This is
// the Keychain half of the safety net that paths.guardRealStore provides for
// the filesystem.
func guardRealKeychain() {
	if !testing.Testing() || os.Getenv(AllowRealKeychainEnv) != "" {
		return
	}
	panic("aaswap test safety net: a test tried to run " + SecurityBinary +
		", which would touch the developer's real Keychain.\n" +
		"Build the Keychain with keychain.NewWithRunner and a fake runner, or — " +
		"only for a test that genuinely needs the real binary — set " +
		AllowRealKeychainEnv + "=1 for that test alone.")
}
