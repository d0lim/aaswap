package migrate

import (
	"fmt"
	"log/slog"
	"os"

	"github.com/d0lim/ccswap/internal/apperr"
	"github.com/d0lim/ccswap/internal/platform"
)

// LegacyKeyringService is the service name the third-party keyring library used
// for per-account backups before ccswap moved to security(1) directly.
const LegacyKeyringService = "claude-code"

// LegacyReader reads items out of the legacy keyring service.
type LegacyReader interface {
	// Get returns the stored value and whether an item exists. An error means
	// the Keychain could not answer at all.
	Get(service, account string) (string, bool, error)
}

// KeychainBackups is the Keychain-only view of the backup store this migration
// needs.
//
// Deliberately Keychain-specific: the migration's job is the security service,
// so a fallback .enc must not be mistaken for "already migrated", and the write
// must not be diverted away from the service being populated.
type KeychainBackups interface {
	ReadKeychainBackup(accountNum, email string) (string, error)
	WriteKeychainBackup(accountNum, email, credentials string) error
	DeleteKeychainBackup(accountNum, email string)
}

// MacOSKeyringToSecurity moves macOS backup credentials from the legacy keyring
// service to the current security(1) service.
//
// Source and destination are *different* services, so the old and new items
// coexist through a safe write-verify-delete with no risk window.
//
// # Reading the legacy items
//
// The Python original preferred the third-party keyring library for these
// reads, because a same-app read is silent, and fell back to security(1) only
// when that library was unavailable — a branch it documented as dormant, kept
// "so a future keyring removal can't strand a long-absent user". This is that
// future: Go has no keyring library, so the fallback is the only path.
//
// That fallback also decides what happens to the source item. The original
// deliberately LEAVES the legacy item in place when reading through security(1),
// because deleting an item another app created can raise a second Keychain
// prompt, and the data is by then already safely in the new service. The orphan
// is harmless cruft that `ccswap purge` mops up. This implementation keeps that
// choice, so the migration costs the user at most one prompt.
func MacOSKeyringToSecurity(legacy LegacyReader, backups KeychainBackups, p platform.Platform) Migration {
	return Migration{
		ID: "macos_keyring_to_security",
		Run: func(_ string, roster Roster) (Outcome, error) {
			return runMacOSKeyringToSecurity(legacy, backups, p, roster)
		},
	}
}

func runMacOSKeyringToSecurity(
	legacy LegacyReader,
	backups KeychainBackups,
	p platform.Platform,
	roster Roster,
) (Outcome, error) {
	if p != platform.MacOS {
		return Skipped, nil
	}
	accounts, readable := roster.Accounts()
	if !readable {
		// No managed accounts yet, or a roster that exists but cannot be
		// parsed. Never record it: a user who repairs or restores the roster
		// must still get the migration.
		return Skipped, nil
	}
	if len(accounts) == 0 {
		return Completed, nil // readable roster, nothing to migrate
	}

	// Pre-check: anything already in the new service is done. New installs and
	// already-migrated users have every account here, so they never touch the
	// legacy service at all. On a retry this also narrows the work to the
	// accounts that actually failed last time.
	//
	// A Keychain that cannot answer here is NOT "nothing to migrate": defer and
	// retry rather than skipping real entries.
	var pending []Account
	for _, account := range accounts {
		existing, err := backups.ReadKeychainBackup(account.Num, account.Email)
		if err != nil {
			return Skipped, fmt.Errorf(
				"keychain unavailable, deferring the macOS keyring migration: %w: %w",
				apperr.ErrMigrationIncomplete, err)
		}
		if existing == "" {
			pending = append(pending, account)
		}
	}
	if len(pending) == 0 {
		return Completed, nil
	}

	// The legacy account-None-{email} spelling maps to a slot only when its
	// email is unique; with duplicates there is no way to tell which slot it
	// belonged to, so it is left alone.
	emailCounts := map[string]int{}
	for _, account := range accounts {
		emailCounts[account.Email]++
	}

	migrated, failed := 0, 0
	for _, account := range pending {
		canonical := fmt.Sprintf("account-%s-%s", account.Num, account.Email)
		noneUser := fmt.Sprintf("account-None-%s", account.Email)

		creds, err := readLegacy(legacy, canonical)
		if err != nil {
			slog.Warn("macos_keyring_to_security: legacy read failed",
				"item", canonical, "error", err)
			failed++
			continue
		}
		if creds == "" && account.Num != "None" && emailCounts[account.Email] == 1 {
			creds, err = readLegacy(legacy, noneUser)
			if err != nil {
				slog.Warn("macos_keyring_to_security: legacy read failed",
					"item", noneUser, "error", err)
				failed++
				continue
			}
		}
		if creds == "" {
			// Nothing in the legacy service for this slot — added on a newer
			// version, or an ambiguous account-None this migration will not
			// touch. Benign, not a failure.
			continue
		}

		// Write, then verify, before considering the slot migrated.
		if err := backups.WriteKeychainBackup(account.Num, account.Email, creds); err != nil {
			slog.Warn("macos_keyring_to_security: write failed",
				"item", canonical, "error", err)
			// A partial or garbage item must not shadow the still-intact legacy
			// entry. Drop it; the retry rewrites it.
			backups.DeleteKeychainBackup(account.Num, account.Email)
			failed++
			continue
		}
		readback, err := backups.ReadKeychainBackup(account.Num, account.Email)
		if err != nil || readback != creds {
			slog.Warn("macos_keyring_to_security: read-back mismatch; discarding the "+
				"new item and leaving the legacy entry in place",
				"item", canonical, "error", err)
			backups.DeleteKeychainBackup(account.Num, account.Email)
			failed++
			continue
		}

		// The legacy source is deliberately left in place — see the doc comment.
		migrated++
	}

	if migrated > 0 {
		fmt.Fprintf(os.Stderr,
			"ccswap: migrated %d macOS credential(s) from the legacy keyring "+
				"into the Keychain\n", migrated)
	}
	if failed > 0 {
		return Skipped, fmt.Errorf(
			"%d account(s) could not be migrated to the security service; "+
				"will retry on the next run: %w", failed, apperr.ErrMigrationIncomplete)
	}
	return Completed, nil
}

// readLegacy returns a legacy item's value, or "" when it is absent. An error
// means the Keychain could not answer, which is a real failure rather than a
// miss.
func readLegacy(legacy LegacyReader, account string) (string, error) {
	value, _, err := legacy.Get(LegacyKeyringService, account)
	if err != nil {
		return "", err
	}
	return value, nil
}
