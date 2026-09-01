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
