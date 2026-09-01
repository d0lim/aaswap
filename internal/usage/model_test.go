package usage

import (
	json "encoding/json/v2"
	"reflect"
	"testing"
	"time"
)

func TestWindowsAlwaysIncludeTheAccountWideOnes(t *testing.T) {
	r := &Result{
		FiveHour: &Window{Pct: 12, ResetsAt: "2026-06-15T18:00:00Z"},
		SevenDay: &Window{Pct: 40, ResetsAt: "2026-06-20T00:00:00Z"},
	}
	got := r.Windows(nil)
	want := []Relevant{
		{Label: "5h", Pct: 12, ResetsAt: "2026-06-15T18:00:00Z"},
		{Label: "7d", Pct: 40, ResetsAt: "2026-06-20T00:00:00Z"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Windows(nil) = %v, want %v", got, want)
	}
}

// Pay-as-you-go credits are a separate axis and do not gate requests, so they
// must never appear among the windows a decision is made on.
func TestWindowsExcludeSpend(t *testing.T) {
	r := &Result{
		FiveHour: &Window{Pct: 10},
		Spend:    &Spend{Used: 50, Limit: 100, Pct: 50, Currency: "USD"},
	}
	for _, w := range r.Windows([]string{AllModels}) {
		if w.Label == "spend" || w.Pct == 50 {
			t.Errorf("spend leaked into the gating windows: %v", w)
		}
	}
}

func TestScopedWindowSelection(t *testing.T) {
	r := &Result{
		FiveHour: &Window{Pct: 10},
		SevenDay: &Window{Pct: 20},
		Scoped: []Scoped{
			{Name: "Fable", Pct: 80},
			{Name: "Opus", Pct: 30},
		},
	}

	tests := []struct {
		name       string
		models     []string
		wantLabels []string
	}{
		{"no models selects none", nil, []string{"5h", "7d"}},
		{"one model", []string{"Fable"}, []string{"5h", "7d", "Fable"}},
		// Matched case-insensitively on display name, because the user types
		// these into settings and flags.
		{"case insensitive", []string{"fABLE"}, []string{"5h", "7d", "Fable"}},
		{"several models", []string{"Fable", "Opus"}, []string{"5h", "7d", "Fable", "Opus"}},
		{"the all sentinel", []string{AllModels}, []string{"5h", "7d", "Fable", "Opus"}},
		{"a model the account does not report", []string{"Sonnet"}, []string{"5h", "7d"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var labels []string
			for _, w := range r.Windows(tt.models) {
				labels = append(labels, w.Label)
			}
			if !reflect.DeepEqual(labels, tt.wantLabels) {
				t.Errorf("labels = %v, want %v", labels, tt.wantLabels)
			}
		})
	}
}

func TestHeadroom(t *testing.T) {
	tests := []struct {
		name         string
		result       *Result
		models       []string
		wantHeadroom float64
		wantOK       bool
	}{
		{
			// The binding window is the worst one, not the average.
			name:         "the worst window binds",
			result:       &Result{FiveHour: &Window{Pct: 10}, SevenDay: &Window{Pct: 75}},
			wantHeadroom: 25, wantOK: true,
		},
		{
			// A model maxed at 100% blocks that model's work even with plenty
			// of account-wide headroom.
			name:         "a maxed model binds when selected",
			result:       &Result{FiveHour: &Window{Pct: 10}, Scoped: []Scoped{{Name: "Fable", Pct: 100}}},
			models:       []string{"Fable"},
			wantHeadroom: 0, wantOK: true,
		},
		{
			name:         "the same model is invisible when not selected",
			result:       &Result{FiveHour: &Window{Pct: 10}, Scoped: []Scoped{{Name: "Fable", Pct: 100}}},
			wantHeadroom: 90, wantOK: true,
		},
		{
			name:         "over a limit reports negative headroom",
			result:       &Result{SevenDay: &Window{Pct: 110}},
			wantHeadroom: -10, wantOK: true,
		},
		// Unknown must never be confused with exhausted: callers treat it as
		// "do not skip this account", and a zero would mean the opposite.
		{name: "no window data is unknown", result: &Result{}, wantOK: false},
		{name: "a nil result is unknown", result: nil, wantOK: false},
		{
			name:   "spend alone is not window data",
			result: &Result{Spend: &Spend{Pct: 90}},
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.result.Headroom(tt.models)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && got != tt.wantHeadroom {
				t.Errorf("Headroom = %v, want %v", got, tt.wantHeadroom)
			}
		})
	}
}

