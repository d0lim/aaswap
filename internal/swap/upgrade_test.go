package swap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedLegacyStore writes a version 1 store: the old table, and credentials and
// configs filed under slot numbers.
func (f *fixture) seedLegacyStore(t *testing.T, table string, slots map[string]string) {
	t.Helper()
	if err := os.WriteFile(f.RosterPath(), []byte(table), 0o600); err != nil {
		t.Fatal(err)
	}
	// Seeded through the unscoped store and the unscoped configs directory:
	// that is where a version 1 install actually put these.
	legacy := f.Creds.Unscoped()
	for num, email := range slots {
		if err := legacy.WriteAccount(num, email,
			`{"claudeAiOauth":{"accessToken":"tok-`+num+`"}}`); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(f.legacyConfigsDir(), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.legacyConfigsDir(),
			".claude-config-"+num+"-"+email+".json"),
			[]byte(`{"oauthAccount":{"emailAddress":"`+email+`"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// The upgrade is only worth anything if the credentials come with it. A table
// that names accounts whose material is still filed under slot numbers is a
// store where every switch fails.
func TestUpgradeMovesTheStoredMaterial(t *testing.T) {
	f := newFixture(t)
	f.seedLegacyStore(t, `{"activeAccountNumber":2,"sequence":[1,2],
	  "accounts":{"1":{"email":"one@example.com","alias":"work"},
	              "2":{"email":"two@example.com"}}}`,
		map[string]string{"1": "one@example.com", "2": "two@example.com"})

	moved, err := f.EnsureUpgraded()
	if err != nil {
		t.Fatal(err)
	}
	if moved != 2 {
		t.Errorf("moved = %d, want both accounts", moved)
	}

	roster := f.roster()
	if roster.Accounts["work"] == nil || roster.Accounts["two"] == nil {
		t.Fatalf("accounts = %v, want work and two", roster.Names())
	}
	if name, ok := roster.ActiveName(); !ok || name != "two" {
		t.Errorf("active = (%q, %v), want two", name, ok)
	}

	for name, email := range map[string]string{"work": "one@example.com", "two": "two@example.com"} {
		value, unreadable := f.Creds.ReadAccount(name, email)
		if unreadable || value == "" {
			t.Errorf("%s has no credential under its new name", name)
		}
		if config := f.ReadAccountConfig(name, email); !strings.Contains(config, email) {
			t.Errorf("%s has no config under its new name: %q", name, config)
		}
	}

	// The old copies are gone, or every account's credential exists twice and
	// the stale one outlives the next refresh.
	for num, email := range map[string]string{"1": "one@example.com", "2": "two@example.com"} {
		if value, _ := f.Creds.Unscoped().ReadAccount(num, email); value != "" {
			t.Errorf("slot %s's credential was left behind", num)
		}
	}
}

// Running twice must not move anything the second time, or every invocation
// renames the whole store.
func TestUpgradeIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.seedLegacyStore(t, `{"sequence":[1],"accounts":{"1":{"email":"one@example.com"}}}`,
		map[string]string{"1": "one@example.com"})

	if _, err := f.EnsureUpgraded(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(f.RosterPath())
	if err != nil {
		t.Fatal(err)
	}

	moved, err := f.EnsureUpgraded()
	if err != nil {
		t.Fatal(err)
	}
	if moved != 0 {
		t.Errorf("moved = %d on a store already upgraded", moved)
	}
	after, err := os.ReadFile(f.RosterPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Errorf("the table changed on a second run:\n%s\nvs\n%s", before, after)
	}
}

// An absent store has nothing to upgrade and must not be created by asking.
func TestUpgradeLeavesAnAbsentStoreAbsent(t *testing.T) {
	f := newFixture(t)
	moved, err := f.EnsureUpgraded()
	if err != nil {
		t.Fatal(err)
	}
	if moved != 0 {
		t.Errorf("moved = %d, want nothing", moved)
	}
	if _, err := os.Stat(f.RosterPath()); err == nil {
		t.Error("asking about an absent store created one")
	}
}

// The table is published only after the material has moved. A crash before the
// publish has to leave version 1 readable rather than a table naming
// credentials that are not there.
func TestUpgradeRefusesWhenTheMaterialCannotMove(t *testing.T) {
	f := newFixture(t)
	f.seedLegacyStore(t, `{"sequence":[1,2],
	  "accounts":{"1":{"email":"one@example.com"},"2":{"email":"two@example.com"}}}`,
		map[string]string{"1": "one@example.com"})
	// Slot 2 is in the table with no credential on disk at all — the shape a
	// half-written store has.

	if _, err := f.EnsureUpgraded(); err != nil {
		t.Fatalf("a slot with no credential blocked the upgrade: %v", err)
	}
	// It still upgrades: an empty slot is not an unreadable one, and stranding
	// the whole store over a missing file would be worse than carrying an
	// account that needs re-adding.
	if _, ok := f.roster().Accounts["two"]; !ok {
		t.Errorf("accounts = %v, want the credential-less account carried over", f.roster().Names())
	}
}

// The upgrade lands the material in the CURRENT layout, not merely under a new
// name in the old one.
//
// This is what folding the vault into version 2 buys: version 2 was never
// released, so a person upgrading walks their live credentials exactly once.
// Had the two changes shipped separately, the same tokens would have been moved
// twice — and every move is a chance to lose one.
func TestUpgradeLandsInTheVaultLayout(t *testing.T) {
	f := newFixture(t)
	f.seedLegacyStore(t, `{"activeAccountNumber":1,"sequence":[1],
	  "accounts":{"1":{"email":"one@example.com","alias":"work"}}}`,
		map[string]string{"1": "one@example.com"})

	if _, err := f.EnsureUpgraded(); err != nil {
		t.Fatal(err)
	}

	// The account has a directory of its own, holding its credential.
	dir := f.Creds.AccountDir("work", "one@example.com")
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("the upgraded account has no directory of its own: %v", err)
	}
	backup := f.Creds.BackupPath("work", "one@example.com")
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("the credential is not in the account's directory: %v", err)
	}
	if filepath.Dir(backup) != dir {
		t.Errorf("the credential is at %q, outside the account's directory %q", backup, dir)
	}

	// And the flat copy the old layout used is gone. Two copies of one
	// credential means the stale one outlives the next refresh.
	stale := filepath.Join(f.BackupRoot(), "credentials", ".creds-1-one@example.com.enc")
	if _, err := os.Stat(stale); err == nil {
		t.Errorf("the pre-upgrade copy at %s was left behind", stale)
	}
}

// The upgrade must not be able to delete what it just wrote.
//
// The old copies and the new ones have to be at genuinely different paths. An
// earlier version of the unscoped view derived its directory by walking back up
// from the scoped one and landed on the same place — and every test that seeded
// through that same accessor agreed with the bug.
func TestUpgradeReadsAndWritesDifferentPlaces(t *testing.T) {
	f := newFixture(t)
	legacy := f.Creds.Unscoped()
	if legacy.BackupPath("1", "one@example.com") == f.Creds.BackupPath("1", "one@example.com") {
		t.Fatalf("the legacy view and the current store share %s, so the upgrade's "+
			"final step would delete the copy it had just written",
			legacy.BackupPath("1", "one@example.com"))
	}
}

// A version 1 store predates providers, and every account in it is Claude's.
//
// The upgrade filed them under whoever happened to ASK. So a user upgrading
// from ccswap whose first command was `aaswap --provider codex list` had every
// Claude account migrated into the Codex section, with the Claude credentials
// written into the Codex vault. `aaswap list` then showed no accounts at all,
// and `aaswap --provider codex switch` wrote a Claude credential into
// ~/.codex/auth.json, over the Codex login that worked.
//
// One command, in the ordinary course of trying out a new provider.
func TestAVersionOneStoreUpgradesToClaudeWhoeverAsks(t *testing.T) {
	f := codexFixture(t)
	f.seedLegacyStore(t, `{
	  "accounts": {"1": {"email": "one@example.com"}, "2": {"email": "two@example.com"}},
	  "order": ["1", "2"],
	  "active": "1"
	}`, map[string]string{"1": "one@example.com", "2": "two@example.com"})

	if _, err := f.EnsureUpgraded(); err != nil {
		t.Fatalf("upgrading while addressing Codex: %v", err)
	}

	file, _, err := f.StoreOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	claude := file.Providers[ProviderClaude]
	if claude == nil || len(claude.Accounts) != 2 {
		t.Fatalf("the Claude section holds %v, want both version 1 accounts",
			file.Providers)
	}
	if codex := file.Providers[ProviderCodex]; codex != nil && len(codex.Accounts) != 0 {
		t.Errorf("the Codex section holds %v, which came out of a Claude store",
			codex.Accounts)
	}
}

// And the credentials go to Claude's vault, not the asking provider's. A roster
// that names them while the bytes sit somewhere else is a store where every
// switch fails.
func TestAVersionOneStoresCredentialsUpgradeToClaudes(t *testing.T) {
	f := codexFixture(t)
	f.seedLegacyStore(t, `{
	  "accounts": {"1": {"email": "one@example.com"}},
	  "order": ["1"],
	  "active": "1"
	}`, map[string]string{"1": "one@example.com"})

	if _, err := f.EnsureUpgraded(); err != nil {
		t.Fatal(err)
	}

	// Read through a Claude-scoped store, which is where the account now lives.
	claudeStore := f.storeFor(ProviderClaude)
	roster := mustRosterFor(t, f, ProviderClaude)
	name := roster.Names()[0]
	value, unreadable := claudeStore.ReadAccount(name, "one@example.com")
	if unreadable || !strings.Contains(value, "tok-1") {
		t.Errorf("Claude's store holds %q for %s, want the migrated credential",
			value, name)
	}
	// And nothing was filed under the provider that happened to ask.
	if value, _ := f.Creds.ReadAccount(name, "one@example.com"); value != "" {
		t.Errorf("the Codex store holds %q, a Claude credential", value)
	}
}
