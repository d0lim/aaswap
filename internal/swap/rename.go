package swap

import (
	"fmt"

	"github.com/d0lim/aaswap/internal/apperr"
)

// Rename gives an account a new handle.
//
// The name is not a label on the account — it IS where the account's credential
// and config are filed. So a rename moves bytes, and the order matters: the new
// copies are written before the roster names them, and the old ones are dropped
// only once the roster has been published. A crash anywhere in between leaves a
// readable store, which is the property worth paying two copies for.
func (s *Switcher) Rename(identifier, to string) (from, normalized string, err error) {
	normalized, err = NormalizeName(to)
	if err != nil {
		return "", "", err
	}

	err = s.withLock(func() error {
		roster, rosterErr := s.RosterOrEmpty()
		if rosterErr != nil {
			return rosterErr
		}
		account, current, resolveErr := s.mustResolve(roster, identifier)
		if resolveErr != nil {
			return resolveErr
		}
		from = current
		if current == normalized {
			return nil // already called that; nothing to move
		}
		if occupant, taken := roster.Accounts[normalized]; taken {
			return fmt.Errorf("%w: %q is already %s", apperr.ErrValidation,
				normalized, occupant.Email)
		}

		if moveErr := s.copyStored(current, normalized, account.Email); moveErr != nil {
			return moveErr
		}
		roster.Rename(current, normalized)
		if writeErr := s.WriteRoster(roster); writeErr != nil {
			return writeErr
		}
		// Published. The old copies are now unreferenced, and a failure to
		// remove them costs disk rather than correctness.
		s.dropStored(current, account.Email)
		return nil
	})
	return from, normalized, err
}

// mustResolve resolves an identifier to an existing slot, or explains why not.
func (s *Switcher) mustResolve(roster *Roster, identifier string) (*Account, string, error) {
	if len(roster.Accounts) == 0 {
		return nil, "", fmt.Errorf("%w: no accounts are managed yet", apperr.ErrConfig)
	}
	num, ok, err := s.ResolveIdentifier(roster, identifier)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "", fmt.Errorf("%w: %q does not match any managed account",
			apperr.ErrAccountNotFound, identifier)
	}
	account, exists := roster.Accounts[num]
	if !exists {
		return nil, "", fmt.Errorf("%w: account %s does not exist",
			apperr.ErrAccountNotFound, num)
	}
	return account, num, nil
}

// ResolveAccount resolves an identifier to a slot and its record.
func (s *Switcher) ResolveAccount(identifier string) (string, *Account, error) {
	roster, err := s.RosterOrEmpty()
	if err != nil {
		return "", nil, err
	}
	account, num, err := s.mustResolve(roster, identifier)
	if err != nil {
		return "", nil, err
	}
	return num, account, nil
}
