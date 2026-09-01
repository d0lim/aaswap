package platform

import (
	"runtime"
	"testing"
)

func TestClassification(t *testing.T) {
	tests := []struct {
		p         Platform
		str       string
		usesXDG   bool
		isWindows bool
	}{
		{MacOS, "macos", false, false},
		// WSL runs the POSIX code paths and the XDG layout; it is deliberately
		// not Windows for any purpose in this codebase.
		{Linux, "linux", true, false},
		{WSL, "wsl", true, false},
		{Windows, "windows", false, true},
		{Unknown, "unknown", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.str, func(t *testing.T) {
			if got := tt.p.String(); got != tt.str {
				t.Errorf("String() = %q, want %q", got, tt.str)
			}
			if got := tt.p.UsesXDG(); got != tt.usesXDG {
				t.Errorf("UsesXDG() = %v, want %v", got, tt.usesXDG)
			}
			if got := tt.p.IsWindows(); got != tt.isWindows {
				t.Errorf("IsWindows() = %v, want %v", got, tt.isWindows)
			}
		})
	}
}

func TestDetect(t *testing.T) {
	// GOOS is fixed at compile time, so only the branch for the host OS is
	// reachable here; the WSL split is the one Detect decides at runtime.
	switch runtime.GOOS {
	case "darwin":
		if got := Detect(); got != MacOS {
			t.Errorf("Detect() on darwin = %v, want macos", got)
		}
	case "windows":
		if got := Detect(); got != Windows {
			t.Errorf("Detect() on windows = %v, want windows", got)
		}
	case "linux":
		t.Setenv("WSL_DISTRO_NAME", "")
		if got := Detect(); got != Linux {
			t.Errorf("Detect() without WSL_DISTRO_NAME = %v, want linux", got)
		}
		t.Setenv("WSL_DISTRO_NAME", "Ubuntu")
		if got := Detect(); got != WSL {
			t.Errorf("Detect() with WSL_DISTRO_NAME = %v, want wsl", got)
		}
	default:
		if got := Detect(); got != Unknown {
			t.Errorf("Detect() on %s = %v, want unknown", runtime.GOOS, got)
		}
	}
}
