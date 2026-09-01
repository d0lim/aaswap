package settings

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"slices"
	"strconv"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/fsutil"
)

// readRaw reads settings.json for a *read* path. Anything unreadable or
// malformed degrades to an empty object with a logged warning, so a bad hand
// edit costs the user their customizations rather than the whole CLI.
func readRaw(path string) map[string]any {
	text, err := fsutil.ReadText(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("could not read settings; using defaults", "path", path, "error", err)
		}
		return map[string]any{}
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		slog.Warn("settings file is not a JSON object; using defaults", "path", path, "error", err)
		return map[string]any{}
	}
	if raw == nil {
		return map[string]any{}
	}
	return raw
}

// readRawForWrite reads settings.json for a *read-modify-write* path.
//
// readRaw's degrade-to-defaults is right for reads, but a read-modify-write
// starting from an empty object would replace a malformed — and maybe
// hand-recoverable — file with a near-empty one, destroying the user's other
// settings to change one key.
func readRawForWrite(path string) (map[string]any, error) {
	text, err := fsutil.ReadText(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, fmt.Errorf("could not read %s: %w: %w", path, apperr.ErrConfig, err)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil, fmt.Errorf(
			"%s is not a valid JSON object; fix or delete it before changing settings: %w: %w",
			path, apperr.ErrConfig, err)
	}
	if raw == nil {
		raw = map[string]any{}
	}
	return raw, nil
}

// section returns the named section as an object, and whether it was one.
// A section holding a non-object (a list, a string) is treated as absent.
func section(raw map[string]any, name string) (map[string]any, bool) {
	s, ok := raw[name].(map[string]any)
	return s, ok
}

// Load reads settings.json and returns the effective settings. It never fails:
// a missing, corrupt, or partially garbage file yields defaults for whatever it
// could not make sense of.
func Load(backupRoot string) Settings {
	return fromRaw(readRaw(Path(backupRoot)))
}

func fromRaw(raw map[string]any) Settings {
	out := Defaults()
	for _, spec := range specs {
		sec, ok := section(raw, spec.Section)
		if !ok {
			continue
		}
		value, present := sec[spec.JSONKey]
		if !present {
			continue
		}
		spec.set(&out, spec.coerce(value))
	}
	return out
}

// coerce turns a raw JSON value into this spec's Go type, clamping numbers into
// range and falling back to the default for anything it cannot use.
//
// This is the lenient half of the pair with ParseValue: out-of-range values
// written by hand are clamped rather than rejected, because refusing to start
// over a settings file is worse than running with a sane value.
func (s Spec) coerce(value any) any {
	switch s.Kind {
	case KindFloat, KindInt:
		// JSON numbers decode to float64. A bool or a string is a type error,
		// not a number to clamp, so it reverts to the default.
		n, ok := value.(float64)
		if !ok {
			return s.Default()
		}
		clamped := min(max(n, s.Lo), s.Hi)
		if s.Kind == KindInt {
			return int(clamped)
		}
		return clamped

	case KindBool:
		return truthy(value)

	case KindString:
		// A non-empty string keeps; anything else — null, a number, garbage —
		// reverts to the default, which disables the filter.
		if str, ok := value.(string); ok && str != "" {
			return str
		}
		return s.Default()

	case KindChoice:
		str, ok := value.(string)
		if !ok || !slices.Contains(s.Choices, str) {
			slog.Warn("unsupported settings value; using the default",
				"key", s.Dotted(), "value", value, "default", s.Default())
			return s.Default()
		}
		return str
	}
	return s.Default()
}

// truthy reproduces Python's bool() over a decoded JSON value, which is what
// the original implementation applied to boolean settings. It keeps a file
// written by the Python version — where `"includeApiKeyAccounts": 1` means
// true — loading the same way here.
func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case float64:
		return v != 0
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	default:
		return true
	}
}

// Set validates and persists one key, returning the stored value.
//
// It writes only that key plus schemaVersion — deliberately not every known key
// the way Save does, which would freeze the current defaults into the file and
// pin the user to them if a later version changes a default. Unknown keys and
// sections in the file survive.
func Set(backupRoot, dotted, rawValue string) (any, error) {
	spec, err := SpecFor(dotted)
	if err != nil {
		return nil, err
	}
	value, err := ParseValue(spec, rawValue)
	if err != nil {
		return nil, err
	}
	path := Path(backupRoot)
	raw, err := readRawForWrite(path)
	if err != nil {
		return nil, err
	}
	stampSchemaVersion(raw)

	sec, ok := section(raw, spec.Section)
	if !ok {
		sec = map[string]any{}
	}
	sec[spec.JSONKey] = value
	raw[spec.Section] = sec
	if err := fsutil.WriteJSONAtomic(path, raw); err != nil {
		return nil, err
	}
	return value, nil
}

