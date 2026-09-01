package swap

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/d0lim/aaswap/internal/apperr"
)

// ConfigBackupPath is where a slot's captured ~/.claude.json lives.
func (s *Switcher) ConfigBackupPath(accountNum, email string) string {
	return filepath.Join(s.ConfigsDir(), fmt.Sprintf(".claude-config-%s-%s.json", accountNum, email))
}

// ReadAccountConfig reads an account's captured config, returning empty when
// there is none.
func (s *Switcher) ReadAccountConfig(accountNum, email string) string {
	data, err := os.ReadFile(s.ConfigBackupPath(accountNum, email))
	if err != nil {
		return ""
	}
	return string(data)
}

// readLegacyConfig reads a captured config from the pre-provider layout, for
// the upgrade.
func (s *Switcher) readLegacyConfig(accountNum, email string) string {
	data, err := os.ReadFile(filepath.Join(s.legacyConfigsDir(),
		fmt.Sprintf(".claude-config-%s-%s.json", accountNum, email)))
	if err != nil {
		return ""
	}
	return string(data)
}

// WriteAccountConfig stores a slot's captured config with owner-only
// permissions.
func (s *Switcher) WriteAccountConfig(accountNum, email, config string) error {
	if err := os.MkdirAll(s.ConfigsDir(), 0o700); err != nil {
		return fmt.Errorf("%w: creating the configs directory: %w", apperr.ErrConfig, err)
	}
	path := s.ConfigBackupPath(accountNum, email)
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		return fmt.Errorf("%w: writing %s: %w", apperr.ErrConfig, path, err)
	}
	return nil
}

// IsSwitchable reports whether a slot can be activated without re-adding the
// account: it needs BOTH a stored credential and a stored config.
//
// Both, because activating with one and not the other logs the user in as one
// account while their projects and settings say another. It tolerates a stale
// sequence entry pointing at a record that is gone.
func (s *Switcher) IsSwitchable(roster *Roster, accountNum string) bool {
	account, ok := roster.Accounts[accountNum]
	if !ok {
		return false
	}
	if value, _ := s.Creds.ReadAccount(accountNum, account.Email); value == "" {
		return false
	}
	return s.ReadAccountConfig(accountNum, account.Email) != ""
}

// SwitchableNumbers lists the slots eligible for AUTOMATIC selection, in
// rotation order.
//
// Excludes slots with no usable backup, and slots the user disabled. A disabled
// slot stays managed and remains a valid explicit switch target — it is only
// held out of rotation and the usage-aware strategies — so parking an account
// never costs its stored login.
func (s *Switcher) SwitchableNumbers(roster *Roster) []string {
	var out []string
	for _, num := range roster.Names() {
		if roster.Accounts[num].Disabled {
			continue
		}
		if s.IsSwitchable(roster, num) {
			out = append(out, num)
		}
	}
	return out
}

// DisabledNumbers lists the slots the user has held out of rotation.
func (s *Switcher) DisabledNumbers(roster *Roster) []string {
	var out []string
	for _, num := range roster.Names() {
		if roster.Accounts[num].Disabled {
			out = append(out, num)
		}
	}
	return out
}

// SetDisabled holds an account out of automatic selection, or returns it.
//
// Reports whether anything changed, so a caller can say "already disabled"
// rather than claiming an edit it did not make.
func (s *Switcher) SetDisabled(identifier string, disabled bool) (num, email string, changed bool, err error) {
	err = s.withLock(func() error {
		roster, rosterErr := s.RosterOrEmpty()
		if rosterErr != nil {
			return rosterErr
		}
		account, slot, resolveErr := s.mustResolve(roster, identifier)
		if resolveErr != nil {
			return resolveErr
		}
		num, email = slot, account.Email
		if account.Disabled == disabled {
			return nil
		}
		changed = true
		account.Disabled = disabled
		return s.WriteRoster(roster)
	})
	if err != nil {
		return "", "", false, err
	}
	return num, email, changed, nil
}

// CurrentNumber is the slot holding the live login, reporting false when there
// is none or the live login is unmanaged.
//
// Deliberately NO fallback to the roster's recorded active slot. An unmanaged
// live login must report nothing rather than a guessed slot, or the auto-switch
// engine would evaluate the wrong account's usage and overwrite a login aaswap
// does not own. Use [Switcher.HasLiveLogin] to tell the two negative cases
// apart.
func (s *Switcher) CurrentNumber(roster *Roster) (string, bool) {
	live, ok := s.LiveIdentity()
	if !ok {
		return "", false
	}
	return roster.FindName(live.Identity())
}

// HasLiveLogin reports whether the machine carries any live account identity.
func (s *Switcher) HasLiveLogin() bool {
	_, ok := s.LiveIdentity()
	return ok
}
