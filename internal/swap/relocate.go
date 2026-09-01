package swap

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/fsutil"
)

// SwapSlots exchanges two accounts' slot numbers.
//
// Everything keyed by the slot number moves: the roster records (aliases
// included — an alias belongs to the account, not the number), the per-slot
// credential and config backups, membership in the ordering, and the active
// pointer. Directory mappings key on the account identity and are unaffected.
// Usage rows key on the slot number but carry the account identity, so a
// swapped row fails its identity check and self-heals on the next poll.
//
// The whole resolve-validate-mutate span runs under the store lock. A slot
// number resolved outside it could be renumbered by a concurrent relocation and
// target the wrong account.
//
// The roster write is the commit point: a failure before it rolls both slots
// back, and after it only best-effort cleanup of the stale keys remains.
func (s *Switcher) SwapSlots(first, second string) (numA, numB string, err error) {
	err = s.withLock(func() error {
		roster, rosterErr := s.RosterOrEmpty()
		if rosterErr != nil {
			return rosterErr
		}
		numA, numB, rosterErr = s.swapLocked(roster, first, second)
		return rosterErr
	})
	if err != nil {
		return "", "", err
	}
	return numA, numB, nil
}

func (s *Switcher) swapLocked(roster *Roster, first, second string) (string, string, error) {
	if len(roster.Accounts) == 0 {
		return "", "", fmt.Errorf("%w: no accounts are managed yet", apperr.ErrConfig)
	}
	accountA, numA, err := s.mustResolve(roster, first)
	if err != nil {
		return "", "", err
	}
	accountB, numB, err := s.mustResolve(roster, second)
	if err != nil {
		return "", "", err
	}
	if numA == numB {
		return "", "", fmt.Errorf("%w: cannot swap an account with itself", apperr.ErrValidation)
	}

	emailA, emailB := accountA.Email, accountB.Email

	// Read both slots' material up front, so a read failure aborts before
	// anything has moved. Missing material reads as absent and stays absent
	// after the swap — but an UNREADABLE backup is not missing, and committing
	// a swap on that reading would silently drop a slot's live refresh token in
	// favor of an empty destination.
	credsA, err := s.readBackupOrAbort(numA, emailA)
	if err != nil {
		return "", "", err
	}
	credsB, err := s.readBackupOrAbort(numB, emailB)
	if err != nil {
		return "", "", err
	}
	configA := s.ReadAccountConfig(numA, emailA)
	configB := s.ReadAccountConfig(numB, emailB)

	var staging map[string]string
	if emailA == emailB {
		// Same address: the two slots' backup keys fully overlap, so every
		// write below overwrites the other account's material. Park durable
		// copies first, so a failure mid-write can never leave a credential
		// existing only in this process's memory.
		staging, err = s.stageOverlap(map[string][2]string{
			numA: {credsA, configA},
			numB: {credsB, configB},
		})
		if err != nil {
			return "", "", err
		}
	}
	defer s.discardStaging(staging)

	rollback := func() {
		s.restoreSlot(numA, emailA, credsA, configA)
		s.restoreSlot(numB, emailB, credsB, configB)
	}

	// Set each destination key to its owner's exact state: write material that
	// exists, actively CLEAR what does not. An empty source must never leave
	// the destination serving leftover material — the other account's, where
	// the addresses overlap and no separate cleanup runs, or a stale file
	// leaked by an earlier crash.
	if err := s.writeOrClearSlot(numB, emailA, credsA, configA); err != nil {
		rollback()
		return "", "", err
	}
	if err := s.writeOrClearSlot(numA, emailB, credsB, configB); err != nil {
		rollback()
		return "", "", err
	}

	roster.Accounts[numA], roster.Accounts[numB] = accountB, accountA
	intA, intB := mustAtoi(numA), mustAtoi(numB)
	for i, n := range roster.Sequence {
		switch n {
		case intA:
			roster.Sequence[i] = intB
		case intB:
			roster.Sequence[i] = intA
		}
	}
	// Sorted, so rotation and list order follow the new numbers rather than
	// preserving the old visual positions.
	slices.Sort(roster.Sequence)
	if active := roster.ActiveAccountNumber; active != nil {
		switch *active {
		case intA:
			roster.SetActive(numB, s.now())
		case intB:
			roster.SetActive(numA, s.now())
		}
	}
	roster.LastUpdated = Timestamp(s.now())

	if err := s.WriteRoster(roster); err != nil {
		rollback()
		return "", "", err
	}

	// Post-commit, all best effort: the roster already references the new keys
	// only, so a failure here leaks a stale file, never a wrong read. Logged
	// loudly, because a stale key under a freed slot would poison a future
	// same-address account landing on that number.
	if emailA != emailB {
		for _, pair := range [][2]string{{numA, emailA}, {numB, emailB}} {
			if err := s.deleteAccountFiles(pair[0], pair[1]); err != nil {
				slog.Error("a stale backup was left under an old slot key",
					"account", pair[0], "error", err)
			}
		}
	}
	// The retained previous generations hold the DISPLACED material — another
	// account's credential, or a stale one — which recovery must never
	// resurrect onto the key's new owner.
	if credsA != "" {
		s.Creds.DeletePreviousBackup(numB, emailA)
	}
	if credsB != "" {
		s.Creds.DeletePreviousBackup(numA, emailB)
	}
	return numA, numB, nil
}