// Unset removes one key from settings.json, reporting whether it was there.
// An absent key writes nothing at all.
func Unset(backupRoot, dotted string) (bool, error) {
	spec, err := SpecFor(dotted)
	if err != nil {
		return false, err
	}
	path := Path(backupRoot)
	raw, err := readRawForWrite(path)
	if err != nil {
		return false, err
	}
	sec, ok := section(raw, spec.Section)
	if !ok {
		return false, nil
	}
	if _, present := sec[spec.JSONKey]; !present {
		return false, nil
	}
	stampSchemaVersion(raw)
	delete(sec, spec.JSONKey)
	if len(sec) == 0 {
		delete(raw, spec.Section)
	}
	if err := fsutil.WriteJSONAtomic(path, raw); err != nil {
		return false, err
	}
	return true, nil
}

func stampSchemaVersion(raw map[string]any) {
	if _, ok := raw["schemaVersion"]; !ok {
		raw["schemaVersion"] = SchemaVersion
	}
}

// Row is one line of `aaswap config`'s listing.
type Row struct {
	Spec  Spec
	Value any
	// IsSet reports whether the key is present in the file. An explicit value
	// equal to the default still counts, so the "(default)" marker reflects the
	// file rather than value equality.
	IsSet bool
}

// Effective returns every key's effective value and whether it was explicitly
// set, in registry order.
func Effective(backupRoot string) []Row {
	raw := readRaw(Path(backupRoot))
	loaded := fromRaw(raw)

	rows := make([]Row, len(specs))
	for i, spec := range specs {
		sec, ok := section(raw, spec.Section)
		_, present := sec[spec.JSONKey]
		rows[i] = Row{Spec: spec, Value: spec.get(loaded), IsSet: ok && present}
	}
	return rows
}

var boolWords = map[string]bool{
	"true": true, "1": true, "yes": true,
	"false": false, "0": false, "no": false,
}

// ParseValue strictly parses a CLI-provided string for `aaswap config set`.
//
// Unlike the forgiving clamp on load, an out-of-range or mistyped value is an
// error, so the user learns about the problem when setting the value rather
// than through silently degraded behaviour at `aaswap auto` time.
func ParseValue(spec Spec, raw string) (any, error) {
	switch spec.Kind {
	case KindBool:
		// Never coerce the string itself: a naive conversion makes "false"
		// true, which is the opposite of what the user typed.
		parsed, ok := boolWords[strings.ToLower(strings.TrimSpace(raw))]
		if !ok {
			return nil, fmt.Errorf(
				"%s expects true or false (or 1/0, yes/no), got %q: %w",
				spec.Dotted(), raw, apperr.ErrConfig)
		}
		return parsed, nil

	case KindChoice:
		if !slices.Contains(spec.Choices, raw) {
			return nil, fmt.Errorf(
				"%s must be one of: %s: %w",
				spec.Dotted(), strings.Join(spec.Choices, ", "), apperr.ErrConfig)
		}
		return raw, nil

	case KindString:
		value := strings.TrimSpace(raw)
		if value == "" {
			return nil, fmt.Errorf(
				"%s expects a non-empty value; use 'aaswap config unset %s' to clear it: %w",
				spec.Dotted(), spec.Dotted(), apperr.ErrConfig)
		}
		return value, nil

	case KindInt:
		n, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil {
			return nil, fmt.Errorf(
				"%s expects an integer, got %q: %w", spec.Dotted(), raw, apperr.ErrConfig)
		}
		if err := spec.checkRange(float64(n)); err != nil {
			return nil, err
		}
		return n, nil

	case KindFloat:
		n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
		if err != nil {
			return nil, fmt.Errorf(
				"%s expects a number, got %q: %w", spec.Dotted(), raw, apperr.ErrConfig)
		}
		if err := spec.checkRange(n); err != nil {
			return nil, err
		}
		return n, nil
	}
	return nil, fmt.Errorf("%s has no parser: %w", spec.Dotted(), apperr.ErrConfig)
}

func (s Spec) checkRange(n float64) error {
	if n < s.Lo || n > s.Hi {
		return fmt.Errorf(
			"%s must be between %s and %s: %w",
			s.Dotted(), FormatValue(s.Lo), FormatValue(s.Hi), apperr.ErrConfig)
	}
	return nil
}

// FormatValue renders a settings value the way settings.json writes it.
func FormatValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "(none)"
	case bool:
		return strconv.FormatBool(v)
	case string:
		if v == "" {
			// The empty string is how an unset optional (autoswitch.model) is
			// carried; show it the way a null would read.
			return "(none)"
		}
		return v
	case int:
		return strconv.Itoa(v)
	case float64:
		if v == float64(int64(v)) {
			return strconv.FormatInt(int64(v), 10)
		}
		return strconv.FormatFloat(v, 'g', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}
