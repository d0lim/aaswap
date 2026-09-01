package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/jsonout"
	"github.com/d0lim/aaswap/internal/swap"
	"github.com/d0lim/aaswap/internal/usage"
	"github.com/spf13/cobra"

	"github.com/d0lim/aaswap/internal/testutil"
)

func TestListShowsEveryAccount(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	h.login("1", "one@example.com")
	h.measuring(map[string]*usage.Result{"1": measured(40), "2": measured(10)})

	if code := h.run("list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "1", "one@example.com", "2", "two@example.com", "40%", "10%")
}

func TestListOnAnEmptyStoreExplainsWhatToDo(t *testing.T) {
	h := newHarness(t)
	if code := h.run("list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "No accounts are managed yet", "aaswap login")
}

// The JSON surface is what scripts consume, so it must be exactly one object
// with nothing else on stdout.
func TestListJSONIsOneObject(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	h.login("1", "one@example.com")
	h.measuring(map[string]*usage.Result{"1": measured(40)})

	if code := h.run("list", "--json"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	payload := h.decodeJSON()
	if payload["schemaVersion"] != float64(jsonout.SchemaVersion) {
		t.Errorf("schemaVersion = %v", payload["schemaVersion"])
	}
	accounts := payload["accounts"].([]any)
	if len(accounts) != 1 {
		t.Fatalf("accounts = %v", accounts)
	}
	row := accounts[0].(map[string]any)
	if row["email"] != "one@example.com" || row["active"] != true {
		t.Errorf("row = %v", row)
	}
	// No prose leaked onto the machine-readable stream.
	if strings.Contains(h.stdout(), "Account 1:") {
		t.Errorf("human output leaked into the JSON stream:\n%s", h.stdout())
	}
}

func TestStatus(t *testing.T) {
	t.Run("no live login", func(t *testing.T) {
		h := newHarness(t)
		h.seed(map[string]string{"1": "one@example.com"})
		if code := h.run("status"); code != ExitOK {
			t.Fatalf("exit = %d: %s", code, h.stderr())
		}
		wantContains(t, h.stdout(), "no active Claude account")
	})

	t.Run("a managed login", func(t *testing.T) {
		h := newHarness(t)
		h.seed(map[string]string{"1": "one@example.com"})
		h.login("1", "one@example.com")
		h.measuring(map[string]*usage.Result{"1": measured(40)})
		if code := h.run("status"); code != ExitOK {
			t.Fatalf("exit = %d: %s", code, h.stderr())
		}
		wantContains(t, h.stdout(), "1", "one@example.com", "40%")
	})

	t.Run("an unmanaged login says how to store it", func(t *testing.T) {
		h := newHarness(t)
		h.seed(map[string]string{"1": "one@example.com"})
		h.login("9", "stranger@example.com")
		if code := h.run("status"); code != ExitOK {
			t.Fatalf("exit = %d: %s", code, h.stderr())
		}
		wantContains(t, h.stdout(), "stranger@example.com", "Not managed", "aaswap login")
	})

	t.Run("as JSON, an absent login is null", func(t *testing.T) {
		h := newHarness(t)
		if code := h.run("status", "--json"); code != ExitOK {
			t.Fatalf("exit = %d: %s", code, h.stderr())
		}
		payload := h.decodeJSON()
		if payload["active"] != nil {
			t.Errorf("active = %v, want null", payload["active"])
		}
	})
}

func TestSwitchToAnAccount(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	h.login("1", "one@example.com")

	if code := h.run("switch", "2"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "2", "two@example.com")
	if live := h.switcher.Creds.ReadActive().Value; !strings.Contains(live, "tok-2") {
		t.Errorf("the live credential is %q", live)
	}
}

// A bare switch rotates to the next account, wrapping.
func TestBareSwitchRotates(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com", "3": "three@example.com"})
	h.login("2", "two@example.com")

	if code := h.run("switch"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "3")

	h.login("3", "three@example.com")
	if code := h.run("switch"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "1")
}

func TestSwitchByAliasAndEmail(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	h.login("1", "one@example.com")
	if code := h.run("account", "rename", "2", "work"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}

	for _, identifier := range []string{"work", "two@example.com"} {
		h.login("1", "one@example.com")
		if code := h.run("switch", identifier); code != ExitOK {
			t.Fatalf("switch %q: exit = %d: %s", identifier, code, h.stderr())
		}
		wantContains(t, h.stdout(), "two@example.com")
	}
}

// The payload names both ends of the move, so a wrapper can log what changed.
func TestSwitchJSONCarriesBothSides(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	h.login("1", "one@example.com")

	if code := h.run("switch", "2", "--json"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	payload := h.decodeJSON()
	if payload["switched"] != true {
		t.Errorf("switched = %v", payload["switched"])
	}
	from := payload["from"].(map[string]any)
	if from["name"] != "1" || from["email"] != "one@example.com" {
		t.Errorf("from = %v", from)
	}
	to := payload["to"].(map[string]any)
	if to["name"] != "2" {
		t.Errorf("to = %v", to)
	}
}

// A handled failure is one machine-readable object too, so a consumer parses
// one shape and branches on `error`.
func TestAFailureInJSONModeIsAnEnvelope(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	h.login("1", "one@example.com")

	if code := h.run("switch", "9", "--json"); code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	payload := h.decodeJSON()
	if payload["schemaVersion"] != float64(jsonout.SchemaVersion) {
		t.Errorf("schemaVersion = %v", payload["schemaVersion"])
	}
	envelope := payload["error"].(map[string]any)
	if envelope["type"] != "AccountNotFoundError" {
		t.Errorf("type = %v", envelope["type"])
	}
	if !strings.Contains(envelope["message"].(string), "9") {
		t.Errorf("message = %v", envelope["message"])
	}
	// The prose channel stays empty in JSON mode.
	if h.stderr() != "" {
		t.Errorf("stderr = %q, want nothing in JSON mode", h.stderr())
	}
}

// The error type is a stable taxonomy name, not a Go type: renaming an internal
// type must not break a consumer's branch.
func TestErrorTypesAreTheTaxonomysNames(t *testing.T) {
	h := newHarness(t)
	if code := h.run("account", "remove", "nobody", "--json", "--yes"); code != ExitError {
		t.Fatalf("exit = %d", code)
	}
	envelope := h.decodeJSON()["error"].(map[string]any)
	if envelope["type"] != "ConfigError" {
		t.Errorf("type = %v, want the taxonomy's name", envelope["type"])
	}
}

func TestLoginAndRemove(t *testing.T) {
	h := newHarness(t)
	h.login("1", "one@example.com")

	if code := h.run("login"); code != ExitOK {
		t.Fatalf("add: exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Added", "one@example.com")

	if code := h.run("account", "remove", "one", "--yes"); code != ExitOK {
		t.Fatalf("remove: exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Removed", "one@example.com")

	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Accounts) != 0 {
		t.Errorf("the roster still holds %v", roster.Accounts)
	}
}

// Every irreversible command asks first, and a declined answer changes nothing.
func TestDestructiveCommandsAskFirst(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"remove", []string{"account", "remove", "1"}},
		{"remove --all", []string{"account", "remove", "--all"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.seed(map[string]string{"1": "one@example.com"})
			var asked string
			h.app.Confirm = func(prompt string) bool {
				asked = prompt
				return false
			}

			if code := h.run(tt.args...); code != ExitOK {
				t.Fatalf("exit = %d: %s", code, h.stderr())
			}
			if asked == "" {
				t.Error("nothing was asked before an irreversible command")
			}
			wantContains(t, h.stdout(), "Cancelled")

			roster, err := h.switcher.RosterOrEmpty()
			if err != nil {
				t.Fatal(err)
			}
			if len(roster.Accounts) != 1 {
				t.Error("a declined command changed the roster")
			}
		})
	}
}

// A caller with no way to ask gets a refusal, never a silent yes.
func TestNoWayToAskIsARefusal(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	h.app.In = nil

	if code := h.run("account", "remove", "--all"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Accounts) != 1 {
		t.Error("every account was removed with nobody to confirm it")
	}
}

func TestDisableAndEnable(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	h.login("1", "one@example.com")

	if code := h.run("account", "disable", "2"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Disabled", "2")

	// A disabled account is out of rotation but still an explicit target.
	if code := h.run("switch", "2"); code != ExitOK {
		t.Fatalf("switch to a disabled account: exit = %d: %s", code, h.stderr())
	}

	// Saying it twice reports the state rather than claiming an edit.
	if code := h.run("account", "disable", "2"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "already disabled")

	if code := h.run("account", "enable", "2"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Enabled", "back in the rotation")
}

// Disabling the last rotatable account leaves auto-switch nothing to pick.
func TestDisablingTheLastAccountWarns(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	h.login("1", "one@example.com")

	if code := h.run("account", "disable", "1"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "No accounts remain in rotation", "aaswap account enable")
}

func TestConfig(t *testing.T) {
	h := newHarness(t)

	if code := h.run("config"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "autoswitch.threshold", "(default)")

	if code := h.run("config", "set", "autoswitch.threshold", "80"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Set", "80")

	if code := h.run("config", "get", "autoswitch.threshold"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if strings.TrimSpace(h.stdout()) != "80" {
		t.Errorf("get printed %q, want just the value", h.stdout())
	}

	// Now that it is in the file, it is no longer marked as a default.
	if code := h.run("config", "list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	for line := range strings.SplitSeq(h.stdout(), "\n") {
		if strings.HasPrefix(line, "autoswitch.threshold") && strings.Contains(line, "(default)") {
			t.Errorf("an explicitly set key is still marked as a default: %q", line)
		}
	}

	if code := h.run("config", "unset", "autoswitch.threshold"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Unset", "(default)")
}

func TestConfigRejectsBadInput(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"an unknown key", []string{"config", "set", "autoswitch.nonsense", "1"}, "nonsense"},
		{"a value out of range", []string{"config", "set", "autoswitch.threshold", "500"}, "threshold"},
		{"a non-numeric value", []string{"config", "set", "autoswitch.threshold", "high"}, "threshold"},
		{"getting an unknown key", []string{"config", "get", "nope"}, "nope"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			if code := h.run(tt.args...); code != ExitError {
				t.Fatalf("exit = %d, want a rejection: %s", code, h.stdout())
			}
			wantContains(t, h.stderr(), tt.wantErr)
		})
	}
}

// Which file the settings came from is part of listing them, not a command of
// its own: one line is not worth a verb.
func TestConfigListNamesTheFile(t *testing.T) {
	h := newHarness(t)
	if code := h.run("config", "list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "settings.json")

	if code := h.run("config", "list", "--json"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	payload := h.decodeJSON()
	path, _ := payload["path"].(string)
	if !strings.Contains(path, "settings.json") {
		t.Errorf("path = %q, want the settings file", path)
	}
	if _, ok := payload["settings"].([]any); !ok {
		t.Errorf("the payload carries no settings: %v", payload)
	}
}

// The model setting decides which per-model weekly windows a listing counts.
//
// Display only. It used to steer the headroom comparison a rotation strategy
// made, and that comparison is gone — see docs/PROVIDERS.md.
func TestTheModelSettingReachesTheListing(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	h.login("1", "one@example.com")
	// Account-wide, slot 1 is comfortable. Its Fable window is spent.
	h.measuring(map[string]*usage.Result{
		"1": {
			FiveHour: &usage.Window{Pct: 10}, SevenDay: &usage.Window{Pct: 10},
			Scoped: []usage.Scoped{{Name: "Fable", Pct: 100}},
		},
		"2": {FiveHour: &usage.Window{Pct: 50}, SevenDay: &usage.Window{Pct: 50}},
	})

	if code := h.run("config", "set", "autoswitch.model", "Fable"); code != ExitOK {
		t.Fatalf("setting the model: exit = %d: %s", code, h.stderr())
	}
	if code := h.run("list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	// The spent per-model window has to be visible, or counting it changed
	// nothing a person can see.
	wantContains(t, h.stdout(), "Fable")
}

func TestUnclaimedListingAndPurge(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	h.login("1", "one@example.com")
	// A live credential that resolves to slot 2 but is a DIFFERENT generation
	// than slot 2's stored one, so it is neither slot's filed credential and
	// has nowhere to go but the stash.
	if err := h.switcher.Creds.WriteActive(
		`{"claudeAiOauth":{"accessToken":"tok-2-live","refreshToken":"r-2-live"}}`); err != nil {
		t.Fatal(err)
	}
	h.switcher.Oracle = staticOracle{uuid: "acct-2", email: "two@example.com"}

	if code := h.run("switch", "2"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}

	if code := h.run("account", "unclaimed"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "reason:", "the config named slot 1", "--purge")

	if code := h.run("account", "unclaimed", "--purge", "all", "--yes"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Dropped")

	if code := h.run("account", "unclaimed"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "No preserved credentials")
}

func TestUnclaimedOnAnEmptyStore(t *testing.T) {
	h := newHarness(t)
	if code := h.run("account", "unclaimed"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "No preserved credentials")
}

// No arguments is a request for help, not a failure.
func TestNoArgumentsPrintsHelp(t *testing.T) {
	h := newHarness(t)
	if code := h.run(); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Usage:", "Available Commands:")
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	if code := h.run("--version"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if strings.TrimSpace(h.stdout()) == "" {
		t.Error("--version printed nothing")
	}
}

// A wrong flag deserves usage; a failed operation does not.
func TestUsageIsShownOnlyForAMisusedCommand(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	h.login("1", "one@example.com")

	if code := h.run("switch", "9"); code != ExitError {
		t.Fatalf("exit = %d", code)
	}
	if strings.Contains(h.stderr(), "Usage:") {
		t.Errorf("a runtime failure printed the usage block:\n%s", h.stderr())
	}

	if code := h.run("switch", "--nonsense"); code != ExitError {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(h.stderr(), "unknown flag") {
		t.Errorf("a misused flag was not reported:\n%s", h.stderr())
	}
}

// The listing's token diagnostics are opt-in: they are noise for the ordinary
// case.
func TestTokenStatusIsOptIn(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	h.login("1", "one@example.com")
	h.measuring(map[string]*usage.Result{"1": measured(40)})

	if code := h.run("list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if strings.Contains(h.stdout(), "oauth:") {
		t.Errorf("token diagnostics appeared without being asked for:\n%s", h.stdout())
	}

	if code := h.run("list", "--token-status"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "oauth:", "refresh token yes")
}

// staticOracle answers the ownership question with a fixed identity.
type staticOracle struct{ uuid, email string }

func (o staticOracle) Profile(context.Context, string) *claudeapi.Identity {
	return &claudeapi.Identity{UUID: o.uuid, Email: o.email}
}

func TestExportAndImportRoundTripThroughTheCLI(t *testing.T) {
	from := newHarness(t)
	from.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	from.login("1", "one@example.com")

	path := filepath.Join(t.TempDir(), "accounts.json")
	if code := from.run("account", "export", path); code != ExitOK {
		t.Fatalf("export: exit = %d: %s", code, from.stderr())
	}
	// The summary is on stderr, so a piped export yields nothing but the file.
	wantContains(t, from.stderr(), "Exported", "2 account(s)")
	if from.stdout() != "" {
		t.Errorf("export to a file wrote to stdout: %q", from.stdout())
	}

	// The file carries live refresh tokens.
	testutil.AssertPerm(t, path, 0o600)

	to := newHarness(t)
	if code := to.run("account", "import", path); code != ExitOK {
		t.Fatalf("import: exit = %d: %s", code, to.stderr())
	}
	wantContains(t, to.stdout(), "Imported", "one@example.com", "two@example.com")

	roster, err := to.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Accounts) != 2 {
		t.Errorf("the roster holds %v", roster.Accounts)
	}
}

// "-" is how a user adds their own encryption, so stdout must be nothing but
// the envelope.
func TestExportToStdoutIsPureJSON(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})

	if code := h.run("account", "export", "-"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	payload := h.decodeJSON()
	if payload["version"] != float64(1) {
		t.Errorf("version = %v", payload["version"])
	}
	if payload["encrypted"] != false {
		t.Errorf("encrypted = %v", payload["encrypted"])
	}
	accounts := payload["accounts"].([]any)
	if len(accounts) != 1 {
		t.Errorf("accounts = %v", accounts)
	}
}

func TestImportFromStdin(t *testing.T) {
	from := newHarness(t)
	from.seed(map[string]string{"1": "one@example.com"})
	if code := from.run("account", "export", "-"); code != ExitOK {
		t.Fatalf("export: exit = %d: %s", code, from.stderr())
	}
	envelope := from.stdout()

	to := newHarness(t)
	to.app.In = strings.NewReader(envelope)
	if code := to.run("account", "import", "-"); code != ExitOK {
		t.Fatalf("import: exit = %d: %s", code, to.stderr())
	}
	wantContains(t, to.stdout(), "Imported", "one@example.com")
}

func TestImportRefusesAnEncryptedEnvelope(t *testing.T) {
	h := newHarness(t)
	path := filepath.Join(t.TempDir(), "encrypted.json")
	if err := os.WriteFile(path,
		[]byte(`{"version":1,"encrypted":true,"accounts":[{"number":1,"email":"a@b.c"}]}`),
		0o600); err != nil {
		t.Fatal(err)
	}

	if code := h.run("account", "import", path); code != ExitError {
		t.Fatalf("exit = %d, want a refusal", code)
	}
	wantContains(t, h.stderr(), "Decrypt it before importing", "gpg")
}

func TestImportOfAMissingFile(t *testing.T) {
	h := newHarness(t)
	if code := h.run("account", "import", filepath.Join(t.TempDir(), "nope.json")); code != ExitError {
		t.Fatalf("exit = %d", code)
	}
	wantContains(t, h.stderr(), "import file not found")
}

func TestMappings(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	dir := t.TempDir()

	if code := h.run("dir", "map", "1", dir); code != ExitOK {
		t.Fatalf("map: exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Mapped", "one@example.com")

	if code := h.run("dir", "list"); code != ExitOK {
		t.Fatalf("mappings: exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "one@example.com")

	if code := h.run("dir", "unmap", dir); code != ExitOK {
		t.Fatalf("unmap: exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Unmapped")

	if code := h.run("dir", "list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "No directories are mapped", "aaswap dir map")
}

func TestMappingAnUnknownAccount(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	if code := h.run("dir", "map", "nobody@example.com", t.TempDir()); code != ExitError {
		t.Fatalf("exit = %d", code)
	}
	wantContains(t, h.stderr(), "does not match any managed account")
}

// Everything after `--` belongs to Claude Code, including something that looks
// like an account.
func TestSplitRunArgs(t *testing.T) {
	tests := []struct {
		name           string
		args           []string
		dashAt         int
		wantIdentifier string
		wantClaude     []string
	}{
		{"just an account", []string{"2"}, -1, "2", nil},
		{"an account and passthrough", []string{"2", "--resume"}, 1, "2", []string{"--resume"}},
		{"only passthrough", []string{"--resume"}, 0, "", []string{"--resume"}},
		{"nothing at all", nil, -1, "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := &cobra.Command{}
			cmd.SetArgs(tt.args)
			// ArgsLenAtDash is what cobra records; drive it directly rather
			// than re-parsing.
			identifier, claudeArgs := splitRunArgsAt(tt.args, tt.dashAt)
			if identifier != tt.wantIdentifier {
				t.Errorf("identifier = %q, want %q", identifier, tt.wantIdentifier)
			}
			if len(claudeArgs) != len(tt.wantClaude) {
				t.Errorf("claudeArgs = %v, want %v", claudeArgs, tt.wantClaude)
				return
			}
			for i := range claudeArgs {
				if claudeArgs[i] != tt.wantClaude[i] {
					t.Errorf("claudeArgs = %v, want %v", claudeArgs, tt.wantClaude)
					break
				}
			}
		})
	}
}

// The event stream is one JSON object per line, so a consumer can tail it.
func TestLoginWithAToken(t *testing.T) {
	tests := []struct {
		name      string
		token     string
		flags     []string
		wantKind  swap.Kind
		wantEmail string
	}{
		{
			// A synthesized label, because these tokens carry no address and
			// making the user invent one is noise.
			name: "a setup token", token: "sk-ant-oat01-abcdef",
			wantKind: swap.KindOAuth, wantEmail: "setup-token@token.local",
		},
		{
			name: "a managed API key", token: "sk-ant-api03-abcdef",
			wantKind: swap.KindAPIKey, wantEmail: "api-key@token.local",
		},
		{
			name: "with an address", token: "sk-ant-oat01-abcdef",
			flags:    []string{"--email", "me@example.com"},
			wantKind: swap.KindOAuth, wantEmail: "me@example.com",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			args := append([]string{"login", "--token", tt.token}, tt.flags...)
			if code := h.run(args...); code != ExitOK {
				t.Fatalf("exit = %d: %s", code, h.stderr())
			}
			wantContains(t, h.stdout(), "Added", tt.wantEmail)

			roster, err := h.switcher.RosterOrEmpty()
			if err != nil {
				t.Fatal(err)
			}
			name := roster.Names()[0]
			account := roster.Accounts[name]
			if account == nil || account.Email != tt.wantEmail {
				t.Fatalf("roster = %+v", roster.Accounts)
			}
			if account.AuthKind() != tt.wantKind {
				t.Errorf("kind = %q, want %q", account.AuthKind(), tt.wantKind)
			}

			stored, _ := h.switcher.Creds.ReadAccount(name, tt.wantEmail)
			if tt.wantKind == swap.KindAPIKey {
				// Stored raw: that is what Claude Code's API-key axis reads.
				if stored != tt.token {
					t.Errorf("the stored key is %q", stored)
				}
			} else if !strings.Contains(stored, tt.token) {
				t.Errorf("the stored credential does not carry the token: %q", stored)
			}
		})
	}
}

// A token on the command line lands in the shell history, so piping it is worth
// having.
func TestLoginWithATokenFromStdin(t *testing.T) {
	h := newHarness(t)
	h.app.In = strings.NewReader("sk-ant-oat01-piped\n")

	if code := h.run("login", "--token", "-"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	stored, _ := h.switcher.Creds.ReadAccount("setup-token", "setup-token@token.local")
	if !strings.Contains(stored, "sk-ant-oat01-piped") {
		t.Errorf("the stored credential is %q", stored)
	}
}

// Identity is the address alone here, so two kinds sharing one could not be
// told apart at switch time.
func TestLoginWithATokenRefusesACrossKindCollision(t *testing.T) {
	h := newHarness(t)
	if code := h.run("login", "--token", "sk-ant-oat01-abc", "--email", "me@example.com"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if code := h.run("login", "--token", "sk-ant-api03-abc", "--email", "me@example.com"); code != ExitError {
		t.Fatalf("exit = %d, want a refusal", code)
	}
	wantContains(t, h.stderr(), "already exists as an OAuth account", "distinct --email")
}

// A new token for an account already here refreshes it in place rather than
// making a second slot.
func TestLoginWithATokenRefreshesInPlace(t *testing.T) {
	h := newHarness(t)
	if code := h.run("login", "--token", "sk-ant-oat01-first", "--email", "me@example.com"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if code := h.run("login", "--token", "sk-ant-oat01-second", "--email", "me@example.com"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Updated the token")

	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Accounts) != 1 {
		t.Errorf("the account was duplicated: %+v", roster.Accounts)
	}
	stored, _ := h.switcher.Creds.ReadAccount("me", "me@example.com")
	if !strings.Contains(stored, "second") {
		t.Errorf("the stored credential is %q", stored)
	}
}

func TestLoginWithATokenRejections(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"an empty token", []string{"login", "--token", "   "}, "cannot be empty"},
		{"a bad address", []string{"login", "--token", "tok", "--email", "not an email"}, "not a valid email"},
		{"a numeric name", []string{"login", "--token", "tok", "--name", "0"}, "cannot be purely numeric"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			code := h.run(tt.args...)
			if tt.wantErr == "" {
				// A zero slot is the same as not naming one.
				if code != ExitOK {
					t.Fatalf("exit = %d: %s", code, h.stderr())
				}
				return
			}
			if code != ExitError {
				t.Fatalf("exit = %d, want a rejection", code)
			}
			wantContains(t, h.stderr(), tt.wantErr)
		})
	}
}

// A token account has no quota to fetch, so it says so rather than showing a
// blank.
func TestATokenAccountReportsNoQuota(t *testing.T) {
	h := newHarness(t)
	if code := h.run("login", "--token", "sk-ant-api03-abc"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if code := h.run("list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "API key", "no quota")
}

// A name is where an account's credential is filed, so renaming has to move
// stored material — not just relabel a row.
func TestRenameMovesTheStoredMaterial(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"work": "one@example.com"})

	if code := h.run("account", "rename", "work", "Personal"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Renamed", "work", "personal")

	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if roster.Accounts["personal"] == nil {
		t.Fatalf("accounts = %v, want one called \"personal\"", roster.Names())
	}
	if _, still := roster.Accounts["work"]; still {
		t.Error("the old name survived")
	}
	if value, _ := h.switcher.Creds.ReadAccount("personal", "one@example.com"); value == "" {
		t.Error("the credential did not move with the name")
	}
	if value, _ := h.switcher.Creds.ReadAccount("work", "one@example.com"); value != "" {
		t.Error("the credential was left behind under the old name")
	}
}

// Taking a name another account holds would file two accounts in one place.
func TestRenameRefusesAHeldName(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"work": "one@example.com", "spare": "two@example.com"})

	if code := h.run("account", "rename", "work", "spare"); code != ExitError {
		t.Fatalf("exit = %d, want a refusal: %s", code, h.stdout())
	}
	wantContains(t, h.stderr(), "already")
}

// --all and an account name are two different requests, and doing either one
// silently when both were given would remove more or less than was asked.
func TestRemoveRefusesAllTogetherWithAnAccount(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	h.app.Confirm = func(string) bool { return true }

	if code := h.run("account", "remove", "--all", "1"); code != ExitError {
		t.Fatalf("exit = %d, want a refusal: %s", code, h.stdout())
	}
	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Accounts) != 1 {
		t.Error("a refused removal changed the roster anyway")
	}
}

// Naming nothing at all is a request that cannot be carried out, and must not
// become "remove everything".
func TestRemoveWithNoAccountRefuses(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	h.app.Confirm = func(string) bool { return true }

	if code := h.run("account", "remove"); code != ExitError {
		t.Fatalf("exit = %d, want a refusal: %s", code, h.stdout())
	}
	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Accounts) != 1 {
		t.Error("removing with no argument removed something")
	}
}
