//go:build !windows

package cli

import (
	"fmt"
	"io"
	"syscall"

	"github.com/realiti4/claude-swap/internal/apperr"
)

// handOver replaces this process with Claude Code, and does not return.
//
// Replaced rather than wrapped, for two reasons. An exec'd Claude Code must
// never inherit a held lock — and by this point ccswap holds none, which is what
// makes the exec safe. And a wrapper would sit in the process tree for the
// whole session, taking the signals and job-control events that belong to the
// program the user is actually talking to.
func handOver(binary string, args, env []string, _, _ io.Writer, _ io.Reader) error {
	argv := append([]string{binary}, args...)
	if err := syscall.Exec(binary, argv, env); err != nil {
		return fmt.Errorf("%w: launching Claude Code: %w", apperr.ErrSession, err)
	}
	// Unreachable: a successful exec never comes back.
	return nil
}
