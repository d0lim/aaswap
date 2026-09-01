package credstore

import (
	"slices"
	"testing"

	"github.com/d0lim/aaswap/internal/platform"
)

// seedClaudeSwapItem plants a backup under the claude-swap project's service,
// which is what an install being imported from looks like.
func seedClaudeSwapItem(s *Store, fake *fakeKeychain, num, email, value string) {
	fake.items[ClaudeSwapBackupService+"\x00"+s.backupUsername(num, email)] = value
}

// The whole point of a separate service: neither project's `remove` may delete
// the other's item for the same slot.
func TestTheTwoProjectsUseDifferentKeychainServices(t *testing.T) {
	if BackupService == ClaudeSwapBackupService {
		t.Fatalf("both projects share the service %q, so either one's remove "+
			"deletes the other's backup", BackupService)
	}
}

func TestAdoptingBackupsFromClaudeSwap(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	seedClaudeSwapItem(s, fake, "1", "a@example.com", "creds-1")
	seedClaudeSwapItem(s, fake, "2", "b@example.com", "creds-2")

	report, err := s.AdoptKeychain(ClaudeSwapBackupService, map[string]string{
		"1": "a@example.com",
		"2": "b@example.com",
		// A slot with nothing in the claude-swap Keychain — off macOS there
		// never was one, and its .enc came across with the directory.
		"3": "c@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}

	if report.Copied != 2 {
		t.Errorf("Copied = %d, want 2", report.Copied)
	}
	if !slices.Equal(report.Missing, []string{"3"}) {
		t.Errorf("Missing = %v, want just the slot that had no item", report.Missing)
	}
	for _, tc := range []struct{ num, email, want string }{
		{"1", "a@example.com", "creds-1"},
		{"2", "b@example.com", "creds-2"},
	} {
		got, err := s.readBackupKeychain(tc.num, tc.email)
		if err != nil || got != tc.want {
			t.Errorf("account %s reads %q (err %v) under aaswap's service, want %q",
				tc.num, got, err, tc.want)
		}
	}
}

// The originals stay. Moving the directory back has to restore a working
// claude-swap install, or the import is a one-way door.
func TestAdoptionLeavesTheClaudeSwapItemsInPlace(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	username := s.backupUsername("1", "a@example.com")
	seedClaudeSwapItem(s, fake, "1", "a@example.com", "creds-1")

	if _, err := s.AdoptKeychain(ClaudeSwapBackupService, map[string]string{"1": "a@example.com"}); err != nil {
		t.Fatal(err)
	}

	value, found, err := s.kc.Get(ClaudeSwapBackupService, username)
	if err != nil || !found || value != "creds-1" {
		t.Error("the claude-swap item was consumed, so the import cannot be undone")
	}
}

// Reads are .enc-wins. An .enc that arrived with the directory would otherwise
// keep answering for the Keychain item just written, serving a stale generation
// forever.
func TestAdoptionClearsAnEncThatWouldShadowTheAdoptedItem(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	writeEnc(t, s, "1", "a@example.com", "stale-from-the-moved-directory")
	seedClaudeSwapItem(s, fake, "1", "a@example.com", "fresh")

	if _, err := s.AdoptKeychain(ClaudeSwapBackupService, map[string]string{"1": "a@example.com"}); err != nil {
		t.Fatal(err)
	}

	got, unreadable := s.ReadAccount("1", "a@example.com")
	if unreadable {
		t.Fatal("the backup became unreadable")
	}
	if got != "fresh" {
		t.Errorf("ReadAccount = %q, want the adopted item to win over the stale .enc", got)
	}
}

// Off macOS the Keychain was never a backend, so there is nothing to adopt and
// nothing to report as missing.
func TestAdoptionIsANoOpOffMacOS(t *testing.T) {
	s, _ := newTestStore(t, platform.Linux)
	report, err := s.AdoptKeychain(ClaudeSwapBackupService, map[string]string{"1": "a@example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Copied != 0 || len(report.Missing) != 0 || len(report.Failed) != 0 {
		t.Errorf("report = %+v, want an empty one", report)
	}
}
