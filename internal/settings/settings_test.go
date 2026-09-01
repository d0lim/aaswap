package settings

import (
	json "encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/d0lim/ccswap/internal/apperr"
)

// writeSettings drops a raw settings.json into a fresh backup root.
func writeSettings(t *testing.T, raw any) string {
	t.Helper()
	root := t.TempDir()
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(root), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

func readSettings(t *testing.T, root string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(Path(root))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	return raw
}

// ---------------------------------------------------------------- Load

func TestLoadDegradesToDefaults(t *testing.T) {
	tests := []struct {
		name  string
		write func(t *testing.T) string
	}{
		{
			name:  "missing file",
			write: func(t *testing.T) string { return t.TempDir() },
		},
		{
			name: "corrupt file",
			write: func(t *testing.T) string {
				root := t.TempDir()
				if err := os.WriteFile(Path(root), []byte("{not json"), 0o600); err != nil {
					t.Fatal(err)
				}
				return root
			},
		},
		{
			name: "top level is not an object",
			write: func(t *testing.T) string {
				root := t.TempDir()
				if err := os.WriteFile(Path(root), []byte(`["a", "b"]`), 0o600); err != nil {
					t.Fatal(err)
				}
				return root
			},
		},
		{
			name:  "section is not an object",
			write: func(t *testing.T) string { return writeSettings(t, map[string]any{"autoswitch": "nope"}) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, want := Load(tt.write(t)), Defaults(); !reflect.DeepEqual(got, want) {
				t.Errorf("Load() = %+v, want the defaults %+v", got, want)
			}
		})
	}
}

func TestLoadFillsDefaultsForAbsentKeys(t *testing.T) {
	root := writeSettings(t, map[string]any{"autoswitch": map[string]any{"threshold": 80}})

	got := Load(root)
	if got.AutoSwitch.Threshold != 80 {
		t.Errorf("Threshold = %v, want the file's 80", got.AutoSwitch.Threshold)
	}
	if want := Defaults().AutoSwitch.CooldownSeconds; got.AutoSwitch.CooldownSeconds != want {
		t.Errorf("CooldownSeconds = %v, want the default %v", got.AutoSwitch.CooldownSeconds, want)
	}
	if want := Defaults().UI.Theme; got.UI.Theme != want {
		t.Errorf("Theme = %v, want the default %v", got.UI.Theme, want)
	}
}

// Out-of-range values written by hand are clamped rather than rejected:
// refusing to start over a settings file is worse than running with a sane one.
func TestLoadClampsOutOfRangeValues(t *testing.T) {
	root := writeSettings(t, map[string]any{
		"autoswitch": map[string]any{
			"threshold":       200,
			"intervalSeconds": 1,
			"hysteresisPct":   -5,
			"unhealthyTicks":  0,
		},
	})

	got := Load(root).AutoSwitch
	if got.Threshold != 99.9 {
		t.Errorf("Threshold = %v, want the 99.9 ceiling", got.Threshold)
	}
	if got.IntervalSeconds != 15 {
		t.Errorf("IntervalSeconds = %v, want the 15s floor (the usage-cache TTL)", got.IntervalSeconds)
	}
	if got.HysteresisPct != 0 {
		t.Errorf("HysteresisPct = %v, want the 0 floor", got.HysteresisPct)
	}
	if got.UnhealthyTicks != 1 {
		t.Errorf("UnhealthyTicks = %v, want the 1 floor", got.UnhealthyTicks)
	}
}

func TestLoadCoercesBadTypes(t *testing.T) {
	root := writeSettings(t, map[string]any{
		"autoswitch": map[string]any{
			"threshold": "high",
			// A number in a boolean slot follows Python's truthiness, which is
			// how the file the original wrote has always been read.
			"includeApiKeyAccounts": 1,
			// Garbage in the model slot disables the filter rather than
			// wedging the engine on a name no account reports.
			"model": 123,
		},
	})

	got := Load(root).AutoSwitch
	if want := Defaults().AutoSwitch.Threshold; got.Threshold != want {
		t.Errorf("Threshold = %v, want the default %v for a non-numeric value", got.Threshold, want)
	}
	if !got.IncludeAPIKeyAccounts {
		t.Error("IncludeAPIKeyAccounts = false, want a non-zero number to read as true")
	}
	if got.Model != "" {
		t.Errorf("Model = %q, want it cleared for a non-string value", got.Model)
	}
}

func TestLoadChoiceValues(t *testing.T) {
	tests := []struct {
		name    string
		section string
		key     string
		value   any
		want    string
		get     func(Settings) string
	}{
		{"valid strategy", "autoswitch", "strategy", "consume-first", "consume-first",
			func(s Settings) string { return s.AutoSwitch.Strategy }},
		{"unsupported strategy", "autoswitch", "strategy", "chaos", "best",
			func(s Settings) string { return s.AutoSwitch.Strategy }},
		{"valid theme", "ui", "theme", "light", "light",
			func(s Settings) string { return s.UI.Theme }},
		{"unsupported theme", "ui", "theme", "purple", "auto",
			func(s Settings) string { return s.UI.Theme }},
		{"non-string theme", "ui", "theme", 42, "auto",
			func(s Settings) string { return s.UI.Theme }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := writeSettings(t, map[string]any{tt.section: map[string]any{tt.key: tt.value}})
			if got := tt.get(Load(root)); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// ---------------------------------------------------------------- Save

func TestSaveRoundTrips(t *testing.T) {
	root := t.TempDir()
	custom := Defaults().AutoSwitch
	custom.Threshold = 85
	custom.CooldownSeconds = 60

	if err := Save(root, custom); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got := Load(root).AutoSwitch; !reflect.DeepEqual(got, custom) {
		t.Errorf("round trip = %+v, want %+v", got, custom)
	}
}

// Another tool's keys, or a future version's, must survive a write from this
// one — the file is shared, additive space.
func TestSavePreservesUnknownKeysAndSections(t *testing.T) {
	root := writeSettings(t, map[string]any{
		"schemaVersion": 1,
		"futureSection": map[string]any{"x": 1},
		"autoswitch":    map[string]any{"threshold": 80, "futureKnob": true},
	})

	auto := Defaults().AutoSwitch
	auto.Threshold = 70
	if err := Save(root, auto); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw := readSettings(t, root)
	future, ok := raw["futureSection"].(map[string]any)
	if !ok || future["x"] != float64(1) {
		t.Errorf("futureSection = %v, want it preserved", raw["futureSection"])
	}
	sec := raw["autoswitch"].(map[string]any)
	if sec["futureKnob"] != true {
		t.Errorf("futureKnob = %v, want it preserved", sec["futureKnob"])
	}
	if sec["threshold"] != float64(70) {
		t.Errorf("threshold = %v, want the saved 70", sec["threshold"])
	}
}

func TestSaveFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not meaningful on Windows")
	}
	root := t.TempDir()
	if err := Save(root, Defaults().AutoSwitch); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(Path(root))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("settings.json mode = %#o, want 0600", got)
	}
}

// ---------------------------------------------------------------- Set / Unset

// Set writes only the key it was given, never every known key: writing them all
// would freeze today's defaults into the file and pin the user to them if a
// later version changes one.
func TestSetWritesAMinimalFile(t *testing.T) {
	root := t.TempDir()

	value, err := Set(root, "autoswitch.threshold", "80")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if value != 80.0 {
		t.Errorf("Set returned %v, want 80", value)
	}

	want := map[string]any{
		"schemaVersion": float64(1),
		"autoswitch":    map[string]any{"threshold": float64(80)},
	}
	if got := readSettings(t, root); !reflect.DeepEqual(got, want) {
		t.Errorf("file = %v, want %v", got, want)
	}
}

func TestSetParsing(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		raw     string
		want    any
		wantErr string // substring; empty means success
	}{
		{name: "float", key: "autoswitch.threshold", raw: "80", want: 80.0},
		{name: "int", key: "autoswitch.unhealthyTicks", raw: "5", want: 5},
		{name: "int rejects a float", key: "autoswitch.unhealthyTicks", raw: "3.5", wantErr: "integer"},
		{name: "float out of range", key: "autoswitch.threshold", raw: "200", wantErr: "between 50 and 99.9"},
		{name: "choice", key: "autoswitch.strategy", raw: "consume-first", want: "consume-first"},
		{name: "choice rejects", key: "autoswitch.strategy", raw: "chaos", wantErr: "must be one of"},
		{name: "string", key: "autoswitch.model", raw: "Fable", want: "Fable"},
		{name: "string rejects empty", key: "autoswitch.model", raw: "   ", wantErr: "non-empty"},
		// bool words are matched explicitly: a naive string conversion makes
		// "false" true, the opposite of what the user typed.
		{name: "bool false word", key: "autoswitch.includeApiKeyAccounts", raw: "FALSE", want: false},
		{name: "bool true word", key: "autoswitch.includeApiKeyAccounts", raw: "yes", want: true},
		{name: "bool rejects nonsense", key: "autoswitch.includeApiKeyAccounts", raw: "falsy", wantErr: "true or false"},
		{name: "unknown key", key: "autoswitch.bogus", raw: "1", wantErr: "unknown setting"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			got, err := Set(root, tt.key, tt.raw)

			if tt.wantErr != "" {
				if err == nil {
					t.Fatalf("Set = %v, want an error containing %q", got, tt.wantErr)
				}
				if !errors.Is(err, apperr.ErrConfig) {
					t.Errorf("error %v does not wrap apperr.ErrConfig", err)
				}
				if !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error %q does not contain %q", err, tt.wantErr)
				}
				// A rejected value must not leave a file behind.
				if _, statErr := os.Stat(Path(root)); statErr == nil {
					t.Error("a rejected Set wrote settings.json anyway")
				}
				return
			}

			if err != nil {
				t.Fatalf("Set: %v", err)
			}
			if got != tt.want {
				t.Errorf("Set = %v (%T), want %v (%T)", got, got, tt.want, tt.want)
			}
		})
	}
}

