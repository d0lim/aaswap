package apperr

import (
	"errors"
	"fmt"
	"testing"
)

// The taxonomy replaces Python's exception hierarchy, so what matters is that
// errors.Is answers exactly what the old except clauses did: a specific error
// matches itself, every ancestor, and nothing off its branch.
func TestHierarchy(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		ancestors []error
		unrelated []error
	}{
		{
			name:      "credential read",
			err:       ErrCredentialRead,
			ancestors: []error{ErrCredential, Err},
			unrelated: []error{ErrCredentialWrite, ErrConfig, ErrLock},
		},
		{
			name:      "credential write",
			err:       ErrCredentialWrite,
			ancestors: []error{ErrCredential, Err},
			unrelated: []error{ErrCredentialRead, ErrSwitch},
		},
		{
			// The one the CLI treats specially: Claude Code holding its own
			// lock is not a cswap defect, but it is still a lock error.
			name:      "Claude Code lock timeout",
			err:       ErrClaudeCodeLockTimeout,
			ancestors: []error{ErrLock, Err},
			unrelated: []error{ErrCredential, ErrMigration},
		},
		{
			name:      "migration",
			err:       ErrMigration,
			ancestors: []error{Err},
			unrelated: []error{ErrMigrationIncomplete, ErrTransfer},
		},
		{
			// Incomplete is a sibling of migration, not a child: a partially
			// applied migration is recoverable and reported differently.
			name:      "migration incomplete",
			err:       ErrMigrationIncomplete,
			ancestors: []error{Err},
			unrelated: []error{ErrMigration},
		},
		{
			name:      "account not found",
			err:       ErrAccountNotFound,
			ancestors: []error{Err},
			unrelated: []error{ErrValidation, ErrSession},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !errors.Is(tt.err, tt.err) {
				t.Error("an error does not match itself")
			}
			for _, ancestor := range tt.ancestors {
				if !errors.Is(tt.err, ancestor) {
					t.Errorf("errors.Is(%v, %v) = false, want true", tt.err, ancestor)
				}
			}
			for _, other := range tt.unrelated {
				if errors.Is(tt.err, other) {
					t.Errorf("errors.Is(%v, %v) = true, want false", tt.err, other)
				}
			}
		})
	}
}

// Producers wrap a sentinel to add context. The wrapped error must keep
// answering the whole chain, which is what lets the CLI catch the root.
func TestWrappingPreservesTheChain(t *testing.T) {
	wrapped := fmt.Errorf("read %s: %w", "/tmp/creds.json", ErrCredentialRead)

	for _, ancestor := range []error{ErrCredentialRead, ErrCredential, Err} {
		if !errors.Is(wrapped, ancestor) {
			t.Errorf("wrapped error lost its link to %v", ancestor)
		}
	}
	if got, want := wrapped.Error(), "read /tmp/creds.json: failed to read credentials"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// A node's message is its own label only. Chaining must not accumulate every
// ancestor's text into "credential: claude-swap error" noise.
func TestMessagesDoNotAccumulate(t *testing.T) {
	if got, want := ErrCredentialRead.Error(), "failed to read credentials"; got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

// Everything claude-swap raises on purpose must reach the root, because the CLI
// uses that match to decide between a clean message and a stack trace.
func TestEveryKindReachesTheRoot(t *testing.T) {
	all := map[string]error{
		"ErrCredential":            ErrCredential,
		"ErrCredentialRead":        ErrCredentialRead,
		"ErrCredentialWrite":       ErrCredentialWrite,
		"ErrConfig":                ErrConfig,
		"ErrSwitch":                ErrSwitch,
		"ErrSession":               ErrSession,
		"ErrAccountNotFound":       ErrAccountNotFound,
		"ErrValidation":            ErrValidation,
		"ErrTransfer":              ErrTransfer,
		"ErrLock":                  ErrLock,
		"ErrClaudeCodeLockTimeout": ErrClaudeCodeLockTimeout,
		"ErrMigration":             ErrMigration,
		"ErrMigrationIncomplete":   ErrMigrationIncomplete,
	}
	for name, err := range all {
		if !errors.Is(err, Err) {
			t.Errorf("%s does not unwrap to the root sentinel", name)
		}
	}
}
