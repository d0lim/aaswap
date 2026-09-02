// Package platform classifies the host OS into the four cases aaswap
// actually branches on.
//
// The distinction that matters is not GOOS alone: WSL is a Linux kernel that
// stores its data under the XDG layout but sits on a Windows filesystem, and it
// has to be told apart from native Linux. Everything downstream (credential
// backend, backup root, symlink-vs-copy sharing) keys off this type rather than
// re-testing runtime.GOOS in a dozen places.
package platform

import (
	"os"
	"runtime"
)

// Platform is the host operating system, as aaswap classifies it.
type Platform int

const (
	Unknown Platform = iota
	MacOS
	Linux
	WSL
	Windows
)

// String implements fmt.Stringer.
func (p Platform) String() string {
	switch p {
	case MacOS:
		return "macos"
	case Linux:
		return "linux"
	case WSL:
		return "wsl"
	case Windows:
		return "windows"
	default:
		return "unknown"
	}
}

// UsesXDG reports whether this platform stores aaswap data under the XDG
// Base Directory layout rather than the legacy ~/.claude-swap-backup.
func (p Platform) UsesXDG() bool {
	return p == Linux || p == WSL
}

// IsWindows reports whether this platform is Windows proper. WSL is not
// Windows for any purpose in this codebase: it runs the POSIX code paths.
func (p Platform) IsWindows() bool {
	return p == Windows
}

// Detect classifies the running host.
//
// WSL is identified by WSL_DISTRO_NAME, which the WSL init sets for every
// session; the Python original used the same signal, and it is the only one
// that does not require reading /proc.
func Detect() Platform {
	switch runtime.GOOS {
	case "darwin":
		return MacOS
	case "windows":
		return Windows
	case "linux":
		if os.Getenv("WSL_DISTRO_NAME") != "" {
			return WSL
		}
		return Linux
	default:
		return Unknown
	}
}
