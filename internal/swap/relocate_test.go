package swap

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/testutil"
)

// threeAccounts registers three switchable slots with slot 2 active.
func (f *fixture) threeAccounts() *Roster {
	f.t.Helper()
	roster := f.seedAccounts(map[string]*Account{
		"1": {Email: "one@example.com", Alias: "first"},
		"2": {Email: "two@example.com", Alias: "second"},
		"3": {Email: "three@example.com"},
	})
	roster.SetActive("2", f.now)
	f.seedRoster(roster)
	return roster
}

func TestSwapExchangesTwoSlots(t *testing.T) {
	f := newFixture(t)
	f.threeAccounts()
	credsOne, _ := f.Creds.ReadAccount("1", "one@example.com")
	credsThree, _ := f.Creds.ReadAccount("3", "three@example.com")

	numA, numB, err := f.SwapSlots("1", "3")
	if err != nil {
		t.Fatal(err)
	}
	if numA != "1" || numB != "3" {
		t.Errorf("SwapSlots = (%q, %q)", numA, numB)
	}

	roster := f.roster()
	if roster.Accounts["1"].Email != "three@example.com" {
		t.Errorf("slot 1 = %q, want the other account", roster.Accounts["1"].Email)
	}
	if roster.Accounts["3"].Email != "one@example.com" {
		t.Errorf("slot 3 = %q", roster.Accounts["3"].Email)
	}
	// An alias belongs to the account, so it moves with it.
	if roster.Accounts["3"].Alias != "first" {
		t.Errorf("slot 3's alias = %q, want it to have travelled", roster.Accounts["3"].Alias)
	}

	// The stored material moved too, or the slots would name credentials
	// nobody wrote.
	if got, _ := f.Creds.ReadAccount("3", "one@example.com"); got != credsOne {
		t.Errorf("slot 3's credential = %q, want the one that moved there", got)
	}
	if got, _ := f.Creds.ReadAccount("1", "three@example.com"); got != credsThree {
		t.Errorf("slot 1's credential = %q", got)
	}
	// And the old keys are cleared, so a future account on that number cannot
	// inherit them.
	if got, _ := f.Creds.ReadAccount("1", "one@example.com"); got != "" {
		t.Errorf("a stale credential was left under the old key: %q", got)
	}
	if config := f.ReadAccountConfig("1", "one@example.com"); config != "" {
		t.Errorf("a stale config was left under the old key: %q", config)
	}
}

// The ordering follows the new numbers, so rotation and list order do too.
func TestSwapKeepsTheSequenceSorted(t *testing.T) {
	f := newFixture(t)
	f.threeAccounts()
	if _, _, err := f.SwapSlots("1", "3"); err != nil {
		t.Fatal(err)
	}
	if want := []int{1, 2, 3}; !reflect.DeepEqual(f.roster().Sequence, want) {
		t.Errorf("Sequence = %v, want %v", f.roster().Sequence, want)
	}
}

// The active pointer follows its account, not its old number.
func TestSwapMovesTheActivePointer(t *testing.T) {
	f := newFixture(t)
	f.threeAccounts()

	if _, _, err := f.SwapSlots("2", "3"); err != nil {
		t.Fatal(err)
	}
	roster := f.roster()
	num, ok := roster.Active()
	if !ok || num != "3" {
		t.Errorf("Active = (%q, %v), want slot 3", num, ok)
	}
	if roster.Accounts[num].Email != "two@example.com" {
		t.Errorf("the active slot holds %q, want the account that was active",
			roster.Accounts[num].Email)
	}
}

// Two slots sharing an address have identical backup keys, so writing one
// destroys the other. Their material is staged first.
func TestSwappingTwoSlotsThatShareAnAddress(t *testing.T) {
	f := newFixture(t)
	roster := f.seedAccounts(map[string]*Account{
		"1": {Email: "same@example.com", OrganizationUUID: "org-1", OrganizationName: "One"},
		"2": {Email: "same@example.com", OrganizationUUID: "org-2", OrganizationName: "Two"},
	})
	f.seedRoster(roster)
	// Distinct material under identical keys.
	if err := f.Creds.WriteAccount("1", "same@example.com", "creds-for-org-1"); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteAccountConfig("1", "same@example.com", "config-for-org-1"); err != nil {
		t.Fatal(err)
	}
	if err := f.Creds.WriteAccount("2", "same@example.com", "creds-for-org-2"); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteAccountConfig("2", "same@example.com", "config-for-org-2"); err != nil {
		t.Fatal(err)
	}

	if _, _, err := f.SwapSlots("1", "2"); err != nil {
		t.Fatal(err)
	}

	roster = f.roster()
	if roster.Accounts["1"].OrganizationUUID != "org-2" {
		t.Errorf("slot 1 = %+v, want the other organization", roster.Accounts["1"])
	}
	// Each slot serves its new owner's material, not the other's.
	if got, _ := f.Creds.ReadAccount("1", "same@example.com"); got != "creds-for-org-2" {
		t.Errorf("slot 1's credential = %q, want org 2's", got)
	}
	if got, _ := f.Creds.ReadAccount("2", "same@example.com"); got != "creds-for-org-1" {
		t.Errorf("slot 2's credential = %q, want org 1's", got)
	}
	if got := f.ReadAccountConfig("1", "same@example.com"); got != "config-for-org-2" {
		t.Errorf("slot 1's config = %q", got)
	}
	// Nothing is left parked.
	if entries, err := os.ReadDir(f.BackupRoot() + "/staging"); err == nil && len(entries) > 0 {
		t.Errorf("staging still holds %d files", len(entries))
	}
}