// MoveAccount moves one account to an empty slot.
//
// The one-way counterpart of [Switcher.SwapSlots]. When the target is occupied
// this becomes a swap, because that is what the user meant.
func (s *Switcher) MoveAccount(identifier, target string) (from, to string, swapped bool, err error) {
	if !isDigits(target) {
		return "", "", false, fmt.Errorf("%w: a move target must be a slot number, not %q",
			apperr.ErrValidation, target)
	}
	if mustAtoi(target) < 1 {
		return "", "", false, fmt.Errorf("%w: a slot number must be 1 or greater", apperr.ErrValidation)
	}

	err = s.withLock(func() error {
		roster, rosterErr := s.RosterOrEmpty()
		if rosterErr != nil {
			return rosterErr
		}
		if len(roster.Accounts) == 0 {
			return fmt.Errorf("%w: no accounts are managed yet", apperr.ErrConfig)
		}
		_, source, resolveErr := s.mustResolve(roster, identifier)
		if resolveErr != nil {
			return resolveErr
		}
		if source == target {
			return fmt.Errorf("%w: account %s is already in slot %s",
				apperr.ErrValidation, source, target)
		}

		if _, occupied := roster.Accounts[target]; occupied {
			// Resolved and dispatched inside ONE lock acquisition: a number
			// resolved outside could be renumbered by a concurrent relocation.
			swapped = true
			var swapErr error
			from, to, swapErr = s.swapLocked(roster, source, target)
			return swapErr
		}
		from, to = source, target
		return s.relocateLocked(roster, source, target)
	})
	if err != nil {
		return "", "", false, err
	}
	return from, to, swapped, nil
}

// relocateLocked moves one account to an empty slot.
//
// No rollback is needed: the roster write is the commit point. Before it the
// old keys are untouched, and any stray written under the target key is cleaned
// up on failure; after it only best-effort cleanup of the old keys remains.
func (s *Switcher) relocateLocked(roster *Roster, source, target string) error {
	account, ok := roster.Accounts[source]
	if !ok {
		return fmt.Errorf("%w: account %s does not exist", apperr.ErrAccountNotFound, source)
	}
	if _, occupied := roster.Accounts[target]; occupied {
		return fmt.Errorf("%w: slot %s is already occupied — retry the move",
			apperr.ErrValidation, target)
	}
	email := account.Email

	credentials, err := s.readBackupOrAbort(source, email)
	if err != nil {
		return err
	}
	config := s.ReadAccountConfig(source, email)

	cleanup := func() {
		if err := s.deleteAccountFiles(target, email); err != nil {
			slog.Error("cleanup after a failed move was incomplete", "slot", target, "error", err)
		}
	}

	if err := s.writeOrClearSlot(target, email, credentials, config); err != nil {
		cleanup()
		return err
	}

	roster.Accounts[target] = account
	delete(roster.Accounts, source)
	intSource, intTarget := mustAtoi(source), mustAtoi(target)
	for i, n := range roster.Sequence {
		if n == intSource {
			roster.Sequence[i] = intTarget
		}
	}
	slices.Sort(roster.Sequence)
	if roster.ActiveAccountNumber != nil && *roster.ActiveAccountNumber == intSource {
		roster.SetActive(target, s.now())
	}
	roster.LastUpdated = Timestamp(s.now())

	if err := s.WriteRoster(roster); err != nil {
		cleanup()
		return err
	}

	// Post-commit: clear the old key, best effort. A failure leaks a stale
	// backup under the freed number, which would poison a future same-address
	// account landing there.
	if err := s.deleteAccountFiles(source, email); err != nil {
		slog.Error("a stale backup was left under the old slot key",
			"account", source, "error", err)
	}
	if credentials != "" {
		// Any generation retained while overwriting a stale target key holds
		// that stale material, not this account's history.
		s.Creds.DeletePreviousBackup(target, email)
	}
	return nil
}

