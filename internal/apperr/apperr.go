// Package apperr carries claude-swap's error taxonomy.
//
// The Python original expressed this as an exception hierarchy rooted at
// ClaudeSwitchError, and callers branched on subclasses: the CLI catches the
// root to print "Error: ..." and exit 1 instead of dumping a traceback, while
// specific layers catch CredentialError or LockError. Go has no subclassing, so
// the hierarchy is rebuilt as a chain of sentinels linked by Unwrap: every kind
// unwraps to its parent, and Err sits at the root.
//
// That means errors.Is answers the same questions the except clauses did:
//
//	errors.Is(err, apperr.Err)              // any claude-swap error (CLI)
//	errors.Is(err, apperr.ErrCredential)    // any credential problem
//	errors.Is(err, apperr.ErrCredentialRead) // specifically a failed read
//
// Producers wrap a sentinel to add context, exactly as they would any error:
//
//	return fmt.Errorf("read %s: %w", path, apperr.ErrCredentialRead)
package apperr

import "errors"

// kind is a node in the taxonomy. Error returns only the node's own label —
// the parent is reachable through Unwrap but is deliberately absent from the
// message, so wrapping does not produce "credential: claude-swap error" noise.
type kind struct {
	label  string
	parent error
}

func (k *kind) Error() string { return k.label }
func (k *kind) Unwrap() error { return k.parent }

func derive(label string, parent error) error { return &kind{label: label, parent: parent} }

// Err is the root of the taxonomy: every error claude-swap raises on purpose
// unwraps to it. The CLI treats a match as "expected failure, print and exit 1"
// and anything else as a bug worth a stack trace.
var Err = errors.New("claude-swap error")

// Credential storage and retrieval.
var (
	ErrCredential      = derive("credential error", Err)
	ErrCredentialRead  = derive("failed to read credentials", ErrCredential)
	ErrCredentialWrite = derive("failed to write credentials", ErrCredential)
)

// Configuration and account state.
var (
	ErrConfig          = derive("configuration error", Err)
	ErrSwitch          = derive("account switch failed", Err)
	ErrSession         = derive("session profile error", Err)
	ErrAccountNotFound = derive("account not found", Err)
	ErrValidation      = derive("validation error", Err)
	ErrTransfer        = derive("account transfer error", Err)
)

// Locking. ErrClaudeCodeLockTimeout is distinct because it is not a ccswap
// defect: Claude Code legitimately holds the lock during a token refresh, and
// the right response is to tell the user to retry rather than to fail hard.
var (
	ErrLock                  = derive("failed to acquire lock", Err)
	ErrClaudeCodeLockTimeout = derive("timed out waiting for Claude Code's lock", ErrLock)
)

// Data migrations between on-disk layouts.
//
// ErrMigrationIncomplete is separate from ErrMigration because a partially
// applied migration is recoverable: the records that did move are valid, and
// the run should report what is left rather than roll everything back.
var (
	ErrMigration           = derive("migration error", Err)
	ErrMigrationIncomplete = derive("migration incomplete", Err)
)
