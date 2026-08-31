package migrate

import (
	json "encoding/json/v2"
	"errors"
	"os"
	"testing"

	"github.com/realiti4/claude-swap/internal/apperr"
	"github.com/realiti4/claude-swap/internal/platform"
)

// ---------------------------------------------------------------- Fakes

type fakeRoster struct {
	accounts []Account
	readable bool
}

func (r fakeRoster) Accounts() ([]Account, bool) { return r.accounts, r.readable }

// fakeLegacy is the legacy keyring service as security(1) would see it.
type fakeLegacy struct {
	items map[string]string
	err   error
}

func (f fakeLegacy) Get(service, account string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	value, ok := f.items[service+"\x00"+account]
	return value, ok, nil
}

// fakeBackups is the current security service.
type fakeBackups struct {
	items    map[string]string
	readErr  error
	writeErr error
	// garbleOne is the account num whose write lands garbled, so the read-back
	// afterwards does not match. Simulated in the WRITE, not the read: doing it
	// in the read would also poison the pre-check and make the account look
	// already-migrated, so the migration would never reach the write at all.
	garbleOne string
	deletes   []string
}

func newFakeBackups() *fakeBackups {
	return &fakeBackups{items: map[string]string{}}
}

func (f *fakeBackups) key(num, email string) string { return num + "\x00" + email }

func (f *fakeBackups) ReadKeychainBackup(num, email string) (string, error) {
	if f.readErr != nil {
		return "", f.readErr
	}
	return f.items[f.key(num, email)], nil
}

func (f *fakeBackups) WriteKeychainBackup(num, email, credentials string) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if num == f.garbleOne {
		f.items[f.key(num, email)] = "GARBLED"
		return nil
	}
	f.items[f.key(num, email)] = credentials
	return nil
}

func (f *fakeBackups) DeleteKeychainBackup(num, email string) {
	f.deletes = append(f.deletes, f.key(num, email))
	delete(f.items, f.key(num, email))
}

// ---------------------------------------------------------------- State file