// A read-modify-write starting from an empty object would replace a malformed —
// and maybe hand-recoverable — file with a near-empty one.
func TestSetOnACorruptFilePreservesIt(t *testing.T) {
	root := t.TempDir()
	const corrupt = "{not json"
	if err := os.WriteFile(Path(root), []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Set(root, "autoswitch.threshold", "80"); !errors.Is(err, apperr.ErrConfig) {
		t.Fatalf("Set on a corrupt file = %v, want an apperr.ErrConfig", err)
	}
	b, err := os.ReadFile(Path(root))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != corrupt {
		t.Errorf("corrupt file was rewritten as %q", b)
	}
}

func TestUnset(t *testing.T) {
	t.Run("removes the key and an emptied section", func(t *testing.T) {
		root := t.TempDir()
		if _, err := Set(root, "ui.theme", "light"); err != nil {
			t.Fatal(err)
		}
		removed, err := Unset(root, "ui.theme")
		if err != nil {
			t.Fatalf("Unset: %v", err)
		}
		if !removed {
			t.Error("Unset reported no removal for a key that was set")
		}
		raw := readSettings(t, root)
		if _, ok := raw["ui"]; ok {
			t.Errorf("emptied ui section survived: %v", raw)
		}
		if Load(root).UI.Theme != "auto" {
			t.Error("theme did not fall back to the default after unset")
		}
	})

	t.Run("keeps a section that still has keys", func(t *testing.T) {
		root := t.TempDir()
		if _, err := Set(root, "autoswitch.threshold", "80"); err != nil {
			t.Fatal(err)
		}
		if _, err := Set(root, "autoswitch.strategy", "consume-first"); err != nil {
			t.Fatal(err)
		}
		if _, err := Unset(root, "autoswitch.threshold"); err != nil {
			t.Fatal(err)
		}
		sec := readSettings(t, root)["autoswitch"].(map[string]any)
		if _, ok := sec["threshold"]; ok {
			t.Error("threshold survived unset")
		}
		if sec["strategy"] != "consume-first" {
			t.Errorf("strategy = %v, want it untouched", sec["strategy"])
		}
	})

	t.Run("an absent key writes nothing", func(t *testing.T) {
		root := t.TempDir()
		removed, err := Unset(root, "autoswitch.threshold")
		if err != nil {
			t.Fatalf("Unset: %v", err)
		}
		if removed {
			t.Error("Unset reported a removal for a key that was never set")
		}
		if _, err := os.Stat(Path(root)); err == nil {
			t.Error("Unset created settings.json for an absent key")
		}
	})

	t.Run("stamps schemaVersion on an unversioned file", func(t *testing.T) {
		root := writeSettings(t, map[string]any{"ui": map[string]any{"theme": "light"}})
		if _, err := Unset(root, "ui.theme"); err != nil {
			t.Fatal(err)
		}
		if got := readSettings(t, root)["schemaVersion"]; got != float64(SchemaVersion) {
			t.Errorf("schemaVersion = %v, want %d", got, SchemaVersion)
		}
	})
}

// ---------------------------------------------------------------- Effective

func TestEffective(t *testing.T) {
	t.Run("a missing file reports every key as a default", func(t *testing.T) {
		rows := Effective(t.TempDir())
		if len(rows) != len(specs) {
			t.Fatalf("got %d rows, want one per spec (%d)", len(rows), len(specs))
		}
		for _, row := range rows {
			if row.IsSet {
				t.Errorf("%s reported as set with no file present", row.Spec.Dotted())
			}
			if !reflect.DeepEqual(row.Value, row.Spec.Default()) {
				t.Errorf("%s = %v, want the default %v", row.Spec.Dotted(), row.Value, row.Spec.Default())
			}
		}
	})

	// "Set" means present in the file. An explicit value equal to the default
	// still counts, so the "(default)" marker reflects the file rather than
	// value equality.
	t.Run("presence not value equality marks a key set", func(t *testing.T) {
		defaultThreshold := Defaults().AutoSwitch.Threshold
		root := writeSettings(t, map[string]any{
			"autoswitch": map[string]any{"threshold": defaultThreshold},
		})
		for _, row := range Effective(root) {
			if row.Spec.Dotted() == "autoswitch.threshold" && !row.IsSet {
				t.Error("a key explicitly set to its default value was reported as unset")
			}
			if row.Spec.Dotted() == "autoswitch.strategy" && row.IsSet {
				t.Error("an absent key was reported as set")
			}
		}
	})
}

// ---------------------------------------------------------------- Overrides

func TestMergeCLI(t *testing.T) {
	base := Defaults().AutoSwitch

	t.Run("no overrides leaves settings untouched", func(t *testing.T) {
		if got := MergeCLI(base, Overrides{}); !reflect.DeepEqual(got, base) {
			t.Errorf("MergeCLI with no overrides = %+v, want %+v", got, base)
		}
	})

	t.Run("a flag beats the file", func(t *testing.T) {
		from := base
		from.Threshold = 70
		if got := MergeCLI(from, Overrides{Threshold: new(85.0)}); got.Threshold != 85 {
			t.Errorf("Threshold = %v, want the flag's 85", got.Threshold)
		}
	})

	// A flag is subject to the same bounds as a file value.
	t.Run("flag values are clamped", func(t *testing.T) {
		if got := MergeCLI(base, Overrides{IntervalSeconds: new(1.0)}); got.IntervalSeconds != 15 {
			t.Errorf("IntervalSeconds = %v, want the 15s floor", got.IntervalSeconds)
		}
	})

	t.Run("every field can be overridden", func(t *testing.T) {
		got := MergeCLI(base, Overrides{
			Threshold:             new(75.0),
			CooldownSeconds:       new(30.0),
			IncludeAPIKeyAccounts: new(true),
			Model:                 new("Fable"),
			Strategy:              new("consume-first"),
		})
		if got.Threshold != 75 || got.CooldownSeconds != 30 || !got.IncludeAPIKeyAccounts ||
			got.Model != "Fable" || got.Strategy != "consume-first" {
			t.Errorf("MergeCLI = %+v, want every override applied", got)
		}
	})

	t.Run("an unsupported strategy reverts to the default", func(t *testing.T) {
		if got := MergeCLI(base, Overrides{Strategy: new("chaos")}); got.Strategy != "best" {
			t.Errorf("Strategy = %q, want the default", got.Strategy)
		}
	})
}

// ---------------------------------------------------------------- Registry

// The registry is the single source of truth for bounds and defaults, so it has
// to stay in step with the structs it describes.
func TestRegistryCoversEveryField(t *testing.T) {
	byField := map[string]int{}
	for _, spec := range specs {
		byField[spec.Section]++
	}
	if got, want := byField["autoswitch"], reflect.TypeFor[AutoSwitch]().NumField(); got != want {
		t.Errorf("autoswitch has %d specs for %d struct fields", got, want)
	}
	if got, want := byField["ui"], reflect.TypeFor[UI]().NumField(); got != want {
		t.Errorf("ui has %d specs for %d struct fields", got, want)
	}
}

func TestSpecDefaultsMatchTheStructs(t *testing.T) {
	d := Defaults()
	for _, spec := range specs {
		if !reflect.DeepEqual(spec.Default(), spec.get(d)) {
			t.Errorf("%s default = %v, want %v", spec.Dotted(), spec.Default(), spec.get(d))
		}
	}
}

func TestSpecForRejectsUnknownKeys(t *testing.T) {
	_, err := SpecFor("autoswitch.nope")
	if !errors.Is(err, apperr.ErrConfig) {
		t.Fatalf("SpecFor = %v, want an apperr.ErrConfig", err)
	}
	// The message has to list the valid keys, since that is what the user
	// needs to see next.
	if !strings.Contains(err.Error(), "autoswitch.threshold") {
		t.Errorf("error %q does not list the valid keys", err)
	}
}

func TestSpecsIsACopy(t *testing.T) {
	got := Specs()
	if len(got) == 0 {
		t.Fatal("Specs() is empty")
	}
	got[0].Section = "mutated"
	if specs[0].Section == "mutated" {
		t.Error("Specs() handed out the registry itself; a caller can corrupt it")
	}
}

// ---------------------------------------------------------------- Helpers

func TestParseModelNames(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"Fable", []string{"Fable"}},
		{"Fable,Opus", []string{"Fable", "Opus"}},
		{" Fable , Opus ", []string{"Fable", "Opus"}},
		// Case-insensitively deduped, first spelling wins.
		{"Fable,fable,FABLE", []string{"Fable"}},
		{"Fable,,Opus", []string{"Fable", "Opus"}},
		{",,,", nil},
		{"all", []string{"all"}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := ParseModelNames(tt.in); !slices.Equal(got, tt.want) {
				t.Errorf("ParseModelNames(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestFormatValue(t *testing.T) {
	tests := []struct {
		in   any
		want string
	}{
		{nil, "(none)"},
		{"", "(none)"},
		{true, "true"},
		{false, "false"},
		{90.0, "90"},
		{99.9, "99.9"},
		{5, "5"},
		{"Fable", "Fable"},
	}
	for _, tt := range tests {
		if got := FormatValue(tt.in); got != tt.want {
			t.Errorf("FormatValue(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestPath(t *testing.T) {
	if got, want := Path("/backup"), filepath.Join("/backup", FileName); got != want {
		t.Errorf("Path = %q, want %q", got, want)
	}
}
