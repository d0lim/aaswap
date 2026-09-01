package settings

import (
	json "encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/apperr"
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
	if want := Defaults().AutoSwitch.Model; got.AutoSwitch.Model != want {
		t.Errorf("Model = %q, want the default %q", got.AutoSwitch.Model, want)
	}
	if want := Defaults().UI.Theme; got.UI.Theme != want {
		t.Errorf("Theme = %v, want the default %v", got.UI.Theme, want)
	}
}

// Out-of-range values written by hand are clamped rather than rejected:
// refusing to start over a settings file is worse than running with a sane one.
func TestLoadClampsOutOfRangeValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
		want  float64
	}{
		{"above the ceiling", 200, 99.9},
		{"below the floor", -5, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := writeSettings(t, map[string]any{
				"autoswitch": map[string]any{"threshold": tc.value},
			})
			if got := Load(root).AutoSwitch.Threshold; got != tc.want {
				t.Errorf("Threshold = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLoadCoercesBadTypes(t *testing.T) {
	root := writeSettings(t, map[string]any{
		"autoswitch": map[string]any{
			"threshold": "high",
			// Garbage in the model slot disables the filter rather than
			// wedging the listing on a name no account reports.
			"model": 123,
		},
	})

	got := Load(root).AutoSwitch
	if want := Defaults().AutoSwitch.Threshold; got.Threshold != want {
		t.Errorf("Threshold = %v, want the default %v for a non-numeric value", got.Threshold, want)
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
		{name: "float out of range", key: "autoswitch.threshold", raw: "200", wantErr: "between 50 and 99.9"},
		{name: "choice", key: "ui.theme", raw: "light", want: "light"},
		{name: "choice rejects", key: "ui.theme", raw: "purple", wantErr: "must be one of"},
		{name: "string", key: "autoswitch.model", raw: "Fable", want: "Fable"},
		{name: "string rejects empty", key: "autoswitch.model", raw: "   ", wantErr: "non-empty"},
		// A key the rotation engine used to own. It is no longer offered, and
		// the refusal is the point: it used to report success and do nothing.
		{name: "a removed key", key: "autoswitch.cooldownSeconds", raw: "300", wantErr: "unknown setting"},
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
		if _, err := Set(root, "autoswitch.model", "Fable"); err != nil {
			t.Fatal(err)
		}
		if _, err := Unset(root, "autoswitch.threshold"); err != nil {
			t.Fatal(err)
		}
		sec := readSettings(t, root)["autoswitch"].(map[string]any)
		if _, ok := sec["threshold"]; ok {
			t.Error("threshold survived unset")
		}
		if sec["model"] != "Fable" {
			t.Errorf("model = %v, want it untouched", sec["model"])
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

// ------------------------------------------------------------------- Clamp

// Clamp is what the command layer applies to whatever the file produced, so a
// hand-edited value out of range still reaches the listing inside its bounds.
func TestClampBringsValuesBackInRange(t *testing.T) {
	base := Defaults().AutoSwitch

	t.Run("an in-range value is untouched", func(t *testing.T) {
		from := base
		from.Threshold = 70
		if got := Clamp(from); !reflect.DeepEqual(got, from) {
			t.Errorf("Clamp = %+v, want %+v", got, from)
		}
	})

	for _, tc := range []struct {
		name string
		from float64
		want float64
	}{
		{"above the ceiling", 200, 99.9},
		{"below the floor", 1, 50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			from := base
			from.Threshold = tc.from
			if got := Clamp(from).Threshold; got != tc.want {
				t.Errorf("Threshold = %v, want %v", got, tc.want)
			}
		})
	}
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
