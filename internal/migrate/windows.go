package migrate

import (
	"fmt"
	"os"
	"strings"

	"github.com/realiti4/claude-swap/internal/platform"
)

// WindowsKeyringMigrationID is the identifier the Python implementation
// recorded for its Windows keyring-to-files migration.
//
// The Go notice below reuses it deliberately: an install where the Python
// version already ran that migration has this key in .migrations.json, and the
// runner then skips the notice entirely. Only a user who jumped straight from a
// pre-0.11 Python install to the Go binary — and so never had the migration run
// — can still see it.
const WindowsKeyringMigrationID = "windows_keyring_to_files"

// BackupProbe reports whether a slot's backup can be read. It is satisfied by
// the credential store's own account read.
type BackupProbe interface {
	// ReadAccount returns a slot's backup and whether the read FAILED, as
	// opposed to finding nothing. The distinction matters here: an unreadable
	// backup is a transient problem, while a genuinely absent one on Windows is
	// what the legacy keyring would explain.
	ReadAccount(accountNum, email string) (value string, unreadable bool)
}

// WindowsKeyringNotice tells a Windows user whose backups predate ccswap 0.11
// how to recover them.
//
// # Why this is a notice and not a migration
//
// Before 0.11 the Windows backend was the Windows Credential Manager, reached
// through Python's keyring library; ccswap then moved to base64 .enc files
// because the Credential Manager rejects entries over roughly 2,500 bytes
// (#45). The Python implementation carried a one-time migration that relocated
// those entries.
//
// Go has no keyring library, and golang.org/x/sys/windows does not expose
// CredRead, so reproducing that migration would mean hand-binding
// advapi32!CredReadW and matching Python keyring's exact target-name and
// UTF-16LE blob conventions — unverifiable except on a Windows host, for a
// population that is only those users who skipped every Python release from
// 0.11 onward. Getting that binding subtly wrong would read a credential as
// garbage and write the garbage over a working slot.
//
// So the Go build does not migrate; it explains. A user in that position runs
// the Python ccswap once, which performs the real migration, and then returns to
// the Go binary with everything in .enc files where both implementations read
// it.
//
// The notice reports Completed — and so is recorded and never shown again — only
// once every account has a readable backup. While any account is still missing
// one, it stays unrecorded so the advice keeps appearing until it is acted on.
func WindowsKeyringNotice(backups BackupProbe, p platform.Platform) Migration {
	return Migration{
		ID: WindowsKeyringMigrationID,
		Run: func(_ string, roster Roster) (Outcome, error) {
			return runWindowsKeyringNotice(backups, p, roster)
		},
	}
}

func runWindowsKeyringNotice(backups BackupProbe, p platform.Platform, roster Roster) (Outcome, error) {
	if p != platform.Windows {
		return Skipped, nil
	}
	accounts, readable := roster.Accounts()
	if !readable {
		return Skipped, nil
	}
	if len(accounts) == 0 {
		// Nothing managed yet, so nothing can be stranded. Recording it here
		// would be wrong: accounts restored from a pre-0.11 backup later still
		// deserve the advice.
		return Skipped, nil
	}

	var stranded []string
	for _, account := range accounts {
		value, unreadable := backups.ReadAccount(account.Num, account.Email)
		if value == "" && !unreadable {
			// Genuinely absent, not merely unreachable. An unreadable backup is
			// a transient problem with its own message elsewhere, and advising
			// a re-add for it would send the user to redo work that is fine.
			stranded = append(stranded, fmt.Sprintf("%s (%s)", account.Num, account.Email))
		}
	}
	if len(stranded) == 0 {
		return Completed, nil
	}

	fmt.Fprintf(os.Stderr,
		"claude-swap: %d account(s) have no stored credentials on this machine: %s\n"+
			"  If these accounts were added with claude-swap 0.10 or earlier, their\n"+
			"  credentials are still in the Windows Credential Manager, which no\n"+
			"  current build reads. Log in with each account and re-add it:\n"+
			"      ccswap add --slot N\n"+
			"  The Credential Manager entries are left untouched, so nothing is lost\n"+
			"  by doing this.\n",
		len(stranded), strings.Join(stranded, ", "))

	// Deliberately unrecorded: the advice must keep appearing until the
	// accounts actually have backups.
	return Skipped, nil
}
