// Package migrate holds ccswap's one-time, run-once data migrations.
//
// A small, boring home for compatibility migrations so they do not pollute the
// core switch, read and write flow. Every migration:
//
//   - is idempotent and self-guarded — safe to run twice, and safe even when
//     the state file is missing or corrupt,
//   - reports Completed when it finished, and the runner records it as applied,
//   - reports Skipped when it was not applicable, and the runner records
//     nothing, so a later-restored backup can still trigger it,
//   - returns an error when it PARTIALLY failed. The runner logs it and leaves
//     it unmarked, so the next run retries.
//
// Applied migrations are tracked in <backup-root>/.migrations.json:
//
//	{"version": 1, "applied": {"windows_keyring_to_files": "<iso-timestamp>"}}
//
// The runner is called once at startup; after the state file records a
// migration it short-circuits on a single tiny file read and never touches the
// source backend again.
package migrate

import (
	json "encoding/json/v2"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/d0lim/ccswap/internal/fsutil"
)

const (
	// StateFileName records which migrations have run, inside the backup root.
	StateFileName = ".migrations.json"
	// StateVersion is the state file's schema version.
	StateVersion = 1
)

// Outcome is what a migration reports when it did not fail.
type Outcome int

const (
	// Skipped means the migration was not applicable. Nothing is recorded, so
	// a backup restored later can still trigger it.
	Skipped Outcome = iota
	// Completed means the migration finished. The runner records it, and it
	// never runs again.
	Completed
)

// Account is the minimal view of one managed account a migration needs.
type Account struct {
	Num   string
	Email string
}

// Roster is the account list a migration reads. It is an interface so this
// package stays a leaf: the account store depends on migrations at startup, not
// the other way round.
type Roster interface {
	// Accounts returns the managed accounts and whether the roster could be
	// read at all.
	//
	// The second return separates "no accounts yet" from "the roster exists but
	// is corrupt". A migration must never be recorded as applied in the second
	// case: a user who repairs or restores the roster still needs it to run.
	Accounts() (accounts []Account, readable bool)
}

// Migration is one run-once data migration.
type Migration struct {
	// ID is the key recorded in the state file. It is a permanent identifier:
	// renaming one makes every existing install run it again.
	ID string
	// Run performs the migration.
	Run func(backupRoot string, roster Roster) (Outcome, error)
}

// state is the on-disk shape of .migrations.json.
type state struct {
	Version int               `json:"version"`
	Applied map[string]string `json:"applied"`
}

// StatePath returns where the migration state file lives.
func StatePath(backupRoot string) string {
	return filepath.Join(backupRoot, StateFileName)
}

// loadApplied returns the migration-id to timestamp map.
//
// A missing or unparseable state file is treated as "nothing applied", so a
// corrupt file can never permanently block a migration. The cost of running an
// idempotent migration again is nothing; the cost of never running it is a user
// whose credentials stay in a backend nothing reads.
func loadApplied(backupRoot string) map[string]string {
	text, err := fsutil.ReadText(StatePath(backupRoot))
	if err != nil {
		return map[string]string{}
	}
	var s state
	if err := json.Unmarshal([]byte(text), &s); err != nil || s.Applied == nil {
		return map[string]string{}
	}
	return s.Applied
}

// markApplied records that a migration finished.
//
// A failure here is logged rather than returned: the migration itself already
// succeeded, and failing the whole startup because the bookkeeping could not be
// written would be worse than running an idempotent migration again next time.
func markApplied(backupRoot, id string) {
	applied := loadApplied(backupRoot)
	applied[id] = time.Now().UTC().Format("2006-01-02T15:04:05Z")

	if err := fsutil.WriteJSONAtomic(StatePath(backupRoot), state{
		Version: StateVersion,
		Applied: applied,
	}); err != nil {
		slog.Warn("could not record an applied migration; it will run again next time",
			"migration", id, "error", err)
	}
}

// Run applies every migration that has not been recorded as applied.
//
// It never fails the caller. A migration that errors is logged and left
// unmarked so the next run retries it — startup must not be blocked by a
// compatibility step that can be attempted again.
func Run(backupRoot string, roster Roster, migrations []Migration) {
	applied := loadApplied(backupRoot)

	for _, m := range migrations {
		if _, done := applied[m.ID]; done {
			continue
		}
		outcome, err := m.Run(backupRoot, roster)
		switch {
		case err != nil:
			slog.Warn("migration did not complete; it will be retried on the next run",
				"migration", m.ID, "error", err)
		case outcome == Completed:
			markApplied(backupRoot, m.ID)
		default:
			// Skipped: record nothing, so a backup restored later can still
			// trigger it.
		}
	}
}