func TestSwapRejections(t *testing.T) {
	tests := []struct {
		name    string
		a, b    string
		wantErr []string
	}{
		{"an account with itself", "1", "1", []string{"with itself"}},
		{"an account with its own alias", "1", "first", []string{"with itself"}},
		{"a slot that does not exist", "1", "9", []string{"account 9 does not exist"}},
		{"nothing that matches", "1", "nobody@example.com", []string{"does not match any managed account"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.threeAccounts()
			before := f.rawRoster()

			_, _, err := f.SwapSlots(tt.a, tt.b)
			wantErr(t, err, tt.wantErr...)
			if !reflect.DeepEqual(f.rawRoster(), before) {
				t.Error("a rejected swap changed the roster")
			}
		})
	}
}

// Unreadable is not absent. Committing a relocation on that reading would drop
// the slot's live refresh token in favor of an empty destination.
func TestARelocationAbortsOnAnUnreadableBackup(t *testing.T) {
	f := newFixture(t)
	f.threeAccounts()
	// A backup that EXISTS and cannot be read — not the same as absent. A
	// corrupt .enc would not do: that is content-level and falls through to the
	// Keychain by design.
	testutil.MakeUnreadable(t, filepath.Join(f.Creds.CredentialsDir(),
		".creds-1-one@example.com.enc"))
	before := f.rawRoster()

	_, _, err := f.SwapSlots("1", "3")
	wantErr(t, err, "could not be read", "nothing was changed")
	if !reflect.DeepEqual(f.rawRoster(), before) {
		t.Error("an aborted swap changed the roster")
	}
}

func TestMoveToAnEmptySlot(t *testing.T) {
	f := newFixture(t)
	f.threeAccounts()
	creds, _ := f.Creds.ReadAccount("1", "one@example.com")

	from, to, swapped, err := f.MoveAccount("1", "7")
	if err != nil {
		t.Fatal(err)
	}
	if from != "1" || to != "7" || swapped {
		t.Errorf("MoveAccount = (%q, %q, swapped=%v)", from, to, swapped)
	}

	roster := f.roster()
	if _, still := roster.Accounts["1"]; still {
		t.Error("the source slot survived the move")
	}
	if roster.Accounts["7"].Email != "one@example.com" {
		t.Errorf("slot 7 = %+v", roster.Accounts["7"])
	}
	if got, _ := f.Creds.ReadAccount("7", "one@example.com"); got != creds {
		t.Errorf("slot 7's credential = %q, want the moved one", got)
	}
	if got, _ := f.Creds.ReadAccount("1", "one@example.com"); got != "" {
		t.Errorf("the old key still serves %q", got)
	}
	if want := []int{2, 3, 7}; !reflect.DeepEqual(roster.Sequence, want) {
		t.Errorf("Sequence = %v, want %v", roster.Sequence, want)
	}
}

// Moving onto an occupied slot is a swap, because that is what the user meant.
func TestMoveOntoAnOccupiedSlotSwaps(t *testing.T) {
	f := newFixture(t)
	f.threeAccounts()

	from, to, swapped, err := f.MoveAccount("1", "3")
	if err != nil {
		t.Fatal(err)
	}
	if !swapped || from != "1" || to != "3" {
		t.Errorf("MoveAccount = (%q, %q, swapped=%v), want a swap", from, to, swapped)
	}
	roster := f.roster()
	if roster.Accounts["3"].Email != "one@example.com" || roster.Accounts["1"].Email != "three@example.com" {
		t.Errorf("the slots did not exchange: 1=%q 3=%q",
			roster.Accounts["1"].Email, roster.Accounts["3"].Email)
	}
}

