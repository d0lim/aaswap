//go:build !windows

package procdetect

import (
	"os"
	"syscall"
)

// Signal 0 is the POSIX liveness probe: it runs the kernel's permission and
// existence checks and delivers nothing.
//
// EPERM means the process EXISTS and belongs to someone else, which is still
// alive. "Cannot tell" has to read as alive here, because the caller's other
// branch is to pull a credential out from under a running Claude Code.
func pidAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := process.Signal(syscall.Signal(0)); err == nil {
		return true
	} else {
		return os.IsPermission(err)
	}
}
