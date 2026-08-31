// Command cswap is the claude-swap CLI: a multi-account switcher for Claude Code.
//
// The Go implementation is being ported from the Python original one layer at a
// time (see the migration plan). Until the CLI layer lands, this entry point
// only reports the build version.
package main

import (
	"fmt"
	"os"

	"github.com/realiti4/claude-swap/internal/buildinfo"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 1 && (args[0] == "--version" || args[0] == "-v") {
		fmt.Println(buildinfo.Version())
		return nil
	}
	return fmt.Errorf("the Go CLI is not wired up yet; use the Python `cswap` for now")
}
