package swap

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/realiti4/claude-swap/internal/apperr"
)

// RemoveOutcome reports what a removal did.
type RemoveOutcome struct {
	Number string
	Email  string
	// WasActive marks removing the account currently logged in. The live login
	// is left alone — removing a slot forgets cswap's copy, it does not log the
	// user out — but they should know.
	WasActive bool
	// Cancelled marks a confirmation the user declined.
	Cancelled bool
}

// RemoveRequest is one removal.
type RemoveRequest struct {
	Identifier string
	AssumeYes  bool
	// Confirm asks before removing. Nil means refuse: permanently discarding a
	// stored login is not something to do on a caller's behalf without asking.
	Confirm func(prompt string) bool
	// ChooseAmbiguous picks among several slots sharing an address. Nil leaves
	// the ambiguity as an error.
	ChooseAmbiguous func(matches []AmbiguousMatch) (string, bool)
}

// AmbiguousMatch is one candidate when an address names several slots.
type AmbiguousMatch struct {
	Number string
	Email  string
	Tag    string
}

// Remove permanently forgets a managed account: its stored credential, its
// stored config, and its roster record.
//
// It does NOT log the user out. The live credential is Claude Code's, and a
// user removing a slot is discarding cswap's copy, not ending their session.
func (s *Switcher) Remove(req RemoveRequest) (RemoveOutcome, error) {
	var outcome RemoveOutcome
	err := s.withLock(func() error {
		roster, err := s.RosterOrEmpty()
		if err != nil {
			return err
		}
		if len(roster.Accounts) == 0 {
			return fmt.Errorf("%w: no accounts are managed yet", apperr.ErrConfig)
		}

		num, err := s.resolveForRemoval(roster, req)
		if err != nil {
			return err
		}
		if num == "" {
			outcome.Cancelled = true
			return nil
		}
		account, ok := roster.Accounts[num]
		if !ok {
			return fmt.Errorf("%w: account %s does not exist", apperr.ErrAccountNotFound, num)
		}

		activeNum, _ := roster.Active()
		outcome = RemoveOutcome{Number: num, Email: account.Email, WasActive: activeNum == num}

		if !req.AssumeYes {
			prompt := fmt.Sprintf("Permanently remove account %s (%s)?", num, account.Email)
			if req.Confirm == nil || !req.Confirm(prompt) {
				outcome = RemoveOutcome{Cancelled: true}
				return nil
			}
		}

		if err := s.deleteAccountFiles(num, account.Email); err != nil {
			return err
		}
		roster.Remove(num, s.now())
		return s.WriteRoster(roster)
	})
	if err != nil {
		return RemoveOutcome{}, err
	}
	return outcome, nil
}

// resolveForRemoval resolves the identifier, offering a choice when an address
// names several slots. An empty number with no error means the user declined.
func (s *Switcher) resolveForRemoval(roster *Roster, req RemoveRequest) (string, error) {
	num, ok, err := s.ResolveIdentifier(roster, req.Identifier)
	if err == nil {
		if !ok {
			return "", fmt.Errorf("%w: no account found with identifier %q",
				apperr.ErrAccountNotFound, req.Identifier)
		}
		return num, nil
	}
	// Ambiguous. An interactive caller can disambiguate; anyone else gets the
	// error, which already names the candidates.
	if req.ChooseAmbiguous == nil {
		return "", err
	}

	var matches []AmbiguousMatch
	for _, candidate := range roster.Numbers() {
		account := roster.Accounts[candidate]
		if account.Email == req.Identifier {
			matches = append(matches, AmbiguousMatch{
				Number: candidate, Email: account.Email, Tag: account.DisplayTag(),
			})
		}
	}
	chosen, picked := req.ChooseAmbiguous(matches)
	if !picked {
		return "", nil
	}
	if _, exists := roster.Accounts[chosen]; !exists {
		return "", fmt.Errorf("%w: account %s does not exist", apperr.ErrAccountNotFound, chosen)
	}
	return chosen, nil
}

// PurgeOutcome reports what a purge removed.
type PurgeOutcome struct {
	Removed   []string
	Cancelled bool
}

// Purge forgets every managed account and removes cswap's whole store.
//
// The live login survives, for the same reason a single removal leaves it: it
// is Claude Code's, not cswap's.
func (s *Switcher) Purge(confirm func(prompt string) bool, assumeYes bool) (PurgeOutcome, error) {
	var outcome PurgeOutcome
	err := s.withLock(func() error {
		roster, err := s.RosterOrEmpty()
		if err != nil {
			return err
		}
		numbers := roster.Numbers()
		if len(numbers) == 0 {
			return nil
		}

		if !assumeYes {
			prompt := fmt.Sprintf("Permanently remove all %d managed accounts?", len(numbers))
			if confirm == nil || !confirm(prompt) {
				outcome.Cancelled = true
				return nil
			}
		}

		for _, num := range numbers {
			account := roster.Accounts[num]
			if err := s.deleteAccountFiles(num, account.Email); err != nil {
				// Keep going: a slot whose file resists deletion must not
				// strand every later slot in a roster that no longer names it.
				slog.Error("could not remove an account's stored material during a purge",
					"account", num, "error", err)
			}
			outcome.Removed = append(outcome.Removed, num)
		}

		empty := newRoster(s.now())
		if err := s.WriteRoster(empty); err != nil {
			return err
		}
		// The usage table is regenerable and now describes accounts that no
		// longer exist.
		if err := os.Remove(s.Usage.Path()); err != nil && !os.IsNotExist(err) {
			slog.Warn("could not remove the usage table during a purge", "error", err)
		}
		return nil
	})
	if err != nil {
		return PurgeOutcome{}, err
	}
	return outcome, nil
}
