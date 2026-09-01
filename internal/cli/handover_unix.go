//go:build !windows

package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"syscall"

	"github.com/d0lim/aaswap/internal/apperr"
)

// handOver replaces this process with the provider's tool, and does not return.
//
// Replaced rather than wrapped, for two reasons. The exec'd tool must never
// inherit a held lock — and by this point aaswap holds none, which is what makes
// the exec safe. And a wrapper would sit in the process tree for the whole
// session, taking the signals and job-control events that belong to the program
// the user is actually talking to.
func handOver(binary string, args, env []string, _, _ io.Writer, _ io.Reader) error {
	argv := append([]string{binary}, args...)
	if err := syscall.Exec(binary, argv, env); err != nil {
		return fmt.Errorf("%w: launching %s: %w",
			apperr.ErrSession, filepath.Base(binary), err)
	}
	// Unreachable: a successful exec never comes back.
	return nil
}
