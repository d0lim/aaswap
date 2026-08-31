package cli

import (
	"context"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/realiti4/claude-swap/internal/claudeapi"
	"github.com/realiti4/claude-swap/internal/jsonout"
	"github.com/realiti4/claude-swap/internal/usage"
	"github.com/spf13/cobra"
)

func TestListShowsEveryAccount(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	h.login("1", "one@example.com")
	h.measuring(map[string]*usage.Result{"1": measured(40), "2": measured(10)})

	if code := h.run("list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Account 1", "one@example.com", "Account 2", "two@example.com", "40%", "10%")
}

func TestListOnAnEmptyStoreExplainsWhatToDo(t *testing.T) {
	h := newHarness(t)
	if code := h.run("list"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "No accounts are managed yet", "cswap add")
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
		wantContains(t, h.stdout(), "Account 1", "one@example.com", "40%")
	})

	t.Run("an unmanaged login says how to store it", func(t *testing.T) {
		h := newHarness(t)
		h.seed(map[string]string{"1": "one@example.com"})
		h.login("9", "stranger@example.com")
		if code := h.run("status"); code != ExitOK {
			t.Fatalf("exit = %d: %s", code, h.stderr())
		}
		wantContains(t, h.stdout(), "stranger@example.com", "Not managed", "cswap add")
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
	wantContains(t, h.stdout(), "Account 2", "two@example.com")
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
	wantContains(t, h.stdout(), "Account 3")

	h.login("3", "three@example.com")
	if code := h.run("switch"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Account 1")
}

func TestSwitchByAliasAndEmail(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	h.login("1", "one@example.com")
	if code := h.run("alias", "2", "work"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}

	for _, identifier := range []string{"work", "two@example.com", "2"} {
		h.login("1", "one@example.com")
		if code := h.run("switch", identifier); code != ExitOK {
			t.Fatalf("switch %q: exit = %d: %s", identifier, code, h.stderr())
		}
		wantContains(t, h.stdout(), "Account 2")
	}
}

// The strategy only recommends a move it can prove lands on more headroom.
func TestSwitchWithTheBestStrategy(t *testing.T) {
	t.Run("moves to more headroom", func(t *testing.T) {
		h := newHarness(t)
		h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
		h.login("1", "one@example.com")
		h.measuring(map[string]*usage.Result{"1": measured(80), "2": measured(20)})

		if code := h.run("switch", "--strategy", "best"); code != ExitOK {
			t.Fatalf("exit = %d: %s", code, h.stderr())
		}
		wantContains(t, h.stdout(), "Account 2")
	})

	t.Run("stays rather than moving somewhere worse", func(t *testing.T) {
		h := newHarness(t)
		h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
		h.login("1", "one@example.com")
		h.measuring(map[string]*usage.Result{"1": measured(20), "2": measured(80)})

		if code := h.run("switch", "--strategy", "best"); code != ExitOK {
			t.Fatalf("exit = %d: %s", code, h.stderr())
		}
		wantContains(t, h.stdout(), "most headroom", "Staying put")
		if live := h.switcher.Creds.ReadActive().Value; !strings.Contains(live, "tok-1") {
			t.Error("a stay-put decision switched anyway")
		}
	})

	t.Run("says so when nothing can be measured", func(t *testing.T) {
		h := newHarness(t)
		h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
		h.login("1", "one@example.com")
		// No fetcher: nothing is measurable.
		if code := h.run("switch", "--strategy", "best"); code != ExitOK {
			t.Fatalf("exit = %d: %s", code, h.stderr())
		}
		wantContains(t, h.stdout(), "unknown", "Staying put")
	})
}

// Unknown is not exhausted: an account whose usage could not be measured stays
// a candidate, or a failing endpoint would strand the user.
func TestNextAvailableSkipsOnlyProvenExhaustion(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com", "3": "three@example.com"})
	h.login("1", "one@example.com")
	h.measuring(map[string]*usage.Result{
		"1": measured(50),
		"2": measured(100), // at its limit — skipped
		// slot 3 is unmeasurable, and therefore still a candidate
	})

	if code := h.run("switch", "--strategy", "next-available"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Account 3")
}

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
	if from["number"] != float64(1) || from["email"] != "one@example.com" {
		t.Errorf("from = %v", from)
	}
	to := payload["to"].(map[string]any)
	if to["number"] != float64(2) {
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
	if code := h.run("remove", "nobody", "--json", "--yes"); code != ExitError {
		t.Fatalf("exit = %d", code)
	}
	envelope := h.decodeJSON()["error"].(map[string]any)
	if envelope["type"] != "ConfigError" {
		t.Errorf("type = %v, want the taxonomy's name", envelope["type"])
	}
}

func TestAddAndRemove(t *testing.T) {
	h := newHarness(t)
	h.login("1", "one@example.com")

	if code := h.run("add"); code != ExitOK {
		t.Fatalf("add: exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Added", "Account 1", "one@example.com")

	if code := h.run("remove", "1", "--yes"); code != ExitOK {
		t.Fatalf("remove: exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Removed", "Account 1")

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
		{"remove", []string{"remove", "1"}},
		{"purge", []string{"purge"}},
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

	if code := h.run("purge"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if len(roster.Accounts) != 1 {
		t.Error("a purge ran with nobody to confirm it")
	}
}

func TestDisableAndEnable(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	h.login("1", "one@example.com")

	if code := h.run("disable", "2"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Disabled", "Account 2")

	// A disabled account is out of rotation but still an explicit target.
	if code := h.run("switch", "2"); code != ExitOK {
		t.Fatalf("switch to a disabled account: exit = %d: %s", code, h.stderr())
	}

	// Saying it twice reports the state rather than claiming an edit.
	if code := h.run("disable", "2"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "already disabled")

	if code := h.run("enable", "2"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Enabled", "back in the rotation")
}

// Disabling the last rotatable account leaves auto-switch nothing to pick.
func TestDisablingTheLastAccountWarns(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	h.login("1", "one@example.com")

	if code := h.run("disable", "1"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "No accounts remain in rotation", "cswap enable")
}

func TestAliasCommands(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})

	if code := h.run("alias", "1", "Work"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	// Normalized to lowercase, so resolution is unambiguous.
	wantContains(t, h.stdout(), `"work"`)

	if code := h.run("alias"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "work", "Account 1")

	// A second slot cannot take the same name.
	if code := h.run("alias", "2", "work"); code != ExitError {
		t.Fatalf("exit = %d, want a conflict", code)
	}
	wantContains(t, h.stderr(), "already used by account 1")

	if code := h.run("alias", "1", "--unset"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Removed")
}

func TestAliasRejections(t *testing.T) {
	tests := []struct {
		name    string
		alias   string
		wantErr string
	}{
		{"purely numeric", "3", "purely numeric"},
		// Passed after "--" so the shell's flag parser hands it through as the
		// argument it is.
		{"leading dash", "-x", "cannot start with"},
		{"illegal characters", "my alias", "may only contain"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.seed(map[string]string{"1": "one@example.com"})
			if code := h.run("alias", "1", "--", tt.alias); code != ExitError {
				t.Fatalf("exit = %d, want a rejection", code)
			}
			wantContains(t, h.stderr(), tt.wantErr)
		})
	}
}

func TestSwapAndMove(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})

	if code := h.run("swap", "1", "2"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Swapped", "1", "2")

	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if roster.Accounts["1"].Email != "two@example.com" {
		t.Errorf("slot 1 = %q", roster.Accounts["1"].Email)
	}

	if code := h.run("move", "1", "5"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Moved", "slot 5")
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

func TestConfigPath(t *testing.T) {
	h := newHarness(t)
	if code := h.run("config", "path"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "settings.json")
}

// A --model flag overrides the stored setting for this run only.
func TestTheModelFlagOverridesTheSettingForOneRun(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	h.login("1", "one@example.com")
	// Account-wide, slot 1 has more headroom. Its Fable window is spent.
	h.measuring(map[string]*usage.Result{
		"1": {
			FiveHour: &usage.Window{Pct: 10}, SevenDay: &usage.Window{Pct: 10},
			Scoped: []usage.Scoped{{Name: "Fable", Pct: 100}},
		},
		"2": {FiveHour: &usage.Window{Pct: 50}, SevenDay: &usage.Window{Pct: 50}},
	})

	// Without the flag, slot 1 is the better account and it stays.
	if code := h.run("switch", "--strategy", "best"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Staying put")

	// Counting the Fable window, slot 1 is at its limit for that model.
	if code := h.run("switch", "--strategy", "best", "--model", "Fable"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Account 2")
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

	if code := h.run("unclaimed"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "reason:", "the config named slot 1", "--purge")

	if code := h.run("unclaimed", "--purge", "all", "--yes"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Dropped")

	if code := h.run("unclaimed"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "No preserved credentials")
}

func TestUnclaimedOnAnEmptyStore(t *testing.T) {
	h := newHarness(t)
	if code := h.run("unclaimed"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "No preserved credentials")
}

// The flag spellings are in people's shell history and their scripts.
func TestLegacyFlagSpellingsStillWork(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"--list", []string{"--list"}, "Account 1"},
		{"--status", []string{"--status"}, "Account 1"},
		{"--switch-to", []string{"--switch-to", "2"}, "Account 2"},
		{"--switch-to with an equals sign", []string{"--switch-to=2"}, "Account 2"},
		{"--switch rotates", []string{"--switch"}, "Account 2"},
		{"--list with a flag after it", []string{"--list", "--json"}, `"schemaVersion"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
			h.login("1", "one@example.com")

			if code := h.run(tt.args...); code != ExitOK {
				t.Fatalf("exit = %d: %s", code, h.stderr())
			}
			wantContains(t, h.stdout(), tt.want)
		})
	}
}

func TestTranslateLegacyFlags(t *testing.T) {
	tests := []struct {
		in   []string
		want []string
	}{
		{[]string{"--list"}, []string{"list"}},
		{[]string{"--list", "--json"}, []string{"list", "--json"}},
		{[]string{"--switch"}, []string{"switch"}},
		{[]string{"--switch", "--strategy", "best"}, []string{"switch", "--strategy", "best"}},
		{[]string{"--switch-to", "2"}, []string{"switch", "2"}},
		{[]string{"--remove-account=3"}, []string{"remove", "3"}},
		// A verb is left alone.
		{[]string{"list", "--json"}, []string{"list", "--json"}},
		// A flag that is not a legacy spelling passes through, so cobra reports
		// it rather than this silently swallowing it.
		{[]string{"--nonsense"}, []string{"--nonsense"}},
		// Only the FIRST token is translated: a later one is an argument.
		{[]string{"alias", "1", "--list"}, []string{"alias", "1", "--list"}},
		{nil, nil},
	}
	for _, tt := range tests {
		got := translateLegacyFlags(tt.in)
		if len(got) != len(tt.want) {
			t.Errorf("translateLegacyFlags(%v) = %v, want %v", tt.in, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("translateLegacyFlags(%v) = %v, want %v", tt.in, got, tt.want)
				break
			}
		}
	}
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
	if code := from.run("export", path); code != ExitOK {
		t.Fatalf("export: exit = %d: %s", code, from.stderr())
	}
	// The summary is on stderr, so a piped export yields nothing but the file.
	wantContains(t, from.stderr(), "Exported", "2 account(s)")
	if from.stdout() != "" {
		t.Errorf("export to a file wrote to stdout: %q", from.stdout())
	}

	// The file carries live refresh tokens.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("the export's mode is %o, want 0600", perm)
	}

	to := newHarness(t)
	if code := to.run("import", path); code != ExitOK {
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

	if code := h.run("export", "-"); code != ExitOK {
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
	if code := from.run("export", "-"); code != ExitOK {
		t.Fatalf("export: exit = %d: %s", code, from.stderr())
	}
	envelope := from.stdout()

	to := newHarness(t)
	to.app.In = strings.NewReader(envelope)
	if code := to.run("import", "-"); code != ExitOK {
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

	if code := h.run("import", path); code != ExitError {
		t.Fatalf("exit = %d, want a refusal", code)
	}
	wantContains(t, h.stderr(), "Decrypt it before importing", "gpg")
}

func TestImportOfAMissingFile(t *testing.T) {
	h := newHarness(t)
	if code := h.run("import", filepath.Join(t.TempDir(), "nope.json")); code != ExitError {
		t.Fatalf("exit = %d", code)
	}
	wantContains(t, h.stderr(), "import file not found")
}

func TestMappings(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	dir := t.TempDir()

	if code := h.run("map", "1", dir); code != ExitOK {
		t.Fatalf("map: exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Mapped", "one@example.com")

	if code := h.run("mappings"); code != ExitOK {
		t.Fatalf("mappings: exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "one@example.com")

	if code := h.run("unmap", dir); code != ExitOK {
		t.Fatalf("unmap: exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "Unmapped")

	if code := h.run("mappings"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	wantContains(t, h.stdout(), "No directories are mapped", "cswap map")
}

func TestMappingAnUnknownAccount(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com"})
	if code := h.run("map", "nobody@example.com", t.TempDir()); code != ExitError {
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

func TestAutoOnce(t *testing.T) {
	tests := []struct {
		name     string
		setup    func(*harness)
		wantCode int
		wantSaid string
	}{
		{
			name: "nothing to do",
			setup: func(h *harness) {
				h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
				h.login("1", "one@example.com")
				h.measuring(map[string]*usage.Result{"1": measured(50), "2": measured(10)})
			},
			wantCode: 2,
			wantSaid: "below-threshold",
		},
		{
			name: "a switch",
			setup: func(h *harness) {
				h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
				h.login("1", "one@example.com")
				h.measuring(map[string]*usage.Result{"1": measured(95), "2": measured(10)})
			},
			wantCode: 0,
			wantSaid: "account 2",
		},
		{
			name: "nowhere to go",
			setup: func(h *harness) {
				h.seed(map[string]string{"1": "one@example.com"})
				h.login("1", "one@example.com")
				h.measuring(map[string]*usage.Result{"1": measured(95)})
			},
			wantCode: 3,
			wantSaid: "no-candidates",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			tt.setup(h)

			if code := h.run("auto", "--once"); code != tt.wantCode {
				t.Fatalf("exit = %d, want %d: %s%s", code, tt.wantCode, h.stdout(), h.stderr())
			}
			wantContains(t, h.stdout(), tt.wantSaid)
		})
	}
}

// The event stream is one JSON object per line, so a consumer can tail it.
func TestAutoJSONIsOneEventPerLine(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	h.login("1", "one@example.com")
	h.measuring(map[string]*usage.Result{"1": measured(95), "2": measured(10)})

	if code := h.run("auto", "--once", "--json"); code != 0 {
		t.Fatalf("exit = %d: %s%s", code, h.stdout(), h.stderr())
	}

	var kinds []string
	for line := range strings.SplitSeq(strings.TrimSpace(h.stdout()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("a line is not one JSON object: %v\n%s", err, line)
		}
		if event["schemaVersion"] != float64(jsonout.SchemaVersion) {
			t.Errorf("schemaVersion = %v", event["schemaVersion"])
		}
		if event["ts"] == "" {
			t.Errorf("an event carries no timestamp: %v", event)
		}
		kinds = append(kinds, event["event"].(string))
	}
	if !slices.Contains(kinds, "poll") || !slices.Contains(kinds, "switch") {
		t.Errorf("events = %v, want a poll and a switch", kinds)
	}
}

// A dry run reports the decision and changes nothing.
func TestAutoDryRun(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	h.login("1", "one@example.com")
	h.measuring(map[string]*usage.Result{"1": measured(95), "2": measured(10)})

	if code := h.run("auto", "--once", "--dry-run"); code != 0 {
		t.Fatalf("exit = %d: %s%s", code, h.stdout(), h.stderr())
	}
	wantContains(t, h.stdout(), "would switch")
	if live := h.switcher.Creds.ReadActive().Value; !strings.Contains(live, "tok-1") {
		t.Errorf("a dry run moved the live login: %q", live)
	}
}

// A flag overrides the stored policy for one run only.
func TestAutoThresholdFlagOverridesTheSetting(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"})
	h.login("1", "one@example.com")
	h.measuring(map[string]*usage.Result{"1": measured(60), "2": measured(10)})

	// At the default threshold, 60% is fine.
	if code := h.run("auto", "--once"); code != 2 {
		t.Fatalf("exit = %d, want no action: %s", code, h.stdout())
	}

	// Lowered, it is not.
	if code := h.run("auto", "--once", "--threshold", "50"); code != 0 {
		t.Fatalf("exit = %d, want a switch: %s%s", code, h.stdout(), h.stderr())
	}
	wantContains(t, h.stdout(), "account 2")
}
