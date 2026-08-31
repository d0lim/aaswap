package swap

import (
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/realiti4/claude-swap/internal/claudeapi"
)

// twoAccounts registers two switchable slots and leaves slot 1 live.
func (f *fixture) twoAccounts() *Roster {
	f.t.Helper()
	roster := f.seedAccounts(map[string]*Account{
		"1": {Email: "one@example.com", UUID: "acct-1", Added: Timestamp(f.now)},
		"2": {Email: "two@example.com", UUID: "acct-2", Added: Timestamp(f.now)},
	})
	roster.SetActive("1", f.now)
	f.seedRoster(roster)
	f.setLiveIdentity("one@example.com", "", "", "acct-1")
	if err := f.Creds.WriteActive(`{"claudeAiOauth":{"accessToken":"tok-1"}}`); err != nil {
		f.t.Fatal(err)
	}
	return roster
}

// liveConfig reads the live Claude Code config as an object.
func (f *fixture) liveConfig() map[string]any {
	f.t.Helper()
	data, err := os.ReadFile(f.Paths.GlobalConfigPath())
	if err != nil {
		f.t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		f.t.Fatalf("the live config is not a JSON object: %v\n%s", err, data)
	}
	return out
}

func (f *fixture) activeCreds() string {
	f.t.Helper()
	return f.Creds.ReadActive().Value
}

