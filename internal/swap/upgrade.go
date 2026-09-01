package swap

import (
	"fmt"

	"github.com/d0lim/aaswap/internal/apperr"
)

// EnsureUpgraded brings a version 1 store up to the current schema, moving the
// stored material with it, and reports how many accounts moved.
//
// # Why the material has to move too
//
// A version 1 store files an account's credential and config under its slot
// number. Version 2 addresses accounts by name. Rewriting the table alone would
// produce a store that names accounts whose material is nowhere the reader
// looks — every switch would fail, and the account would look empty rather than
// misfiled.
//
// # The order is the whole design
//
// The new copies are written FIRST, the table is published SECOND, and the old
// copies are dropped THIRD. A crash before the publish leaves version 1 intact
// beside some orphan copies, and the next run redoes it. A crash after the
// publish leaves version 2 valid beside some orphans, and the next run finds
// nothing to do. There is no ordering where the table names material that is
// not there, which is the one state a person cannot recover from.
//
// Idempotent by construction: a store already at the current schema produces no
// renames, so this reads one file and returns.
func (s *Switcher) EnsureUpgraded() (int, error) {
	// Read outside the lock first. This runs before every command, and taking
	// the store lock on a store that needs nothing would put a lock acquisition
	// in front of `aaswap list`.
	_, found, renames, err := s.readStore()
	if err != nil || !found || len(renames) == 0 {
		return 0, err
	}

	moved := 0
	err = s.withLock(func() error {
		// Re-read under the lock: another process may have upgraded it between
		// the check above and the lock, and doing it twice would move material
		// that has already moved.
		file, found, renames, err := s.readStore()
		if err != nil || !found || len(renames) == 0 {
			return err
		}

		// Read through the pre-provider layout, write through the scoped one.
		// That asymmetry IS the upgrade: the material does not move because a
		// name changed, it moves because where a name is filed changed.
		legacy := s.Creds.Unscoped()
		for _, rename := range renames {
			if err := s.copyStoredFrom(legacy, rename.Number, rename.Name, rename.Email); err != nil {
				return fmt.Errorf("%w: upgrading %s (%s) to the current format failed, "+
					"and nothing was changed: %w", apperr.ErrMigration,
					rename.Number, rename.Email, err)
			}
		}
		if err := writeJSON(s.RosterPath(), file); err != nil {
			return err
		}
		// Published. Everything below is now unreferenced, and a failure to
		// remove it costs disk rather than correctness.
		for _, rename := range renames {
			s.dropStoredFrom(legacy, rename.Number, rename.Email)
		}
		moved = len(renames)
		return nil
	})
	return moved, err
}
