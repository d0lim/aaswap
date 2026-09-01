package swap

import (
	"io"
	"log/slog"
	"os"
	"testing"
)

// TestMain silences the package's structured logging.
//
// These paths log deliberately — a stashed credential, a classified backup —
// and that output belongs in a user's log file, not interleaved with test
// results where it reads as failure.
func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}
