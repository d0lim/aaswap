package swap

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/d0lim/ccswap/internal/apperr"
)

// aliasPattern is what an alias may contain once normalized.
var aliasPattern = regexp.MustCompile(`^[a-z0-9_.-]+$`)

// NormalizeAlias lowercases and validates a proposed alias.
//
// Shared by every path that accepts one — the alias command, add's flag, and
// import validation — so the rules cannot differ by entry point.
//
// Three shapes are rejected for reasons that have nothing to do with taste:
// a purely numeric alias would be shadowed by slot-number resolution and could
// never be selected; one starting with a dash would be read as a command flag;
// and anything outside the character class would have to be quoted at every
// call site.
func NormalizeAlias(name string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	switch {
	case normalized == "":
		return "", fmt.Errorf("%w: an alias cannot be empty", apperr.ErrValidation)
	case isDigits(normalized):
		return "", fmt.Errorf("%w: alias %q cannot be purely numeric (that is reserved "+
			"for slot numbers)", apperr.ErrValidation, name)
	case strings.HasPrefix(normalized, "-"):
		return "", fmt.Errorf("%w: alias %q cannot start with '-' (it would be read as "+
			"a command flag)", apperr.ErrValidation, name)
	case !aliasPattern.MatchString(normalized):
		return "", fmt.Errorf("%w: alias %q may only contain letters, digits, '-', '_' "+
			"and '.'", apperr.ErrValidation, name)
	}
	return normalized, nil
}

// SetAlias names a slot, returning the slot and the normalized alias.
func (s *Switcher) SetAlias(identifier, alias string) (num, normalized string, err error) {
	normalized, err = NormalizeAlias(alias)
	if err != nil {
		return "", "", err
	}

	err = s.withLock(func() error {
		roster, rosterErr := s.RosterOrEmpty()
		if rosterErr != nil {
			return rosterErr
		}
		account, slot, resolveErr := s.mustResolve(roster, identifier)
		if resolveErr != nil {
			return resolveErr
		}
		if conflict, inUse := AliasInUse(roster, normalized, slot); inUse {
			return fmt.Errorf("%w: alias %q is already used by account %s",
				apperr.ErrValidation, normalized, conflict)
		}
		num = slot
		account.Alias = normalized
		roster.LastUpdated = Timestamp(s.now())
		return s.WriteRoster(roster)
	})
	if err != nil {
		return "", "", err
	}
	return num, normalized, nil
}

// UnsetAlias removes a slot's alias, reporting whether there was one.
func (s *Switcher) UnsetAlias(identifier string) (num string, had bool, err error) {
	err = s.withLock(func() error {
		roster, rosterErr := s.RosterOrEmpty()
		if rosterErr != nil {
			return rosterErr
		}
		account, slot, resolveErr := s.mustResolve(roster, identifier)
		if resolveErr != nil {
			return resolveErr
		}
		num = slot
		if account.Alias == "" {
			return nil
		}
		had = true
		account.Alias = ""
		roster.LastUpdated = Timestamp(s.now())
		return s.WriteRoster(roster)
	})
	if err != nil {
		return "", false, err
	}
	return num, had, nil
}

// AliasRow is one alias in a listing.
type AliasRow struct {
	Number string
	Alias  string
	Email  string
}

// Aliases lists every named slot, in roster order.
func (s *Switcher) Aliases() ([]AliasRow, error) {
	roster, err := s.RosterOrEmpty()
	if err != nil {
		return nil, err
	}
	var out []AliasRow
	for _, num := range roster.Numbers() {
		account := roster.Accounts[num]
		if account.Alias == "" {
			continue
		}
		out = append(out, AliasRow{Number: num, Alias: account.Alias, Email: account.Email})
	}
	return out, nil
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
