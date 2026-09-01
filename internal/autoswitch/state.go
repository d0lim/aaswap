package autoswitch

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/fsutil"
	"github.com/d0lim/aaswap/internal/lockfile"
)

// State is what the engine remembers between ticks.
type State struct {
	SchemaVersion int `json:"schemaVersion"`

	// LastSwitchAt drives the cooldown.
	LastSwitchAt *float64 `json:"lastSwitchAt,omitzero"`
	LastSwitchTo string   `json:"lastSwitchTo,omitzero"`
	// LastSwitchFrom is where the last switch came FROM, so the next tick can
	// refuse to undo it.
	LastSwitchFrom string `json:"lastSwitchFrom,omitzero"`

	// LeftHeadroom and LeftRecoveryAt record what the account left behind
	// LOOKED LIKE, so the refusal above has a release that burning cannot fake.
	// The present state alone cannot supply one: every return looks the same
	// whether the target recovered or the active merely burned down.
	LeftHeadroom   *float64 `json:"leftHeadroom,omitzero"`
	LeftRecoveryAt *float64 `json:"leftRecoveryAt,omitzero"`
	// LeftTrigger records WHY, so the reader never has to infer it from which
	// fields happen to be null — two different departures can write the same
	// pair of nulls.
	LeftTrigger string `json:"leftTrigger,omitzero"`

	// Quarantine holds accounts the engine will not switch to, keyed by slot.
	Quarantine map[string]*QuarantineEntry `json:"quarantine,omitzero"`

	Extra map[string]jsontext.Value `json:",embed"`
}

// QuarantineEntry is one account held out of rotation.
type QuarantineEntry struct {
	Email  string `json:"email"`
	Reason string `json:"reason"`
	At     string `json:"at"`
	// RefreshTokenFingerprint identifies the credential GENERATION the
	// quarantine condemned. A different one means the user re-logged in, so
	// the dead lineage is gone and the account re-enters rotation on its own.
	RefreshTokenFingerprint string `json:"refreshTokenFingerprint,omitzero"`

	Extra map[string]jsontext.Value `json:",embed"`
}

// Store reads and writes the engine's state.
type Store struct {
	path     string
	lockPath string
	// LockTimeout bounds how long a mutation waits.
	LockTimeout time.Duration
}

// NewStore returns a state store over a backup root.
func NewStore(backupRoot string) *Store {
	return &Store{
		path:        filepath.Join(backupRoot, StateFileName),
		lockPath:    filepath.Join(backupRoot, ".autoswitch_state.lock"),
		LockTimeout: lockfile.DefaultTimeout,
	}
}

// Path is the state file's location. Production never needs it — the store
// owns its own file — but a test that has to inspect or corrupt the state on
// disk should not be reconstructing the path by hand.
func (s *Store) Path() string { return s.path }

// Read loads the state, returning an empty one for anything unusable.
//
// The state is the engine's memory, not its configuration: losing it costs one
// cooldown and one round of re-quarantining, so refusing to run over a corrupt
// file would be far worse than starting fresh.
func (s *Store) Read() State {
	data, err := os.ReadFile(s.path)
	if err != nil {
		return State{SchemaVersion: StateSchemaVersion}
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		slog.Warn("the auto-switch state file is malformed; starting fresh",
			"path", s.path, "error", err)
		return State{SchemaVersion: StateSchemaVersion}
	}
	state.SchemaVersion = StateSchemaVersion
	return state
}

// Mutate reads, modifies and writes the state under its lock.
//
// The lock is what stops two engines — a loop and a cron-driven single tick —
// from overwriting each other's cooldown and quarantine. It is never held while
// any other lock is: the switch path takes the store lock and Claude Code's,
// and taking this one inside that order would build a cycle.
func (s *Store) Mutate(apply func(*State)) (State, error) {
	var out State
	err := s.withLock(func() error {
		state := s.Read()
		apply(&state)
		out = state
		return s.write(state)
	})
	return out, err
}

// WithLock runs fn under the state lock, so a read-decide-write sequence is one
// transaction.
func (s *Store) WithLock(fn func() error) error { return s.withLock(fn) }

func (s *Store) withLock(fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(s.lockPath), 0o700); err != nil {
		return fmt.Errorf("%w: creating the state directory: %w", apperr.ErrConfig, err)
	}
	return lockfile.With(s.lockPath, s.LockTimeout, fn)
}

// Write publishes the state. The caller holds the lock.
func (s *Store) Write(state State) error { return s.write(state) }

func (s *Store) write(state State) error {
	state.SchemaVersion = StateSchemaVersion
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("%w: creating the state directory: %w", apperr.ErrConfig, err)
	}
	data, err := json.Marshal(state, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("%w: encoding the auto-switch state: %w", apperr.ErrConfig, err)
	}
	return fsutil.WriteFileAtomic(s.path, append(data, '\n'))
}

// InCooldown reports whether a switch happened too recently to make another.
//
// The cooldown applies only to PROACTIVE moves. An account at its limit or one
// whose usage cannot be read is an escape, and making the user wait out a
// cooldown there would leave them stuck on a dead account.
func (s State) InCooldown(now time.Time, cooldown time.Duration) bool {
	if s.LastSwitchAt == nil {
		return false
	}
	elapsed := now.Sub(time.UnixMilli(int64(*s.LastSwitchAt * 1000)))
	return elapsed < cooldown
}

// Quarantined reports the slots currently held out of rotation.
func (s State) Quarantined() map[string]bool {
	out := make(map[string]bool, len(s.Quarantine))
	for num := range s.Quarantine {
		out[num] = true
	}
	return out
}

func epochOf(t time.Time) *float64 {
	if t.IsZero() {
		return nil
	}
	seconds := float64(t.UnixMilli()) / 1000
	return &seconds
}
