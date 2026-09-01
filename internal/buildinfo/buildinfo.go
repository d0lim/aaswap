// Package buildinfo reports the version of the running binary.
package buildinfo

import (
	"runtime/debug"
	"sync"
)

// version is set at release time via -ldflags "-X ...buildinfo.version=v1.2.3".
// When empty, the version is read from the embedded module build info, which
// `go install module@version` populates and a plain `go build` leaves as
// "(devel)".
var version string

// Version returns the binary's version string.
var Version = sync.OnceValue(func() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "(unknown)"
	}
	return info.Main.Version
})
