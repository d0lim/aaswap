// Command ccswap manages several Claude Code logins on one machine.
//
// The entry point is deliberately thin: it wires signals to a context and hands
// everything else to internal/cli, which returns an exit code rather than
// calling os.Exit — so the whole command surface stays testable.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/d0lim/ccswap/internal/cli"
)

func main() {
	// A first interrupt cancels the context so an in-flight operation can
	// unwind — releasing locks, completing a rollback. A second one is handled
	// by the runtime's default, which kills the process outright: someone
	// pressing it twice means now.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.New().Execute(ctx, os.Args[1:])
	// Released before the exit rather than deferred: os.Exit does not run
	// deferred functions, and the signal handler must be torn down explicitly
	// or it outlives the work it was guarding.
	stop()
	os.Exit(code)
}
