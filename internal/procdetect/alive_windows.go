package procdetect

import (
	"errors"

	"golang.org/x/sys/windows"
)

// Windows has no signals, and os.Process.Signal rejects everything but
// os.Kill — it answers "not supported by windows" for signal 0. Probing that
// way reports EVERY pid as dead, including this process, which would tell
// ccswap that no Claude Code is ever running and let it swap a credential out
// from under a live session.
//
// OpenProcess plus the exit code is the actual question. ERROR_ACCESS_DENIED
// means the process exists but is not ours to inspect — alive. Any other open
// failure means there is no such process.
func pidAlive(pid int) bool {
	// QUERY_LIMITED_INFORMATION is the narrowest right that answers this, and
	// the one most likely to be granted across an integrity boundary.
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return errors.Is(err, windows.ERROR_ACCESS_DENIED)
	}
	defer windows.CloseHandle(handle)

	var code uint32
	if err := windows.GetExitCodeProcess(handle, &code); err != nil {
		// The handle opened, so the process object exists. Erring toward alive
		// is the safe direction: it makes ccswap defer, never yank.
		return true
	}
	// STILL_ACTIVE. A process that genuinely exited with 259 reads as alive,
	// which is the same safe direction — and Claude Code does not use it.
	return code == 259
}
