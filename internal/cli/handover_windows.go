//go:build windows

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"

	"github.com/d0lim/ccswap/internal/apperr"
)

// handOver runs Claude Code as a child and mirrors its exit code.
//
// Windows has no exec that replaces the running process, so ccswap stays
// resident as a thin wrapper. The streams are passed through untouched, so the
// session behaves as if Claude Code had been started directly — apart from one
// extra process in the tree.
func handOver(binary string, args, env []string, stdout, stderr io.Writer, stdin io.Reader) error {
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// Claude Code's own exit code is what the caller's shell must see;
			// wrapping it in ccswap's would hide it.
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("%w: launching Claude Code: %w", apperr.ErrSession, err)
	}
	return nil
}