func TestBindingPctIsTheComplementOfHeadroom(t *testing.T) {
	r := &Result{FiveHour: &Window{Pct: 10}, SevenDay: &Window{Pct: 75}}
	got, ok := r.BindingPct(nil)
	if !ok || got != 75 {
		t.Errorf("BindingPct = (%v, %v), want (75, true)", got, ok)
	}
	if _, ok := (&Result{}).BindingPct(nil); ok {
		t.Error("BindingPct reported a value with no window data")
	}
}

func TestEmpty(t *testing.T) {
	tests := []struct {
		name   string
		result *Result
		want   bool
	}{
		{"nil", nil, true},
		{"zero value", &Result{}, true},
		{"a five-hour window", &Result{FiveHour: &Window{}}, false},
		{"spend only", &Result{Spend: &Spend{}}, false},
		{"a scoped window only", &Result{Scoped: []Scoped{{Name: "Fable"}}}, false},
	}
	for _, tt := range tests {
		if got := tt.result.Empty(); got != tt.want {
			t.Errorf("%s: Empty() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestParseReset(t *testing.T) {
	want := time.Date(2026, 6, 15, 18, 30, 0, 0, time.UTC)
	tests := []struct {
		name   string
		in     string
		want   time.Time
		wantOK bool
	}{
		{"RFC 3339 with Z", "2026-06-15T18:30:00Z", want, true},
		{"an explicit UTC offset", "2026-06-15T18:30:00+00:00", want, true},
		{"fractional seconds", "2026-06-15T18:30:00.123Z", want.Add(123 * time.Millisecond), true},
		// A value with no zone is read as UTC: every timestamp this endpoint
		// sends is UTC, and guessing the local zone would shift a reset by hours.
		{"no zone", "2026-06-15T18:30:00", want, true},
		{"empty", "", time.Time{}, false},
		{"garbage", "not a timestamp", time.Time{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseReset(tt.in)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && !got.Equal(tt.want) {
				t.Errorf("ParseReset(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// A non-UTC offset must be honoured rather than reinterpreted.
func TestParseResetKeepsAnExplicitOffset(t *testing.T) {
	got, ok := ParseReset("2026-06-15T18:30:00+09:00")
	if !ok {
		t.Fatal("ParseReset rejected a valid offset timestamp")
	}
	if want := time.Date(2026, 6, 15, 9, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Errorf("ParseReset = %v, want %v", got.UTC(), want)
	}
}

// The JSON spelling is a cross-implementation contract: the Python version
// reads and writes the same lastGood payloads during the migration.
func TestJSONShapeMatchesThePythonPayload(t *testing.T) {
	const payload = `{
	  "five_hour": {"pct": 12.5, "resets_at": "2026-06-15T18:00:00Z"},
	  "seven_day": {"pct": 40, "resets_at": "2026-06-20T00:00:00Z"},
	  "spend": {"used": 1.5, "limit": 20, "pct": 7.5, "currency": "USD"},
	  "scoped": [{"name": "Fable", "pct": 80, "resets_at": "2026-06-20T00:00:00Z"}]
	}`

	var got Result
	if err := json.Unmarshal([]byte(payload), &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.FiveHour == nil || got.FiveHour.Pct != 12.5 {
		t.Errorf("five_hour = %+v", got.FiveHour)
	}
	if got.SevenDay == nil || got.SevenDay.ResetsAt != "2026-06-20T00:00:00Z" {
		t.Errorf("seven_day = %+v", got.SevenDay)
	}
	if got.Spend == nil || got.Spend.Currency != "USD" || got.Spend.Used != 1.5 {
		t.Errorf("spend = %+v", got.Spend)
	}
	if len(got.Scoped) != 1 || got.Scoped[0].Name != "Fable" {
		t.Errorf("scoped = %+v", got.Scoped)
	}
}

// A round trip must not invent keys the Python reader would not expect, and must
// keep the reset strings byte-identical.
func TestJSONRoundTrip(t *testing.T) {
	original := Result{
		FiveHour: &Window{Pct: 12.5, ResetsAt: "2026-06-15T18:00:00Z"},
		Scoped:   []Scoped{{Name: "Fable", Pct: 80}},
	}
	encoded, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}

	var back Result
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(original, back) {
		t.Errorf("round trip = %+v, want %+v", back, original)
	}

	// Absent sections stay absent rather than becoming nulls.
	var raw map[string]any
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"seven_day", "spend"} {
		if _, present := raw[key]; present {
			t.Errorf("%s was emitted for an absent section: %s", key, encoded)
		}
	}
	// The human-facing strings are recomputed at render time and must not be
	// persisted: cached ones drift as the measurement ages.
	five := raw["five_hour"].(map[string]any)
	for _, key := range []string{"countdown", "clock"} {
		if _, present := five[key]; present {
			t.Errorf("%s was persisted; it drifts and must be recomputed", key)
		}
	}
}