func TestSwitchActivatesTheTarget(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()

	got, err := f.Switch(t.Context(), SwitchRequest{Target: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if got.From == nil || got.From.Number != "1" || got.To.Number != "2" {
		t.Errorf("outcome = %+v", got)
	}

	// The target's credential is live.
	if !strings.Contains(f.activeCreds(), "tok-2") {
		t.Errorf("active credential = %q, want the target's", f.activeCreds())
	}
	// And the roster records the new active slot.
	if num, ok := f.roster().Active(); !ok || num != "2" {
		t.Errorf("Active = (%q, %v), want slot 2", num, ok)
	}
}

// The live config holds the user's projects, MCP servers and settings. Only the
// identity block belongs to the account.
func TestSwitchPreservesTheUsersOwnConfig(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	// A config with plenty that is none of ccswap's business.
	if err := os.WriteFile(f.Paths.GlobalConfigPath(), []byte(`{
	  "oauthAccount": {"emailAddress": "one@example.com"},
	  "projects": {"/home/u/work": {"allowedTools": ["Bash"]}},
	  "mcpServers": {"local": {"command": "srv"}},
	  "userID": "u-1"
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := f.Switch(t.Context(), SwitchRequest{Target: "2"}); err != nil {
		t.Fatal(err)
	}

	config := f.liveConfig()
	for _, key := range []string{"projects", "mcpServers", "userID"} {
		if _, present := config[key]; !present {
			t.Errorf("%q was lost by the switch: %v", key, config)
		}
	}
	account := config["oauthAccount"].(map[string]any)
	if account["emailAddress"] != "two@example.com" {
		t.Errorf("oauthAccount = %v, want the target's identity", account)
	}
}

// The departing account's live state has to land in its slot, or switching back
// logs the user into a credential the server has retired.
func TestSwitchBacksUpTheDepartingAccount(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	// Claude Code rotated the live credential since ccswap wrote it.
	const rotated = `{"claudeAiOauth":{"accessToken":"tok-1-rotated","refreshToken":"r1"}}`
	if err := f.Creds.WriteActive(rotated); err != nil {
		t.Fatal(err)
	}

	if _, err := f.Switch(t.Context(), SwitchRequest{Target: "2"}); err != nil {
		t.Fatal(err)
	}

	stored, _ := f.Creds.ReadAccount("1", "one@example.com")
	if stored != rotated {
		t.Errorf("slot 1's credential = %q, want the rotated live one", stored)
	}
}

// A slot whose stored credential is byte-identical to the live one needs no
// credential write; only its config backup is refreshed.
func TestAnUnchangedCredentialRefreshesOnlyTheConfig(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	// The live bytes already ARE slot 1's stored backup.
	stored, _ := f.Creds.ReadAccount("1", "one@example.com")
	if err := f.Creds.WriteActive(stored); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.Paths.GlobalConfigPath(), []byte(
		`{"oauthAccount":{"emailAddress":"one@example.com"},"newProject":true}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := f.Switch(t.Context(), SwitchRequest{Target: "2"}); err != nil {
		t.Fatal(err)
	}
	if config := f.ReadAccountConfig("1", "one@example.com"); !strings.Contains(config, "newProject") {
		t.Errorf("slot 1's config backup was not refreshed: %q", config)
	}
}

// Claude Code empties the token fields in place when a refresh is rejected.
// Writing that blob would replace the slot's only surviving refresh token with
// empty strings — the exact destruction observed in the field.
func TestWipedTokensAreNeverWrittenIntoASlot(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	before, _ := f.Creds.ReadAccount("1", "one@example.com")
	// The wrapper and metadata survive; the tokens are emptied.
	if err := f.Creds.WriteActive(
		`{"claudeAiOauth":{"accessToken":"","refreshToken":"","scopes":["user:inference"]}}`); err != nil {
		t.Fatal(err)
	}

	got, err := f.Switch(t.Context(), SwitchRequest{Target: "2"})
	if err != nil {
		t.Fatal(err)
	}

	after, _ := f.Creds.ReadAccount("1", "one@example.com")
	if after != before {
		t.Errorf("slot 1's credential was replaced by the wiped blob: %q", after)
	}
	if !mentions(got.Warnings, "wiped") {
		t.Errorf("warnings = %v, want one naming the wipe", got.Warnings)
	}
}

// Another account's credential goes into no slot, and is never destroyed:
// identity proves ownership, not which generation is fresher.
func TestAForeignCredentialIsPreservedNotFiled(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	// The live credential is slot 2's, while the config still names slot 1.
	const foreign = `{"claudeAiOauth":{"accessToken":"tok-2-live","refreshToken":"r2-live"}}`
	if err := f.Creds.WriteActive(foreign); err != nil {
		t.Fatal(err)
	}
	f.Oracle = &fakeOracle{identity: &claudeapi.Identity{UUID: "acct-2", Email: "two@example.com"}}
	before, _ := f.Creds.ReadAccount("1", "one@example.com")

	got, err := f.Switch(t.Context(), SwitchRequest{Target: "2"})
	if err != nil {
		t.Fatal(err)
	}

	// Slot 1 keeps its own credential.
	after, _ := f.Creds.ReadAccount("1", "one@example.com")
	if after != before {
		t.Errorf("slot 1's credential was poisoned with another account's bytes: %q", after)
	}
	// And the foreign bytes survive somewhere.
	entries, verdict := f.Creds.ListUnclaimed()
	if verdict != "ok" || len(entries) != 1 {
		t.Fatalf("stash = %v (%s), want exactly one preserved credential", entries, verdict)
	}
	for id, entry := range entries {
		value, unreadable := f.Creds.ReadUnclaimed(id)
		if unreadable || value != foreign {
			t.Errorf("stashed value = %q (unreadable=%v), want the foreign credential", value, unreadable)
		}
		if entry.Reason != string(KindForeign) {
			t.Errorf("reason = %q, want %q", entry.Reason, KindForeign)
		}
		if entry.ConfigSlot != "1" {
			t.Errorf("configSlot = %q, want the slot the config named", entry.ConfigSlot)
		}
		if entry.Fingerprint != claudeapi.Fingerprint(foreign) {
			t.Errorf("fingerprint = %q", entry.Fingerprint)
		}
	}
	if !mentions(got.Warnings, "ownership mismatch") {
		t.Errorf("warnings = %v, want one naming the mismatch", got.Warnings)
	}
}

// When the owning slot already holds this lineage there is nothing to preserve
// and nothing that may be written.
func TestAForeignCredentialAlreadySyncedIsNotStashed(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	// Slot 2's stored credential IS the live one.
	stored, _ := f.Creds.ReadAccount("2", "two@example.com")
	if err := f.Creds.WriteActive(stored); err != nil {
		t.Fatal(err)
	}
	f.Oracle = &fakeOracle{identity: &claudeapi.Identity{UUID: "acct-2", Email: "two@example.com"}}

	got, err := f.Switch(t.Context(), SwitchRequest{Target: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if entries, _ := f.Creds.ListUnclaimed(); len(entries) != 0 {
		t.Errorf("stash = %v, want nothing preserved", entries)
	}
	if !mentions(got.Warnings, "already matches") {
		t.Errorf("warnings = %v", got.Warnings)
	}
}

// Ownership could not be established, so the backup falls open: most such
// divergences are the account's own rotation, and skipping the backup would
// leave the slot holding a consumed token.
func TestAnUnresolvedCredentialFallsOpenAndIsBackedUp(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	const rotated = `{"claudeAiOauth":{"accessToken":"a2","refreshToken":"r2"}}`
	if err := f.Creds.WriteActive(rotated); err != nil {
		t.Fatal(err)
	}
	// The lookup does not resolve.
	f.Oracle = &fakeOracle{}

	got, err := f.Switch(t.Context(), SwitchRequest{Target: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if stored, _ := f.Creds.ReadAccount("1", "one@example.com"); stored != rotated {
		t.Errorf("slot 1's credential = %q, want the pre-taxonomy backup", stored)
	}
	// Logged, not warned: it is indistinguishable from a legitimate rotation,
	// so a warning would cry wolf on every ordinary switch.
	if len(got.Warnings) != 0 {
		t.Errorf("warnings = %v, want none for an ordinary rotation", got.Warnings)
	}
}

// A lineage an earlier probe already condemned must not be filed just because
// this pass's lookup failed — this switch may BE the repair that verdict
// triggered.
func TestAKnownForeignLineageIsPreservedDespiteALookupFailure(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	const foreign = `{"claudeAiOauth":{"accessToken":"a9","refreshToken":"r9"}}`
	if err := f.Creds.WriteActive(foreign); err != nil {
		t.Fatal(err)
	}
	f.Oracle = &fakeOracle{} // the lookup fails
	before, _ := f.Creds.ReadAccount("1", "one@example.com")

	condemned := claudeapi.Fingerprint(foreign)
	got, err := f.Switch(t.Context(), SwitchRequest{
		Target:    "2",
		Condemned: func(fp string) bool { return fp == condemned },
	})
	if err != nil {
		t.Fatal(err)
	}
	if after, _ := f.Creds.ReadAccount("1", "one@example.com"); after != before {
		t.Error("a lineage already proven foreign was filed into the slot anyway")
	}
	if entries, _ := f.Creds.ListUnclaimed(); len(entries) != 1 {
		t.Errorf("stash = %v, want the condemned lineage preserved", entries)
	}
	if !mentions(got.Warnings, "previously identified") {
		t.Errorf("warnings = %v", got.Warnings)
	}
}

// A Keychain timeout answers "" rather than failing. Writing that over the
// departing account's backup would destroy its stored credential.
func TestAnEmptyLiveCredentialRefusesTheSwitch(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	before, _ := f.Creds.ReadAccount("1", "one@example.com")
	if err := os.Remove(f.Paths.CredentialsPath()); err != nil {
		t.Fatal(err)
	}

	_, err := f.Switch(t.Context(), SwitchRequest{Target: "2"})
	wantErr(t, err, "reads as empty", "refusing to overwrite its backup")

	if after, _ := f.Creds.ReadAccount("1", "one@example.com"); after != before {
		t.Error("the departing account's backup was disturbed")
	}
	// And the switch did not land.
	if num, _ := f.roster().Active(); num != "1" {
		t.Errorf("Active = %q, want the switch not to have landed", num)
	}
}

// A target with no usable backup must fail while the live login is still
// intact.
func TestSwitchingToAnUnusableTarget(t *testing.T) {
	tests := []struct {
		name    string
		break_  func(*fixture)
		wantErr []string
	}{
		{
			name:    "no stored credential",
			break_:  func(f *fixture) { _ = f.Creds.DeleteAccount("2", "two@example.com") },
			wantErr: []string{"no stored credentials", "ccswap add --slot 2"},
		},
		{
			name: "no stored config",
			break_: func(f *fixture) {
				_ = os.Remove(f.ConfigBackupPath("2", "two@example.com"))
			},
			wantErr: []string{"no stored config backup"},
		},
		{
			name: "a stored config with no identity block",
			break_: func(f *fixture) {
				if err := f.WriteAccountConfig("2", "two@example.com", `{"projects":{}}`); err != nil {
					f.t.Fatal(err)
				}
			},
			wantErr: []string{"carries no account identity"},
		},
		{
			name: "a stored config that is not JSON",
			break_: func(f *fixture) {
				if err := f.WriteAccountConfig("2", "two@example.com", `{oops`); err != nil {
					f.t.Fatal(err)
				}
			},
			wantErr: []string{"not a JSON object"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.twoAccounts()
			liveBefore := f.activeCreds()
			tt.break_(f)

			_, err := f.Switch(t.Context(), SwitchRequest{Target: "2"})
			wantErr(t, err, tt.wantErr...)

			// The live login survives a failed switch untouched.
			if f.activeCreds() != liveBefore {
				t.Errorf("the live credential changed on a failed switch: %q", f.activeCreds())
			}
			if num, _ := f.roster().Active(); num != "1" {
				t.Errorf("Active = %q, want slot 1", num)
			}
		})
	}
}

func TestSwitchingToASlotThatDoesNotExist(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	_, err := f.Switch(t.Context(), SwitchRequest{Target: "9"})
	wantErr(t, err, "account 9 does not exist")
}

// A fresh machine has no live login to back up, so activation writes the stored
// credential directly.
func TestActivatingOnAMachineWithNoLiveLogin(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	f.clearLiveIdentity()
	if err := os.Remove(f.Paths.CredentialsPath()); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}

	got, err := f.Switch(t.Context(), SwitchRequest{Target: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Activated || got.From != nil {
		t.Errorf("outcome = %+v, want a direct activation with no departing account", got)
	}
	if !strings.Contains(f.activeCreds(), "tok-2") {
		t.Errorf("active credential = %q", f.activeCreds())
	}
	// With no live config to splice, the target's stored config is written whole.
	account := f.liveConfig()["oauthAccount"].(map[string]any)
	if account["emailAddress"] != "two@example.com" {
		t.Errorf("oauthAccount = %v", account)
	}
}

// An unmanaged live login is a real departure with no slot to name — and its
// credential is the only copy anywhere, so it must be preserved.
func TestActivatingOverAnUnmanagedLiveLogin(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	f.setLiveIdentity("stranger@example.com", "", "", "acct-stranger")
	const unmanaged = `{"claudeAiOauth":{"accessToken":"stranger","refreshToken":"rs"}}`
	if err := f.Creds.WriteActive(unmanaged); err != nil {
		t.Fatal(err)
	}

	got, err := f.Switch(t.Context(), SwitchRequest{Target: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Activated {
		t.Error("an unmanaged live login did not take the direct path")
	}
	if got.From == nil || got.From.Number != "" || got.From.Email != "stranger@example.com" {
		t.Errorf("From = %+v, want an unnumbered reference to the unmanaged account", got.From)
	}

	entries, _ := f.Creds.ListUnclaimed()
	if len(entries) != 1 {
		t.Fatalf("stash = %v, want the unmanaged credential preserved", entries)
	}
	for id, entry := range entries {
		if value, _ := f.Creds.ReadUnclaimed(id); value != unmanaged {
			t.Errorf("stashed value = %q", value)
		}
		if entry.ConfigSlot != "unmanaged" {
			t.Errorf("configSlot = %q, want %q", entry.ConfigSlot, "unmanaged")
		}
	}
}

// Force rewrites the live login from the stored backup without backing the live
// one up into a slot — but still preserves it, in case the "stale" login is
// actually the fresher generation.
func TestForceActivationStillPreservesTheLiveCredential(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	const live = `{"claudeAiOauth":{"accessToken":"maybe-fresher","refreshToken":"rf"}}`
	if err := f.Creds.WriteActive(live); err != nil {
		t.Fatal(err)
	}
	before, _ := f.Creds.ReadAccount("1", "one@example.com")

	got, err := f.Switch(t.Context(), SwitchRequest{Target: "2", Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Activated {
		t.Error("force did not take the direct path")
	}
	// The departing slot's backup is untouched — that is what force means.
	if after, _ := f.Creds.ReadAccount("1", "one@example.com"); after != before {
		t.Error("force wrote the live credential into the departing slot")
	}
	if entries, _ := f.Creds.ListUnclaimed(); len(entries) != 1 {
		t.Errorf("stash = %v, want the replaced login preserved", entries)
	}
}

// Force never resolves ownership: it explicitly rewrites the live login, so the
// lookup has nothing to decide.
func TestForceActivationSpendsNoLookup(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	oracle := f.resolving("acct-1", "one@example.com", "")

	if _, err := f.Switch(t.Context(), SwitchRequest{Target: "2", Force: true}); err != nil {
		t.Fatal(err)
	}
	if oracle.calls != 0 {
		t.Errorf("the oracle was consulted %d times under --force", oracle.calls)
	}
}

// Agreement between the live credential and the slot's backup needs no network
// at all.
func TestNoLookupWhenProvenanceIsEstablishedLocally(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	stored, _ := f.Creds.ReadAccount("1", "one@example.com")
	if err := f.Creds.WriteActive(stored); err != nil {
		t.Fatal(err)
	}
	oracle := f.resolving("acct-1", "one@example.com", "")

	if _, err := f.Switch(t.Context(), SwitchRequest{Target: "2"}); err != nil {
		t.Fatal(err)
	}
	if oracle.calls != 0 {
		t.Errorf("the oracle was consulted %d times for bytes that already matched", oracle.calls)
	}
}

// The machine-shared integrations are frozen in the slot at backup time and may
// hold rotated-out tokens; the live copies are by definition current. Every
// other stored field travels with the slot.
func TestActivationComposesSharedFieldsFromTheLiveCredential(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	// The slot carries a stale shared field and its own account-bound one.
	if err := f.Creds.WriteAccount("2", "two@example.com",
		`{"claudeAiOauth":{"accessToken":"tok-2"},"mcpOAuth":{"gen":"stale"},"trustedDeviceToken":"slot-2-device"}`); err != nil {
		t.Fatal(err)
	}
	if err := f.Creds.WriteActive(
		`{"claudeAiOauth":{"accessToken":"tok-1"},"mcpOAuth":{"gen":"current"},"trustedDeviceToken":"slot-1-device"}`); err != nil {
		t.Fatal(err)
	}

	if _, err := f.Switch(t.Context(), SwitchRequest{Target: "2"}); err != nil {
		t.Fatal(err)
	}

	var live map[string]any
	if err := json.Unmarshal([]byte(f.activeCreds()), &live); err != nil {
		t.Fatal(err)
	}
	// The shared field came from the live credential.
	if got := live["mcpOAuth"].(map[string]any)["gen"]; got != "current" {
		t.Errorf("mcpOAuth = %v, want the live generation", live["mcpOAuth"])
	}
	// The account-bound one came from the slot — carrying slot 1's across would
	// present one account's device token under another.
	if live["trustedDeviceToken"] != "slot-2-device" {
		t.Errorf("trustedDeviceToken = %v, want the target slot's", live["trustedDeviceToken"])
	}
}

// An unreadable live config is copied aside under a name the user is told
// about, and the switch still lands.
func TestATornLiveConfigIsSalvagedNotDestroyed(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	const torn = `{"oauthAccount":{"emailAddress":"one@exam`
	if err := os.WriteFile(f.Paths.GlobalConfigPath(), []byte(torn), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := f.Switch(t.Context(), SwitchRequest{Target: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if !mentions(got.Warnings, "could not be parsed") {
		t.Errorf("warnings = %v, want one naming the salvage", got.Warnings)
	}

	// The torn bytes survive next to the config.
	configDir := filepath.Dir(f.Paths.GlobalConfigPath())
	names, err := os.ReadDir(configDir)
	if err != nil {
		t.Fatal(err)
	}
	var salvaged bool
	for _, name := range names {
		if !strings.Contains(name.Name(), ".unreadable-") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(configDir, name.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if string(data) == torn {
			salvaged = true
		}
		info, err := name.Info()
		if err != nil {
			t.Fatal(err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("the salvage copy's mode is %o, want 0600 — it may hold a secret", perm)
		}
	}
	if !salvaged {
		t.Errorf("the torn config's bytes were not preserved; directory holds %v", names)
	}
	// And the switch landed.
	if num, _ := f.roster().Active(); num != "2" {
		t.Errorf("Active = %q, want the switch to have landed", num)
	}
}

// A valid but empty config loses nothing by being spliced, and telling the user
// it "could not be parsed" would be a lie.
func TestAnEmptyLiveConfigIsSplicedNotSalvaged(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	if err := os.WriteFile(f.Paths.GlobalConfigPath(), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := f.Switch(t.Context(), SwitchRequest{Target: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if mentions(got.Warnings, "could not be parsed") {
		t.Errorf("an empty but valid config was reported as unparseable: %v", got.Warnings)
	}
	account := f.liveConfig()["oauthAccount"].(map[string]any)
	if account["emailAddress"] != "two@example.com" {
		t.Errorf("oauthAccount = %v", account)
	}
}

// The config's parent is the user's home directory. Hardening it would lock the
// user out of their own home.
func TestSwitchDoesNotHardenTheHomeDirectory(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	// The config's parent is the user's home directory itself.
	home := filepath.Dir(f.Paths.GlobalConfigPath())
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := f.Switch(t.Context(), SwitchRequest{Target: "2"}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("the config's parent directory was chmod'ed to %o", perm)
	}
}

// A lookup that resolves to the departing slot's own uuid is its own rotation,
// however far the bytes moved.
func TestAResolvedOwnRotationIsBackedUpNormally(t *testing.T) {
	f := newFixture(t)
	f.twoAccounts()
	const rotated = `{"claudeAiOauth":{"accessToken":"a9","refreshToken":"r9"}}`
	if err := f.Creds.WriteActive(rotated); err != nil {
		t.Fatal(err)
	}
	f.Oracle = &fakeOracle{identity: &claudeapi.Identity{UUID: "acct-1", Email: "one@example.com"}}

	got, err := f.Switch(t.Context(), SwitchRequest{Target: "2"})
	if err != nil {
		t.Fatal(err)
	}
	if stored, _ := f.Creds.ReadAccount("1", "one@example.com"); stored != rotated {
		t.Errorf("slot 1's credential = %q, want the rotated one", stored)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("warnings = %v, want none for the account's own rotation", got.Warnings)
	}
}

// A slot with no recorded uuid gets one backfilled while the roster is being
// rewritten anyway — an add-token placeholder learning its real identity.
func TestAnOwnRotationBackfillsAMissingSlotUUID(t *testing.T) {
	f := newFixture(t)
	roster := f.seedAccounts(map[string]*Account{
		// No uuid: an add-token placeholder.
		"1": {Email: "one@example.com"},
		"2": {Email: "two@example.com", UUID: "acct-2"},
	})
	roster.SetActive("1", f.now)
	f.seedRoster(roster)
	f.setLiveIdentity("one@example.com", "", "", "")
	if err := f.Creds.WriteActive(`{"claudeAiOauth":{"accessToken":"a9","refreshToken":"r9"}}`); err != nil {
		t.Fatal(err)
	}
	f.Oracle = &fakeOracle{identity: &claudeapi.Identity{UUID: "acct-discovered", Email: "one@example.com"}}

	if _, err := f.Switch(t.Context(), SwitchRequest{Target: "2"}); err != nil {
		t.Fatal(err)
	}
	if got := f.roster().Accounts["1"].UUID; got != "acct-discovered" {
		t.Errorf("slot 1's uuid = %q, want it backfilled", got)
	}
}

func mentions(warnings []string, fragment string) bool {
	for _, w := range warnings {
		if strings.Contains(w, fragment) {
			return true
		}
	}
	return false
}
