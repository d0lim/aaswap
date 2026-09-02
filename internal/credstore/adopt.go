package credstore

import (
	"fmt"
	"log/slog"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/platform"
)

// AdoptReport says what an adoption did to the Keychain.
type AdoptReport struct {
	// Copied is how many slots now have a backup under aaswap's own service.
	Copied int
	// Missing names slots that had no claude-swap Keychain item at all. Not an
	// error: off macOS there never was one, and on macOS a slot whose backup
	// fell back to a .enc file already came across with the directory.
	Missing []string
	// Failed names slots whose item could not be read or written, with the
	// reason. These need the user's attention.
	Failed []string
}

// AdoptKeychain copies backup items from a predecessor's Keychain service into
// this tool's own, for the slots named.
//
// Copies rather than moves. The directory tree has already been moved by the
// time this runs, so the originals are unreferenced — but leaving them is what
// makes the import reversible: a user who moves the directory back has a
// working claude-swap install again. Cleaning them up is the user's call, and
// the command says so.
//
// Never fails outward on one slot. A partial adoption that reports exactly
// which slots did not come across is more useful than an abort that leaves the
// store half-owned and says nothing about which half.
// The item names on BOTH sides are the pre-provider ones. A predecessor never
// wrote a provider into its names, and what lands here is a version 1 store
// that the upgrade has yet to walk — it will re-register these under their new
// names when it does. Writing the scoped name here would file the item where
// the upgrade does not look.
func (s *Store) AdoptKeychain(fromService string, slots map[string]string) (AdoptReport, error) {
	var report AdoptReport
	if s.platform != platform.MacOS {
		// Only macOS ever put a backup in the Keychain; everywhere else the
		// .enc files are the whole store, and they moved with the directory.
		return report, nil
	}

	unscoped := s.Unscoped()
	for num, email := range slots {
		username := unscoped.backupUsername(num, email)
		value, found, err := s.kc.Get(fromService, username)
		switch {
		case err != nil:
			report.Failed = append(report.Failed, fmt.Sprintf("%s: reading: %v", num, err))
			continue
		case !found || value == "":
			report.Missing = append(report.Missing, num)
			continue
		}
		if err := s.kc.Set(BackupService, username, value); err != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: writing: %v", num, err))
			continue
		}
		// A Keychain item now shadows any .enc that came across with the
		// directory, and reads are .enc-wins — so the stale file has to go, or
		// it would keep answering for the item just written.
		if err := s.reconcileEncAfterKeychainWrite(num, email, value); err != nil {
			report.Failed = append(report.Failed, fmt.Sprintf("%s: reconciling the .enc: %v", num, err))
			continue
		}
		report.Copied++
		slog.Info("adopted a backup credential from claude-swap", "account", num)
	}

	if len(report.Failed) > 0 {
		return report, fmt.Errorf("%w: %d account(s) did not come across",
			apperr.ErrCredential, len(report.Failed))
	}
	return report, nil
}