func TestLoadAppliedTreatsAnUnusableStateFileAsEmpty(t *testing.T) {
	// A corrupt state file must never permanently block a migration: running an
	// idempotent migration again costs nothing, while never running it leaves
	// credentials in a backend nothing reads.
	tests := []struct {
		name    string
		content string
		write   bool
	}{
		{name: "missing", write: false},
		{name: "corrupt", content: "{not json", write: true},
		{name: "not an object", content: `["a"]`, write: true},
		{name: "no applied key", content: `{"version":1}`, write: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			if tt.write {
				if err := os.WriteFile(StatePath(root), []byte(tt.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if got := loadApplied(root); len(got) != 0 {
				t.Errorf("loadApplied = %v, want empty", got)
			}
		})
	}
}

func TestMarkAppliedRoundTrips(t *testing.T) {
	root := t.TempDir()
	markApplied(root, "first")
	markApplied(root, "second")

	applied := loadApplied(root)
	for _, id := range []string{"first", "second"} {
		if _, ok := applied[id]; !ok {
			t.Errorf("%s was not recorded", id)
		}
	}

	b, err := os.ReadFile(StatePath(root))
	if err != nil {
		t.Fatal(err)
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.Version != StateVersion {
		t.Errorf("version = %d, want %d", s.Version, StateVersion)
	}
}

// ---------------------------------------------------------------- Runner

func TestRunRecordsOnlyCompletedMigrations(t *testing.T) {
	tests := []struct {
		name       string
		outcome    Outcome
		err        error
		wantRecord bool
	}{
		{"completed is recorded", Completed, nil, true},
		// Skipped records nothing, so a backup restored later can still
		// trigger it.
		{"skipped is not recorded", Skipped, nil, false},
		// A partial failure is left unmarked so the next run retries.
		{"a failure is not recorded", Skipped, errors.New("partial"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			Run(root, fakeRoster{readable: true}, []Migration{{
				ID: "m",
				Run: func(string, Roster) (Outcome, error) {
					return tt.outcome, tt.err
				},
			}})

			_, recorded := loadApplied(root)["m"]
			if recorded != tt.wantRecord {
				t.Errorf("recorded = %v, want %v", recorded, tt.wantRecord)
			}
		})
	}
}

// After the state file records a migration it must short-circuit and never
// touch the source backend again.
func TestRunSkipsAlreadyAppliedMigrations(t *testing.T) {
	root := t.TempDir()
	markApplied(root, "m")

	ran := false
	Run(root, fakeRoster{readable: true}, []Migration{{
		ID: "m",
		Run: func(string, Roster) (Outcome, error) {
			ran = true
			return Completed, nil
		},
	}})
	if ran {
		t.Error("an already-applied migration ran again")
	}
}

// Startup must not be blocked by a compatibility step that can be retried.
func TestRunNeverPropagatesAFailure(t *testing.T) {
	root := t.TempDir()
	second := false
	Run(root, fakeRoster{readable: true}, []Migration{
		{ID: "first", Run: func(string, Roster) (Outcome, error) {
			return Skipped, errors.New("boom")
		}},
		{ID: "second", Run: func(string, Roster) (Outcome, error) {
			second = true
			return Completed, nil
		}},
	})
	if !second {
		t.Error("a failing migration stopped the ones after it")
	}
}

// ---------------------------------------------------------------- macOS migration

func runMacOS(t *testing.T, legacy fakeLegacy, backups *fakeBackups, p platform.Platform, roster Roster) (Outcome, error) {
	t.Helper()
	m := MacOSKeyringToSecurity(legacy, backups, p)
	return m.Run(t.TempDir(), roster)
}

func TestMacOSMigrationSkipsWhenNotApplicable(t *testing.T) {
	tests := []struct {
		name     string
		platform platform.Platform
		roster   fakeRoster
	}{
		{"off macOS", platform.Linux, fakeRoster{accounts: []Account{{Num: "1", Email: "a@x.com"}}, readable: true}},
		// A roster that exists but cannot be parsed must never be recorded:
		// a user who repairs it still needs the migration.
		{"an unreadable roster", platform.MacOS, fakeRoster{readable: false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome, err := runMacOS(t, fakeLegacy{}, newFakeBackups(), tt.platform, tt.roster)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if outcome != Skipped {
				t.Errorf("outcome = %v, want Skipped", outcome)
			}
		})
	}
}

func TestMacOSMigrationCompletesWithNothingToDo(t *testing.T) {
	t.Run("a readable but empty roster", func(t *testing.T) {
		outcome, err := runMacOS(t, fakeLegacy{}, newFakeBackups(), platform.MacOS, fakeRoster{readable: true})
		if err != nil || outcome != Completed {
			t.Errorf("outcome = %v, err = %v, want Completed", outcome, err)
		}
	})

	// New installs and already-migrated users have every account in the new
	// service, so they never touch the legacy one at all.
	t.Run("every account already in the new service", func(t *testing.T) {
		backups := newFakeBackups()
		backups.items[backups.key("1", "a@x.com")] = "already-there"
		legacy := fakeLegacy{items: map[string]string{
			LegacyKeyringService + "\x00account-1-a@x.com": "legacy",
		}}

		outcome, err := runMacOS(t, legacy, backups, platform.MacOS,
			fakeRoster{accounts: []Account{{Num: "1", Email: "a@x.com"}}, readable: true})
		if err != nil || outcome != Completed {
			t.Errorf("outcome = %v, err = %v, want Completed", outcome, err)
		}
		if backups.items[backups.key("1", "a@x.com")] != "already-there" {
			t.Error("an already-migrated account was overwritten from the legacy service")
		}
	})
}

func TestMacOSMigrationRelocatesCredentials(t *testing.T) {
	backups := newFakeBackups()
	legacy := fakeLegacy{items: map[string]string{
		LegacyKeyringService + "\x00account-1-a@x.com": "creds-one",
		LegacyKeyringService + "\x00account-2-b@x.com": "creds-two",
	}}

	outcome, err := runMacOS(t, legacy, backups, platform.MacOS, fakeRoster{
		accounts: []Account{{Num: "1", Email: "a@x.com"}, {Num: "2", Email: "b@x.com"}},
		readable: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != Completed {
		t.Errorf("outcome = %v, want Completed", outcome)
	}
	if got := backups.items[backups.key("1", "a@x.com")]; got != "creds-one" {
		t.Errorf("account 1 = %q, want the relocated credential", got)
	}
	if got := backups.items[backups.key("2", "b@x.com")]; got != "creds-two" {
		t.Errorf("account 2 = %q, want the relocated credential", got)
	}
}

// Reading the legacy items through security(1) means deleting them could raise
// a second Keychain prompt, since another app created them. The data is already
// safely in the new service by then, so the orphan is left as harmless cruft
// that `ccswap purge` mops up.
func TestMacOSMigrationLeavesTheLegacyItemInPlace(t *testing.T) {
	backups := newFakeBackups()
	legacy := fakeLegacy{items: map[string]string{
		LegacyKeyringService + "\x00account-1-a@x.com": "creds",
	}}

	if _, err := runMacOS(t, legacy, backups, platform.MacOS,
		fakeRoster{accounts: []Account{{Num: "1", Email: "a@x.com"}}, readable: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := legacy.items[LegacyKeyringService+"\x00account-1-a@x.com"]; !ok {
		t.Error("the legacy item was deleted; that risks a second Keychain prompt for no gain")
	}
}

// The legacy account-None-{email} spelling maps to a slot only when its email is
// unique — with duplicates there is no way to tell which slot it belonged to.
func TestMacOSMigrationHandlesTheLegacyNoneSpelling(t *testing.T) {
	t.Run("a unique email adopts the None entry", func(t *testing.T) {
		backups := newFakeBackups()
		legacy := fakeLegacy{items: map[string]string{
			LegacyKeyringService + "\x00account-None-a@x.com": "creds-from-none",
		}}

		if _, err := runMacOS(t, legacy, backups, platform.MacOS,
			fakeRoster{accounts: []Account{{Num: "1", Email: "a@x.com"}}, readable: true}); err != nil {
			t.Fatal(err)
		}
		if got := backups.items[backups.key("1", "a@x.com")]; got != "creds-from-none" {
			t.Errorf("account 1 = %q, want the None entry adopted", got)
		}
	})

	t.Run("a duplicated email leaves the None entry alone", func(t *testing.T) {
		backups := newFakeBackups()
		legacy := fakeLegacy{items: map[string]string{
			LegacyKeyringService + "\x00account-None-a@x.com": "ambiguous",
		}}

		outcome, err := runMacOS(t, legacy, backups, platform.MacOS, fakeRoster{
			accounts: []Account{{Num: "1", Email: "a@x.com"}, {Num: "2", Email: "a@x.com"}},
			readable: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if outcome != Completed {
			t.Errorf("outcome = %v, want Completed; an ambiguous entry is benign", outcome)
		}
		if len(backups.items) != 0 {
			t.Errorf("backups = %v, want an ambiguous None entry left untouched", backups.items)
		}
	})
}

// A Keychain that cannot answer is not "nothing to migrate": defer and retry
// rather than skipping real entries.
func TestMacOSMigrationDefersOnAnUnreadableKeychain(t *testing.T) {
	backups := newFakeBackups()
	backups.readErr = errors.New("keychain locked")

	_, err := runMacOS(t, fakeLegacy{}, backups, platform.MacOS,
		fakeRoster{accounts: []Account{{Num: "1", Email: "a@x.com"}}, readable: true})
	if !errors.Is(err, apperr.ErrMigrationIncomplete) {
		t.Fatalf("err = %v, want an incomplete-migration error so the run retries", err)
	}
}

// A partial or garbage item must not shadow the still-intact legacy entry.
func TestMacOSMigrationDiscardsABadWrite(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*fakeBackups)
	}{
		{"the write fails", func(b *fakeBackups) { b.writeErr = errors.New("denied") }},
		{"the read-back does not match", func(b *fakeBackups) { b.garbleOne = "1" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			backups := newFakeBackups()
			legacy := fakeLegacy{items: map[string]string{
				LegacyKeyringService + "\x00account-1-a@x.com": "creds",
			}}
			tt.setup(backups)

			_, err := runMacOS(t, legacy, backups, platform.MacOS,
				fakeRoster{accounts: []Account{{Num: "1", Email: "a@x.com"}}, readable: true})
			if !errors.Is(err, apperr.ErrMigrationIncomplete) {
				t.Fatalf("err = %v, want an incomplete-migration error", err)
			}
			if len(backups.deletes) == 0 {
				t.Error("the bad item was not discarded; it would shadow the intact legacy entry")
			}
			if _, ok := legacy.items[LegacyKeyringService+"\x00account-1-a@x.com"]; !ok {
				t.Error("the legacy entry was removed despite the failure")
			}
		})
	}
}

// An account with nothing in the legacy service is benign, not a failure: it was
// added on a newer version.
func TestMacOSMigrationIgnoresAccountsWithNoLegacyEntry(t *testing.T) {
	outcome, err := runMacOS(t, fakeLegacy{items: map[string]string{}}, newFakeBackups(), platform.MacOS,
		fakeRoster{accounts: []Account{{Num: "1", Email: "a@x.com"}}, readable: true})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if outcome != Completed {
		t.Errorf("outcome = %v, want Completed", outcome)
	}
}

// Running it twice must be safe, and the second run must find nothing to do.
func TestMacOSMigrationIsIdempotent(t *testing.T) {
	backups := newFakeBackups()
	legacy := fakeLegacy{items: map[string]string{
		LegacyKeyringService + "\x00account-1-a@x.com": "creds",
	}}
	roster := fakeRoster{accounts: []Account{{Num: "1", Email: "a@x.com"}}, readable: true}

	for i := range 2 {
		outcome, err := runMacOS(t, legacy, backups, platform.MacOS, roster)
		if err != nil || outcome != Completed {
			t.Fatalf("run %d: outcome = %v, err = %v", i+1, outcome, err)
		}
	}
	if got := backups.items[backups.key("1", "a@x.com")]; got != "creds" {
		t.Errorf("account 1 = %q after two runs, want the credential intact", got)
	}
}

// ---------------------------------------------------------------- Windows notice

// fakeProbe reports which slots have a readable backup.
type fakeProbe struct {
	present    map[string]string
	unreadable map[string]bool
}

func (f fakeProbe) ReadAccount(num, email string) (string, bool) {
	key := num + "\x00" + email
	return f.present[key], f.unreadable[key]
}

func runWindowsNotice(t *testing.T, probe fakeProbe, p platform.Platform, roster Roster) (Outcome, error) {
	t.Helper()
	return WindowsKeyringNotice(probe, p).Run(t.TempDir(), roster)
}

// The notice reuses the Python migration's id, so an install where that
// migration already ran is skipped by the runner before this is ever called.
func TestWindowsNoticeSharesThePythonMigrationID(t *testing.T) {
	root := t.TempDir()
	markApplied(root, WindowsKeyringMigrationID)

	ran := false
	Run(root, fakeRoster{readable: true}, []Migration{{
		ID:  WindowsKeyringMigrationID,
		Run: func(string, Roster) (Outcome, error) { ran = true; return Completed, nil },
	}})
	if ran {
		t.Error("the notice ran on an install where the Python migration was already applied")
	}
}

func TestWindowsNoticeSkipsWhenNotApplicable(t *testing.T) {
	tests := []struct {
		name     string
		platform platform.Platform
		roster   fakeRoster
	}{
		{"off Windows", platform.MacOS, fakeRoster{accounts: []Account{{Num: "1", Email: "a@x.com"}}, readable: true}},
		{"an unreadable roster", platform.Windows, fakeRoster{readable: false}},
		// Accounts restored from a pre-0.11 backup later still deserve the
		// advice, so an empty roster must not record the notice as done.
		{"no accounts yet", platform.Windows, fakeRoster{readable: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outcome, err := runWindowsNotice(t, fakeProbe{}, tt.platform, tt.roster)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if outcome != Skipped {
				t.Errorf("outcome = %v, want Skipped", outcome)
			}
		})
	}
}

// Once every account has a backup the notice is moot, and recording it stops the
// probe cost on every later startup.
func TestWindowsNoticeCompletesWhenEveryBackupIsPresent(t *testing.T) {
	probe := fakeProbe{present: map[string]string{"1\x00a@x.com": "creds"}}

	outcome, err := runWindowsNotice(t, probe, platform.Windows,
		fakeRoster{accounts: []Account{{Num: "1", Email: "a@x.com"}}, readable: true})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Completed {
		t.Errorf("outcome = %v, want Completed", outcome)
	}
}

// A stranded account keeps the notice unrecorded, so the advice reappears until
// it is acted on.
func TestWindowsNoticeStaysUnrecordedWhileAnAccountIsStranded(t *testing.T) {
	outcome, err := runWindowsNotice(t, fakeProbe{}, platform.Windows,
		fakeRoster{accounts: []Account{{Num: "1", Email: "a@x.com"}}, readable: true})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Skipped {
		t.Errorf("outcome = %v, want Skipped so the advice reappears", outcome)
	}
}

// An unreadable backup is a transient problem with its own message; advising a
// Python round trip for it would send the user somewhere useless.
func TestWindowsNoticeIgnoresMerelyUnreadableBackups(t *testing.T) {
	probe := fakeProbe{unreadable: map[string]bool{"1\x00a@x.com": true}}

	outcome, err := runWindowsNotice(t, probe, platform.Windows,
		fakeRoster{accounts: []Account{{Num: "1", Email: "a@x.com"}}, readable: true})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Completed {
		t.Errorf("outcome = %v, want Completed; an unreadable backup is not a stranded one", outcome)
	}
}