func TestMoveRejections(t *testing.T) {
	tests := []struct {
		name       string
		id, target string
		wantErr    []string
	}{
		{"to a non-numeric target", "1", "work", []string{"must be a slot number"}},
		{"to slot zero", "1", "0", []string{"1 or greater"}},
		{"to its own slot", "1", "1", []string{"already in slot 1"}},
		{"an account that does not exist", "nobody@example.com", "7", []string{"does not match"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.threeAccounts()
			before := f.rawRoster()

			_, _, _, err := f.MoveAccount(tt.id, tt.target)
			wantErr(t, err, tt.wantErr...)
			if !reflect.DeepEqual(f.rawRoster(), before) {
				t.Error("a rejected move changed the roster")
			}
		})
	}
}

// A slot with no stored material moves as an empty slot; the destination must
// not be left serving whatever was there before.
func TestMovingAnEmptySlotClearsTheDestination(t *testing.T) {
	f := newFixture(t)
	roster := f.seedAccounts(map[string]*Account{"1": {Email: "one@example.com"}})
	roster.Insert("2", &Account{Email: "unbacked@example.com"}, f.now)
	f.seedRoster(roster)
	// Stale material leaked under slot 7 by an earlier crash.
	if err := f.Creds.WriteAccount("7", "unbacked@example.com", "stale-leftovers"); err != nil {
		t.Fatal(err)
	}

	if _, _, _, err := f.MoveAccount("2", "7"); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.Creds.ReadAccount("7", "unbacked@example.com"); got != "" {
		t.Errorf("slot 7 serves %q, want the leftovers cleared", got)
	}
}

// The retained generation holds the DISPLACED material, which recovery must
// never resurrect onto the key's new owner.
func TestARelocationDropsTheRetainedGeneration(t *testing.T) {
	f := newFixture(t)
	f.threeAccounts()
	// Give slot 3 a retained generation by overwriting its backup.
	if err := f.Creds.WriteAccount("3", "three@example.com", "second-generation"); err != nil {
		t.Fatal(err)
	}
	if got := f.Creds.ReadPreviousBackup("3", "three@example.com"); got == "" {
		t.Fatal("no generation was retained to begin with")
	}

	if _, _, err := f.SwapSlots("1", "3"); err != nil {
		t.Fatal(err)
	}
	if got := f.Creds.ReadPreviousBackup("3", "one@example.com"); got != "" {
		t.Errorf("slot 3 retained %q — another account's displaced material", got)
	}
}

func TestRemoveForgetsAnAccount(t *testing.T) {
	f := newFixture(t)
	f.threeAccounts()

	got, err := f.Remove(RemoveRequest{Identifier: "1", AssumeYes: true})
	if err != nil {
		t.Fatal(err)
	}
	if got.Number != "1" || got.Email != "one@example.com" || got.WasActive {
		t.Errorf("outcome = %+v", got)
	}

	roster := f.roster()
	if _, still := roster.Accounts["1"]; still {
		t.Error("the account survived the removal")
	}
	if want := []int{2, 3}; !reflect.DeepEqual(roster.Sequence, want) {
		t.Errorf("Sequence = %v, want %v", roster.Sequence, want)
	}
	if creds, _ := f.Creds.ReadAccount("1", "one@example.com"); creds != "" {
		t.Errorf("the stored credential survived: %q", creds)
	}
	if config := f.ReadAccountConfig("1", "one@example.com"); config != "" {
		t.Errorf("the stored config survived: %q", config)
	}
}

// Removing a slot forgets aaswap's copy; it does not log the user out.
func TestRemovingTheActiveAccountLeavesTheLiveLoginAlone(t *testing.T) {
	f := newFixture(t)
	f.threeAccounts()
	f.setLiveIdentity("two@example.com", "", "", "")
	if err := f.Creds.WriteActive("live-credential"); err != nil {
		t.Fatal(err)
	}

	got, err := f.Remove(RemoveRequest{Identifier: "2", AssumeYes: true})
	if err != nil {
		t.Fatal(err)
	}
	if !got.WasActive {
		t.Error("removing the active account was not reported as such")
	}
	if f.activeCreds() != "live-credential" {
		t.Errorf("the live credential changed: %q", f.activeCreds())
	}
	if _, ok := f.LiveIdentity(); !ok {
		t.Error("the user was logged out by a removal")
	}
	// The active pointer cannot outlive the slot.
	if num, ok := f.roster().Active(); ok {
		t.Errorf("Active = %q, want nothing", num)
	}
}

