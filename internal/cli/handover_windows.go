//go:build windows

package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/d0lim/aaswap/internal/apperr"
)

// handOver runs the provider's tool as a child and mirrors its exit code.
//
// Windows has no exec that replaces the running process, so aaswap stays
// resident as a thin wrapper. The streams are passed through untouched, so the
// session behaves as if the tool had been started directly — apart from one
// extra process in the tree.
func handOver(binary string, args, env []string, stdout, stderr io.Writer, stdin io.Reader) error {
	cmd := exec.Command(binary, args...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			// The tool's own exit code is what the caller's shell must see;
			// wrapping it in aaswap's would hide it.
			os.Exit(exitErr.ExitCode())
		}
		return fmt.Errorf("%w: launching %s: %w",
			apperr.ErrSession, filepath.Base(binary), err)
	}
	return nil
}
