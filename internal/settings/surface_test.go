package settings

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// A settings key is a promise. Someone reads `aaswap config list`, sets a value,
// gets "ok", and expects the tool to behave differently. A key nothing reads
// keeps that promise only in appearance — and it is invisible to every other
// kind of test, because setting it and reading it back both work perfectly.
//
// Six keys were in exactly that state after automatic rotation was removed:
// intervalSeconds, cooldownSeconds, hysteresisPct, strategy,
// includeApiKeyAccounts and unhealthyTicks configured a loop that no longer
// exists. This list is what stops another one from surviving a removal.
//
// Adding a key here is a claim that something reads it. Name the reader.
var readByProduction = []string{
	// swap.snapshot flags an account whose binding window has reached it.
	"autoswitch.threshold",
	// swap.report folds these models' weekly limits into the binding window.
	"autoswitch.model",
	// render picks the palette from it; "auto" detects the terminal background.
	"ui.theme",
}

func TestEverySettingsKeyIsReadBySomething(t *testing.T) {
	var offered []string
	for _, spec := range Specs() {
		offered = append(offered, spec.Dotted())
	}

	for _, key := range offered {
		if !slices.Contains(readByProduction, key) {
			t.Errorf("`aaswap config set %s` reports success and changes nothing. "+
				"Either something has to read it, or it must not be offered", key)
		}
	}
	for _, key := range readByProduction {
		if !slices.Contains(offered, key) {
			t.Errorf("%q is claimed to be read but `aaswap config` does not offer it", key)
		}
	}
}

// A key removed from the surface must not take an existing settings.json's
// value with it. Someone who set the old rotation knobs still has them in the
// file, and rewriting one setting must not silently drop the rest.
func TestRemovingAKeyFromTheSurfaceLeavesItInTheFile(t *testing.T) {
	root := t.TempDir()
	written := `{
  "version": 1,
  "autoswitch": {"threshold": 80, "cooldownSeconds": 900, "strategy": "consume-first"},
  "somethingNewer": {"key": true}
}`
	if err := os.WriteFile(Path(root), []byte(written), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Set(root, "ui.theme", "dark"); err != nil {
		t.Fatalf("setting an unrelated key: %v", err)
	}

	data, err := os.ReadFile(Path(root))
	if err != nil {
		t.Fatal(err)
	}
	after := string(data)
	for _, keep := range []string{"cooldownSeconds", "consume-first", "somethingNewer"} {
		if !strings.Contains(after, keep) {
			t.Errorf("%q was dropped from settings.json by an unrelated write:\n%s",
				keep, after)
		}
	}
	// And the key that IS still read survives with its configured value.
	if Load(root).AutoSwitch.Threshold != 80 {
		t.Errorf("threshold = %v, want the file's 80", Load(root).AutoSwitch.Threshold)
	}
}