// Permanently discarding a stored login is not something to do without asking.
func TestRemoveRequiresConfirmation(t *testing.T) {
	t.Run("declined", func(t *testing.T) {
		f := newFixture(t)
		f.threeAccounts()
		got, err := f.Remove(RemoveRequest{Identifier: "1", Confirm: func(string) bool { return false }})
		if err != nil {
			t.Fatal(err)
		}
		if !got.Cancelled {
			t.Errorf("outcome = %+v, want a cancellation", got)
		}
		if _, still := f.roster().Accounts["1"]; !still {
			t.Error("a declined removal removed the account anyway")
		}
	})

	t.Run("no way to ask", func(t *testing.T) {
		f := newFixture(t)
		f.threeAccounts()
		got, err := f.Remove(RemoveRequest{Identifier: "1"})
		if err != nil {
			t.Fatal(err)
		}
		if !got.Cancelled {
			t.Errorf("outcome = %+v, want a cancellation", got)
		}
		if _, still := f.roster().Accounts["1"]; !still {
			t.Error("an account was removed with nobody to confirm it")
		}
	})

	t.Run("the prompt names the account", func(t *testing.T) {
		f := newFixture(t)
		f.threeAccounts()
		var prompt string
		if _, err := f.Remove(RemoveRequest{Identifier: "1", Confirm: func(p string) bool {
			prompt = p
			return true
		}}); err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"1", "one@example.com"} {
			if !strings.Contains(prompt, want) {
				t.Errorf("the prompt does not name %q: %s", want, prompt)
			}
		}
	})
}

// An address naming several slots is ambiguous. An interactive caller can
// choose; anyone else gets the error, which already names the candidates.
func TestRemovingAnAmbiguousAddress(t *testing.T) {
	setup := func(t *testing.T) *fixture {
		f := newFixture(t)
		roster := f.seedAccounts(map[string]*Account{
			"1": {Email: "same@example.com", OrganizationName: "One", OrganizationUUID: "org-1"},
			"2": {Email: "same@example.com", OrganizationName: "Two", OrganizationUUID: "org-2"},
		})
		f.seedRoster(roster)
		return f
	}

	t.Run("with no way to choose it is an error", func(t *testing.T) {
		f := setup(t)
		_, err := f.Remove(RemoveRequest{Identifier: "same@example.com", AssumeYes: true})
		wantErr(t, err, "ambiguous", "1 [One]", "2 [Two]")
	})

	t.Run("an interactive caller picks", func(t *testing.T) {
		f := setup(t)
		var offered []AmbiguousMatch
		got, err := f.Remove(RemoveRequest{
			Identifier: "same@example.com", AssumeYes: true,
			ChooseAmbiguous: func(matches []AmbiguousMatch) (string, bool) {
				offered = matches
				return "2", true
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got.Number != "2" {
			t.Errorf("removed slot %q, want the chosen one", got.Number)
		}
		if len(offered) != 2 {
			t.Errorf("offered = %v, want both candidates", offered)
		}
		if _, still := f.roster().Accounts["1"]; !still {
			t.Error("the unchosen slot was removed too")
		}
	})

	t.Run("declining removes nothing", func(t *testing.T) {
		f := setup(t)
		got, err := f.Remove(RemoveRequest{
			Identifier: "same@example.com", AssumeYes: true,
			ChooseAmbiguous: func([]AmbiguousMatch) (string, bool) { return "", false },
		})
		if err != nil {
			t.Fatal(err)
		}
		if !got.Cancelled {
			t.Errorf("outcome = %+v, want a cancellation", got)
		}
		if len(f.roster().Accounts) != 2 {
			t.Error("declining the choice removed something")
		}
	})
}

func TestPurgeForgetsEverything(t *testing.T) {
	f := newFixture(t)
	f.threeAccounts()

	got, err := f.Purge(nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Removed) != 3 {
		t.Errorf("Removed = %v, want all three", got.Removed)
	}

	roster := f.roster()
	if len(roster.Accounts) != 0 || len(roster.Sequence) != 0 {
		t.Errorf("the roster still holds %+v", roster)
	}
	for num, email := range map[string]string{
		"1": "one@example.com", "2": "two@example.com", "3": "three@example.com",
	} {
		if creds, _ := f.Creds.ReadAccount(num, email); creds != "" {
			t.Errorf("slot %s's credential survived the purge", num)
		}
	}
}

// The live login is Claude Code's, not aaswap's.
func TestPurgeLeavesTheLiveLoginAlone(t *testing.T) {
	f := newFixture(t)
	f.threeAccounts()
	f.setLiveIdentity("two@example.com", "", "", "")
	if err := f.Creds.WriteActive("live-credential"); err != nil {
		t.Fatal(err)
	}

	if _, err := f.Purge(nil, true); err != nil {
		t.Fatal(err)
	}
	if f.activeCreds() != "live-credential" {
		t.Errorf("the live credential changed: %q", f.activeCreds())
	}
	if _, ok := f.LiveIdentity(); !ok {
		t.Error("the user was logged out by a purge")
	}
}

func TestPurgeRequiresConfirmation(t *testing.T) {
	f := newFixture(t)
	f.threeAccounts()

	got, err := f.Purge(func(string) bool { return false }, false)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Cancelled {
		t.Errorf("outcome = %+v, want a cancellation", got)
	}
	if len(f.roster().Accounts) != 3 {
		t.Error("a declined purge removed accounts anyway")
	}
}