// readBackupOrAbort reads a slot's stored credential, refusing on an UNREADABLE
// one.
//
// Absent is a real state — an API-key slot, or one never backed up — and stays
// absent through a relocation. Unreadable is not absent, and nothing has moved
// yet at these call sites, so it aborts rather than committing a relocation
// that drops the slot's live refresh token in favor of an empty destination.
func (s *Switcher) readBackupOrAbort(accountNum, email string) (string, error) {
	credentials, unreadable := s.Creds.ReadAccount(accountNum, email)
	if unreadable {
		return "", fmt.Errorf("%w: account %s's stored credential could not be read "+
			"(an unavailable Keychain?); nothing was changed. Retry once it is readable "+
			"again", apperr.ErrConfig, accountNum)
	}
	return credentials, nil
}

// writeOrClearSlot sets a slot key to exactly the given state.
func (s *Switcher) writeOrClearSlot(accountNum, email, credentials, config string) error {
	if credentials != "" {
		if err := s.Creds.WriteAccount(accountNum, email, credentials); err != nil {
			return err
		}
		s.BackupWritten(accountNum, email)
	} else if err := s.Creds.DeleteAccount(accountNum, email); err != nil {
		return err
	}

	if config != "" {
		return s.WriteAccountConfig(accountNum, email, config)
	}
	if err := os.Remove(s.ConfigBackupPath(accountNum, email)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w: clearing the stored config for account %s: %w",
			apperr.ErrConfig, accountNum, err)
	}
	return nil
}

// restoreSlot puts a slot key back to a remembered state, best effort.
func (s *Switcher) restoreSlot(accountNum, email, credentials, config string) {
	if err := s.writeOrClearSlot(accountNum, email, credentials, config); err != nil {
		slog.Error("rollback of a failed relocation was incomplete",
			"account", accountNum, "error", err)
	}
}

// stageOverlap parks durable copies of material whose slot keys overlap.
//
// When two slots share an address their backup keys are identical, so writing
// one destroys the other. Staging first means a failure mid-write can never
// leave a credential existing only in this process's memory.
func (s *Switcher) stageOverlap(material map[string][2]string) (map[string]string, error) {
	staged := map[string]string{}
	dir := filepath.Join(s.BackupRoot(), "staging")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: creating the staging directory: %w", apperr.ErrConfig, err)
	}
	for num, pair := range material {
		for i, value := range pair {
			if value == "" {
				continue
			}
			kind := "creds"
			if i == 1 {
				kind = "config"
			}
			path := filepath.Join(dir, fmt.Sprintf("%s-%s-%d", kind, num, s.now().UnixNano()))
			if err := fsutil.WriteFileAtomic(path, []byte(value)); err != nil {
				s.discardStaging(staged)
				return nil, fmt.Errorf("%w: staging account %s's material before a swap: %w",
					apperr.ErrConfig, num, err)
			}
			staged[kind+":"+num] = path
		}
	}
	return staged, nil
}

// discardStaging removes the parked copies once they are no longer needed.
func (s *Switcher) discardStaging(staged map[string]string) {
	for _, path := range staged {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			slog.Warn("could not remove a staged copy after a relocation",
				"path", path, "error", err)
		}
	}
}

func mustAtoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
