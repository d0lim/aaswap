package transfer

import (
	"context"
	json "encoding/json/v2"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/keychain"
	"github.com/d0lim/aaswap/internal/paths"
	"github.com/d0lim/aaswap/internal/platform"
	"github.com/d0lim/aaswap/internal/settings"
	"github.com/d0lim/aaswap/internal/swap"
	"github.com/d0lim/aaswap/internal/usagestore"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

var testNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

type refusingKeychain struct{}

func (refusingKeychain) Run(context.Context, []string, string) (keychain.Result, error) {
	return keychain.Result{}, os.ErrNotExist
}

type fixture struct {
	*swap.Switcher
	t    *testing.T
	home string
	now  time.Time
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, ".local", "share", paths.BackupDirName)
	resolver := paths.New(home, platform.Linux)
	if err := os.MkdirAll(resolver.ClaudeConfigHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	f := &fixture{t: t, home: home, now: testNow}
	f.Switcher = &swap.Switcher{
		FetchStagger: time.Millisecond,
		Paths:        resolver,
		Creds:        credstore.NewForProvider(resolver, root, keychain.NewWithRunner(refusingKeychain{}, 0), swap.ProviderClaude),
		Usage:        usagestore.New(resolver.CacheDir()),
		Settings:     settings.Defaults(),
	}
	f.SetClock(func() time.Time { return f.now })
	return f
}

const oauthCreds = `{"claudeAiOauth":{"accessToken":"a","refreshToken":"r"},"trustedDeviceToken":"device","mcpOAuth":{"x":1}}`
const fullConfig = `{"oauthAccount":{"emailAddress":"one@example.com","accountUuid":"acct-1"},"userID":"u-1","projects":{"/w":{}}}`

func (f *fixture) seed(num, email, credentials, config string) {
	f.t.Helper()
	roster, err := f.RosterOrEmpty()
	if err != nil {
		f.t.Fatal(err)
	}
	roster.Insert(num, &swap.Account{
		Email: email, UUID: "acct-" + num, Added: swap.Timestamp(f.now),
	})
	if err := f.WriteRoster(roster); err != nil {
		f.t.Fatal(err)
	}
	if err := f.Creds.WriteAccount(num, email, credentials); err != nil {
		f.t.Fatal(err)
	}
	if err := f.WriteAccountConfig(num, email, config); err != nil {
		f.t.Fatal(err)
	}
}

func TestExportSlimsByDefault(t *testing.T) {
	f := newFixture(t)
	f.seed("1", "one@example.com", oauthCreds, fullConfig)

	result, err := Export(f.Switcher, ExportRequest{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Envelope.Accounts) != 1 {
		t.Fatalf("accounts = %+v", result.Envelope.Accounts)
	}
	entry := result.Envelope.Accounts[0]
	if entry.Name != "1" || entry.Email != "one@example.com" || entry.UUID != "acct-1" {
		t.Errorf("entry = %+v", entry)
	}

	// Only what a switch consumes: the source machine's identifiers stay home.
	config := decodeJSONObject(t, entry.Config)
	if _, present := config["oauthAccount"]; !present {
		t.Errorf("config = %v, want the identity block", config)
	}
	for _, leaked := range []string{"userID", "projects"} {
		if _, present := config[leaked]; present {
			t.Errorf("%q was carried across machines: %v", leaked, config)
		}
	}

	// Only the account's own login: the siblings are machine-shared or
	// device-bound, and both are secret surface with no cross-machine value.
	credentials := decodeJSONObject(t, entry.Credentials)
	if _, present := credentials["claudeAiOauth"]; !present {
		t.Errorf("credentials = %v, want the login", credentials)
	}
	for _, leaked := range []string{"trustedDeviceToken", "mcpOAuth"} {
		if _, present := credentials[leaked]; present {
			t.Errorf("%q was carried across machines: %v", leaked, credentials)
		}
	}
}

// A full export is for backing up a machine to itself, where the source
// identifiers are the point rather than a leak.
func TestFullExportKeepsEverything(t *testing.T) {
	f := newFixture(t)
	f.seed("1", "one@example.com", oauthCreds, fullConfig)

	result, err := Export(f.Switcher, ExportRequest{Full: true}, "test")
	if err != nil {
		t.Fatal(err)
	}
	entry := result.Envelope.Accounts[0]
	config := decodeJSONObject(t, entry.Config)
	for _, kept := range []string{"userID", "projects"} {
		if _, present := config[kept]; !present {
			t.Errorf("a full export dropped %q: %v", kept, config)
		}
	}
	credentials := decodeJSONObject(t, entry.Credentials)
	if _, present := credentials["trustedDeviceToken"]; !present {
		t.Errorf("a full export dropped a credential sibling: %v", credentials)
	}
}

// The live store holds fresher tokens than a backup for whichever account is
// active.
func TestTheActiveAccountIsExportedLive(t *testing.T) {
	f := newFixture(t)
	f.seed("1", "one@example.com", `{"claudeAiOauth":{"accessToken":"stale"}}`, fullConfig)
	if err := os.WriteFile(f.Paths.GlobalConfigPath(), []byte(fullConfig), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.Creds.WriteActive(`{"claudeAiOauth":{"accessToken":"fresh"}}`); err != nil {
		t.Fatal(err)
	}

	result, err := Export(f.Switcher, ExportRequest{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	credentials := string(result.Envelope.Accounts[0].Credentials)
	if !strings.Contains(credentials, "fresh") {
		t.Errorf("the active account was exported from its backup: %s", credentials)
	}
}

// One damaged slot must not poison the whole backup — but naming an account
// makes its missing backup a failure, because that is the one the user asked
// for.
func TestADamagedSlot(t *testing.T) {
	t.Run("is skipped when exporting everything", func(t *testing.T) {
		f := newFixture(t)
		f.seed("1", "one@example.com", oauthCreds, fullConfig)
		f.seed("2", "two@example.com", "", "")

		result, err := Export(f.Switcher, ExportRequest{}, "test")
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Envelope.Accounts) != 1 {
			t.Errorf("accounts = %+v, want only the healthy one", result.Envelope.Accounts)
		}
		if len(result.Skipped) != 1 || !strings.Contains(result.Skipped[0], "two@example.com") {
			t.Errorf("skipped = %v, want the damaged slot named", result.Skipped)
		}
	})

	t.Run("fails when it is the account asked for", func(t *testing.T) {
		f := newFixture(t)
		f.seed("1", "one@example.com", oauthCreds, fullConfig)
		f.seed("2", "two@example.com", "", "")

		_, err := Export(f.Switcher, ExportRequest{Account: "2"}, "test")
		if err == nil {
			t.Fatal("exporting a damaged slot by name succeeded")
		}
		if !strings.Contains(err.Error(), "no stored credential") {
			t.Errorf("error = %v", err)
		}
	})
}

// Pointing at an account the import cannot find is worse than saying nothing.
func TestTheActiveNumberIsCarriedOnlyWhenPresent(t *testing.T) {
	f := newFixture(t)
	f.seed("1", "one@example.com", oauthCreds, fullConfig)
	f.seed("2", "two@example.com", oauthCreds, fullConfig)
	roster, err := f.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	roster.SetActive("2")
	if err := f.WriteRoster(roster); err != nil {
		t.Fatal(err)
	}

	result, err := Export(f.Switcher, ExportRequest{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Envelope.ActiveAccount == "" {
		t.Errorf("activeAccount = %q, want the active name", result.Envelope.ActiveAccount)
	}

	// Exporting only slot 1 leaves the active account out of the payload.
	result, err = Export(f.Switcher, ExportRequest{Account: "1"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.Envelope.ActiveAccount != "" {
		t.Errorf("activeAccountNumber = %v, want nothing — that slot is not in the payload",
			result.Envelope.ActiveAccount)
	}
}

// An API-key credential is a raw key, not an object.
func TestAPIKeyAccountsRoundTrip(t *testing.T) {
	f := newFixture(t)
	f.seed("1", "key@token.local", "sk-ant-api03-abcdef", `{"oauthAccount":{"emailAddress":"key@token.local"}}`)

	result, err := Export(f.Switcher, ExportRequest{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	entry := result.Envelope.Accounts[0]
	if entry.Kind != "api_key" {
		t.Errorf("kind = %q, want it tagged", entry.Kind)
	}
	var asString string
	if err := json.Unmarshal(entry.Credentials, &asString); err != nil {
		t.Fatalf("the credential is not a JSON string: %s", entry.Credentials)
	}
	if asString != "sk-ant-api03-abcdef" {
		t.Errorf("credential = %q", asString)
	}

	// And it comes back as itself.
	into := newFixture(t)
	imported, err := Import(into.Switcher, encode(t, result.Envelope), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(imported.Accounts) != 1 {
		t.Fatalf("imported = %+v", imported.Accounts)
	}
	stored, _ := into.Creds.ReadAccount(imported.Accounts[0].Name, "key@token.local")
	if stored != "sk-ant-api03-abcdef" {
		t.Errorf("the restored credential is %q", stored)
	}
	roster, err := into.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if roster.Accounts[imported.Accounts[0].Name].AuthKind() != swap.KindAPIKey {
		t.Error("the restored account lost its kind")
	}
}

func TestExportImportRoundTrip(t *testing.T) {
	from := newFixture(t)
	from.seed("work", "one@example.com", oauthCreds, fullConfig)
	from.seed("spare", "three@example.com", oauthCreds,
		`{"oauthAccount":{"emailAddress":"three@example.com"}}`)

	result, err := Export(from.Switcher, ExportRequest{}, "test")
	if err != nil {
		t.Fatal(err)
	}

	to := newFixture(t)
	imported, err := Import(to.Switcher, encode(t, result.Envelope), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(imported.Accounts) != 2 {
		t.Fatalf("imported = %+v", imported.Accounts)
	}

	roster, err := to.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	// The names are kept, so a shell history full of `aaswap switch work`
	// keeps working on the new machine.
	if roster.Accounts["work"] == nil || roster.Accounts["spare"] == nil {
		t.Fatalf("roster = %+v, want the exported names kept", roster.Accounts)
	}
	if roster.Accounts["work"].Email != "one@example.com" || roster.Accounts["spare"].Email != "three@example.com" {
		t.Errorf("roster = %+v", roster.Accounts)
	}
	if value, _ := to.Creds.ReadAccount("work", "one@example.com"); !strings.Contains(value, "claudeAiOauth") {
		t.Errorf("the restored credential is %q", value)
	}
	if config := to.ReadAccountConfig("work", "one@example.com"); !strings.Contains(config, "one@example.com") {
		t.Errorf("the restored config is %q", config)
	}
}

// A malformed account late in the file must not leave the earlier ones
// half-imported.
func TestValidationHappensBeforeAnyWrite(t *testing.T) {
	from := newFixture(t)
	from.seed("1", "one@example.com", oauthCreds, fullConfig)
	result, err := Export(from.Switcher, ExportRequest{}, "test")
	if err != nil {
		t.Fatal(err)
	}
	// A second account that will not validate.
	result.Envelope.Accounts = append(result.Envelope.Accounts, ExportedAccount{
		Name: "two", Email: "not an email", Config: result.Envelope.Accounts[0].Config,
		Credentials: result.Envelope.Accounts[0].Credentials,
	})

	to := newFixture(t)
	if _, err := Import(to.Switcher, encode(t, result.Envelope), false); err == nil {
		t.Fatal("an invalid account was imported")
	}
	roster, err := to.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Accounts) != 0 {
		t.Errorf("a failed import left %v behind", roster.Accounts)
	}
}

// An envelope is a file from another machine: an address flows into a filename
// and has to be constrained before any path is built from it.
func TestImportRejections(t *testing.T) {
	valid := ExportedAccount{
		Name: "n1", Email: "one@example.com",
		Config:      []byte(`{"oauthAccount":{"emailAddress":"one@example.com"}}`),
		Credentials: []byte(`{"claudeAiOauth":{"accessToken":"a"}}`),
	}

	tests := []struct {
		name     string
		envelope Envelope
		wantErr  string
	}{
		{
			name:     "a wrong version",
			envelope: Envelope{Version: 99, Accounts: []ExportedAccount{valid}},
			wantErr:  "unsupported export version",
		},
		{
			name:     "an encrypted payload",
			envelope: Envelope{Version: 1, Encrypted: true, Accounts: []ExportedAccount{valid}},
			wantErr:  "Decrypt it before importing",
		},
		{
			name:     "no accounts",
			envelope: Envelope{Version: 1},
			wantErr:  "no accounts to import",
		},
		{
			name: "a path traversal in the address",
			envelope: Envelope{Version: 1, Accounts: []ExportedAccount{
				withEmail(valid, "../../etc/passwd@example.com"),
			}},
			wantErr: "invalid or missing email",
		},
		{
			name: "a separator in the address",
			envelope: Envelope{Version: 1, Accounts: []ExportedAccount{
				withEmail(valid, "a/b@example.com"),
			}},
			wantErr: "invalid or missing email",
		},
		{
			name: "no address at all",
			envelope: Envelope{Version: 1, Accounts: []ExportedAccount{
				withEmail(valid, ""),
			}},
			wantErr: "invalid or missing email",
		},
		{
			name: "a config with no identity",
			envelope: Envelope{Version: 1, Accounts: []ExportedAccount{
				func() ExportedAccount { e := valid; e.Config = []byte(`{"projects":{}}`); return e }(),
			}},
			wantErr: "carries no account identity",
		},
		{
			name: "a credential that is not an object",
			envelope: Envelope{Version: 1, Accounts: []ExportedAccount{
				func() ExportedAccount { e := valid; e.Credentials = []byte(`[1,2]`); return e }(),
			}},
			wantErr: "must be a JSON object",
		},
		{
			name: "a bare string that is not an API key",
			envelope: Envelope{Version: 1, Accounts: []ExportedAccount{
				func() ExportedAccount { e := valid; e.Credentials = []byte(`"just a string"`); return e }(),
			}},
			wantErr: "not a managed API key",
		},
		{
			name:     "the same account twice",
			envelope: Envelope{Version: 1, Accounts: []ExportedAccount{valid, valid}},
			wantErr:  "names one@example.com twice",
		},
		{
			name: "the same name twice",
			envelope: Envelope{Version: 1, Accounts: []ExportedAccount{
				func() ExportedAccount { e := valid; e.Name = "work"; return e }(),
				func() ExportedAccount {
					e := withEmail(valid, "two@example.com")
					e.Name = "work"
					return e
				}(),
			}},
			wantErr: "alias \"work\" twice",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			_, err := Import(f.Switcher, encode(t, tt.envelope), false)
			if err == nil {
				t.Fatal("the import was accepted")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}

	t.Run("a file that is not JSON", func(t *testing.T) {
		f := newFixture(t)
		if _, err := Import(f.Switcher, []byte("not json"), false); err == nil {
			t.Fatal("a non-JSON file was accepted")
		}
	})
}

// An existing healthy account is left alone unless --force says otherwise.
func TestImportSkipsExistingAccounts(t *testing.T) {
	f := newFixture(t)
	f.seed("1", "one@example.com", `{"claudeAiOauth":{"accessToken":"local"}}`, fullConfig)

	envelope := Envelope{Version: 1, Accounts: []ExportedAccount{{
		Name: "n1", Email: "one@example.com",
		Config:      []byte(fullConfig),
		Credentials: []byte(`{"claudeAiOauth":{"accessToken":"imported"}}`),
	}}}

	result, err := Import(f.Switcher, encode(t, envelope), false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Accounts[0].Outcome != Skipped {
		t.Errorf("outcome = %q, want %q", result.Accounts[0].Outcome, Skipped)
	}
	if value, _ := f.Creds.ReadAccount("1", "one@example.com"); !strings.Contains(value, "local") {
		t.Errorf("the local credential was replaced: %q", value)
	}

	// With --force it is replaced.
	result, err = Import(f.Switcher, encode(t, envelope), true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Accounts[0].Outcome != Overwrote {
		t.Errorf("outcome = %q, want %q", result.Accounts[0].Outcome, Overwrote)
	}
	if value, _ := f.Creds.ReadAccount("1", "one@example.com"); !strings.Contains(value, "imported") {
		t.Errorf("the credential was not replaced: %q", value)
	}
}

// A plain import heals a quarantined slot: the verdict normally postdates the
// slot's last credential write, so the import is newer than what failed.
func TestAQuarantinedSlotIsHealedWithoutForce(t *testing.T) {
	f := newFixture(t)
	f.seed("1", "one@example.com", `{"claudeAiOauth":{"accessToken":"dead"}}`, fullConfig)
	stored, _ := f.Creds.ReadAccount("1", "one@example.com")
	ids := map[string]usagestore.Identity{"1": {Email: "one@example.com"}}
	if _, err := f.Usage.Record(map[string]usagestore.FetchRecord{
		"1": {Error: claudeapi.KindInvalidGrant, StruckFP: claudeapi.Fingerprint(stored)},
	}, ids, nil, nil); err != nil {
		t.Fatal(err)
	}

	envelope := Envelope{Version: 1, Accounts: []ExportedAccount{{
		Name: "n1", Email: "one@example.com",
		Config:      []byte(fullConfig),
		Credentials: []byte(`{"claudeAiOauth":{"accessToken":"fresh","refreshToken":"r"}}`),
	}}}
	result, err := Import(f.Switcher, encode(t, envelope), false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Accounts[0].Outcome != Replaced {
		t.Errorf("outcome = %q, want %q", result.Accounts[0].Outcome, Replaced)
	}
	if value, _ := f.Creds.ReadAccount("1", "one@example.com"); !strings.Contains(value, "fresh") {
		t.Errorf("the dead credential was not replaced: %q", value)
	}
	// And the quarantine is lifted, or the slot would never fetch to prove the
	// imported token good.
	if f.Usage.Entries(ids, nil)["1"].TokenDead("") {
		t.Error("the quarantine survived the import")
	}
}

// Forcing an import of the very generation a strike condemned must say so —
// silence would read as a clear that did not happen.
func TestForcingTheCondemnedGenerationSaysSo(t *testing.T) {
	f := newFixture(t)
	const condemned = `{"claudeAiOauth":{"accessToken":"a","refreshToken":"dead"}}`
	f.seed("1", "one@example.com", condemned, fullConfig)
	ids := map[string]usagestore.Identity{"1": {Email: "one@example.com"}}
	if _, err := f.Usage.Record(map[string]usagestore.FetchRecord{
		"1": {Error: claudeapi.KindInvalidGrant, StruckFP: claudeapi.Fingerprint(condemned)},
	}, ids, nil, nil); err != nil {
		t.Fatal(err)
	}

	envelope := Envelope{Version: 1, Accounts: []ExportedAccount{{
		Name: "n1", Email: "one@example.com",
		Config: []byte(fullConfig), Credentials: []byte(condemned),
	}}}
	result, err := Import(f.Switcher, encode(t, envelope), true)
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(result.Accounts[0].Notes, "\n")
	if !strings.Contains(notes, "same credential generation") {
		t.Errorf("notes = %q, want the re-import of the condemned generation named", notes)
	}
}

// An alias already held by a different local account is dropped rather than
// failing the transfer.
func TestACollidingNameIsDropped(t *testing.T) {
	f := newFixture(t)
	f.seed("work", "local@example.com", oauthCreds, fullConfig)

	envelope := Envelope{Version: 1, Accounts: []ExportedAccount{{
		Name: "two", Email: "imported@example.com",
		Config:      []byte(`{"oauthAccount":{"emailAddress":"imported@example.com"}}`),
		Credentials: []byte(`{"claudeAiOauth":{"accessToken":"a"}}`),
	}}}
	result, err := Import(f.Switcher, encode(t, envelope), false)
	if err != nil {
		t.Fatalf("a colliding name failed the whole transfer: %v", err)
	}

	roster, err := f.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	// The local account keeps the name; the import lands under another one
	// rather than overwriting an unrelated account.
	if roster.Accounts["work"].Email != "local@example.com" {
		t.Errorf("%q is now %s, want the local account to keep it",
			"work", roster.Accounts["work"].Email)
	}
	if got := result.Accounts[0].Name; got == "work" {
		t.Error("the import took a name that was already held")
	}
}

// An occupied name sends the import to a free one rather than overwriting an
// unrelated account.
func TestAnOccupiedNameGetsAnotherOne(t *testing.T) {
	f := newFixture(t)
	f.seed("1", "local@example.com", oauthCreds, fullConfig)

	envelope := Envelope{Version: 1, Accounts: []ExportedAccount{{
		Name: "n1", Email: "imported@example.com",
		Config:      []byte(`{"oauthAccount":{"emailAddress":"imported@example.com"}}`),
		Credentials: []byte(`{"claudeAiOauth":{"accessToken":"a"}}`),
	}}}
	result, err := Import(f.Switcher, encode(t, envelope), false)
	if err != nil {
		t.Fatal(err)
	}
	if result.Accounts[0].Name == "1" {
		t.Error("an unrelated account was overwritten")
	}
	roster, err := f.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if roster.Accounts["1"].Email != "local@example.com" {
		t.Errorf("slot 1 = %q", roster.Accounts["1"].Email)
	}
}

func encode(t *testing.T, envelope Envelope) []byte {
	t.Helper()
	data, err := json.Marshal(envelope, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func decodeJSONObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not a JSON object: %v\n%s", err, raw)
	}
	return out
}

func withEmail(entry ExportedAccount, email string) ExportedAccount {
	entry.Email = email
	return entry
}
