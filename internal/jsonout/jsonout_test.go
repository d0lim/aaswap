package jsonout

import (
	json "encoding/json/v2"
	"testing"
	"time"

	"github.com/d0lim/ccswap/internal/usage"
)

var now = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// decode round-trips a payload through JSON, which is what a consumer sees.
func decode(t *testing.T, payload any) map[string]any {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("%v\n%s", err, data)
	}
	return out
}

func TestProjectUsage(t *testing.T) {
	reset := now.Add(3 * time.Hour).Format(time.RFC3339)
	result := &usage.Result{
		FiveHour: &usage.Window{Pct: 22, ResetsAt: reset},
		SevenDay: &usage.Window{Pct: 61, ResetsAt: now.Add(48 * time.Hour).Format(time.RFC3339)},
		Spend:    &usage.Spend{Used: 7.29, Limit: 50, Pct: 14.58, Currency: "USD"},
		Scoped:   []usage.Scoped{{Name: "Fable", Pct: 100, ResetsAt: reset}},
	}
	got := decode(t, ProjectUsage(result, now, now))

	five := got["fiveHour"].(map[string]any)
	if five["pct"] != 22.0 || five["resetsAt"] != reset {
		t.Errorf("fiveHour = %v", five)
	}
	// The reset strings are recomputed at serialization, not carried from the
	// fetch.
	if five["countdown"] != "3h 0m" {
		t.Errorf("countdown = %v", five["countdown"])
	}
	if got["spend"].(map[string]any)["currency"] != "USD" {
		t.Errorf("spend = %v", got["spend"])
	}
	scoped := got["scoped"].([]any)[0].(map[string]any)
	if scoped["name"] != "Fable" || scoped["pct"] != 100.0 {
		t.Errorf("scoped = %v", scoped)
	}
}

// Pace rides on the weekly windows only: a five-hour window has no weekly cycle
// to be ahead of.
func TestPaceIsWeeklyOnly(t *testing.T) {
	weekly := now.Add(5 * 24 * time.Hour).Format(time.RFC3339)
	result := &usage.Result{
		FiveHour: &usage.Window{Pct: 90, ResetsAt: now.Add(time.Hour).Format(time.RFC3339)},
		SevenDay: &usage.Window{Pct: 90, ResetsAt: weekly},
		Scoped:   []usage.Scoped{{Name: "Fable", Pct: 90, ResetsAt: weekly}},
	}
	got := decode(t, ProjectUsage(result, now, now))

	if _, present := got["fiveHour"].(map[string]any)["expectedPct"]; present {
		t.Errorf("the five-hour window carries pace fields: %v", got["fiveHour"])
	}
	seven := got["sevenDay"].(map[string]any)
	for _, key := range []string{"expectedPct", "aheadOfPace"} {
		if _, present := seven[key]; !present {
			t.Errorf("the weekly window is missing %q: %v", key, seven)
		}
	}
	scoped := got["scoped"].([]any)[0].(map[string]any)
	if _, present := scoped["expectedPct"]; !present {
		t.Errorf("a scoped weekly window carries no pace fields: %v", scoped)
	}
	// 90% used two days into a week is well ahead of pace.
	if seven["aheadOfPace"] != true {
		t.Errorf("aheadOfPace = %v", seven["aheadOfPace"])
	}
}

// Without a fetch time there is no pace to compute, and inventing one from the
// serialization time would be wrong by however long the measurement sat.
func TestNoFetchTimeMeansNoPace(t *testing.T) {
	result := &usage.Result{
		SevenDay: &usage.Window{Pct: 90, ResetsAt: now.Add(5 * 24 * time.Hour).Format(time.RFC3339)},
	}
	got := decode(t, ProjectUsage(result, time.Time{}, now))
	if _, present := got["sevenDay"].(map[string]any)["expectedPct"]; present {
		t.Errorf("pace was computed with no fetch time: %v", got["sevenDay"])
	}
}

// Absent sections stay absent rather than becoming nulls: a consumer
// distinguishes them.
func TestAbsentSectionsAreOmitted(t *testing.T) {
	got := decode(t, ProjectUsage(&usage.Result{FiveHour: &usage.Window{Pct: 10}}, now, now))
	for _, key := range []string{"sevenDay", "spend", "scoped"} {
		if _, present := got[key]; present {
			t.Errorf("%q was emitted for an absent section: %v", key, got)
		}
	}
}

func TestUsageFields(t *testing.T) {
	tests := []struct {
		name     string
		decision Decision
		want     string
		wantData bool
	}{
		{"a measurement", Decision{Usage: &usage.Result{FiveHour: &usage.Window{Pct: 10}}}, StatusOK, true},
		{"an expired token", Decision{Sentinel: "token expired"}, StatusTokenExpired, false},
		{"an API key", Decision{Sentinel: "api key"}, StatusAPIKey, false},
		{"an unreadable keychain", Decision{Sentinel: "keychain unavailable"}, StatusKeychainUnavailable, false},
		{"a dead lineage", Decision{Sentinel: "re-login needed"}, StatusReloginRequired, false},
		{"a foreign credential", Decision{Sentinel: "foreign credential"}, StatusForeignCredential, false},
		{"an empty slot", Decision{Sentinel: "no credentials"}, StatusNoCredentials, false},
		// Nothing at all is a fetch failure, which is distinct from every
		// stated reason above.
		{"nothing known", Decision{}, StatusUnavailable, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, projected := UsageFields(tt.decision, now, now)
			if status != tt.want {
				t.Errorf("status = %q, want %q", status, tt.want)
			}
			if (projected != nil) != tt.wantData {
				t.Errorf("usage present = %v, want %v", projected != nil, tt.wantData)
			}
		})
	}
}

// A served measurement and a display-only one are reported under different
// names, and never both — that separation is what stops a script acting on old
// data.
func TestFreshnessFieldsAreMutuallyExclusive(t *testing.T) {
	measurement := &usage.Result{FiveHour: &usage.Window{Pct: 10}}

	t.Run("a served measurement", func(t *testing.T) {
		row := AccountRow{Usage: ProjectUsage(measurement, now, now)}
		row.SetFreshness(measurement, now, 30*time.Second, now)
		if row.UsageFetchedAt == "" || row.UsageAgeSeconds == nil {
			t.Error("the served measurement has no freshness fields")
		}
		if row.LastGoodUsage != nil || row.LastGoodFetchedAt != "" {
			t.Error("a served measurement also filled the display-only fields")
		}
	})

	t.Run("a withheld measurement", func(t *testing.T) {
		row := AccountRow{Usage: nil}
		row.SetFreshness(measurement, now, time.Hour, now)
		if row.LastGoodUsage == nil || row.LastGoodFetchedAt == "" || row.LastGoodAgeSeconds == nil {
			t.Error("the display-only fields are missing")
		}
		if row.UsageFetchedAt != "" || row.UsageAgeSeconds != nil {
			t.Error("a withheld measurement filled the served-freshness fields")
		}
	})

	t.Run("nothing ever measured", func(t *testing.T) {
		row := AccountRow{}
		row.SetFreshness(nil, time.Time{}, 0, now)
		if row.UsageFetchedAt != "" || row.LastGoodFetchedAt != "" {
			t.Errorf("freshness was reported for a never-measured account: %+v", row)
		}
	})
}

// Null and absent mean different things, and both occur.
func TestNullIsDistinctFromAbsent(t *testing.T) {
	t.Run("a listing with no active account", func(t *testing.T) {
		got := decode(t, ListPayload{SchemaVersion: SchemaVersion})
		value, present := got["activeAccountNumber"]
		if !present {
			t.Error("activeAccountNumber was omitted; null is the answer, not absence")
		}
		if value != nil {
			t.Errorf("activeAccountNumber = %v, want null", value)
		}
	})

	t.Run("a status with no live login", func(t *testing.T) {
		got := decode(t, StatusPayload{SchemaVersion: SchemaVersion})
		value, present := got["active"]
		if !present || value != nil {
			t.Errorf("active = (%v, present=%v), want an explicit null", value, present)
		}
		// Nothing is managed, so the count is not asserted either way.
		if _, present := got["totalManagedAccounts"]; present {
			t.Error("a count was reported with no roster to count")
		}
	})

	t.Run("an unmanaged live login has no slot number", func(t *testing.T) {
		got := decode(t, StatusPayload{
			SchemaVersion: SchemaVersion,
			Active:        &ActiveStatus{Email: "a@example.com", Managed: false},
		})
		active := got["active"].(map[string]any)
		if _, present := active["number"]; present {
			t.Errorf("an unmanaged account was given a slot number: %v", active)
		}
		if active["managed"] != false {
			t.Errorf("managed = %v", active["managed"])
		}
	})
}

// A switch's departing side is null on a machine that had no live login, and
// numberless for one ccswap did not manage.
func TestSwitchPayloadSides(t *testing.T) {
	got := decode(t, SwitchPayload{
		SchemaVersion: SchemaVersion, Switched: true,
		To: AccountRef{Number: new(2), Email: "b@example.com"},
	})
	if value, present := got["from"]; !present || value != nil {
		t.Errorf("from = (%v, present=%v), want an explicit null", value, present)
	}

	got = decode(t, SwitchPayload{
		SchemaVersion: SchemaVersion, Switched: true,
		From: &AccountRef{Email: "unmanaged@example.com"},
		To:   AccountRef{Number: new(2), Email: "b@example.com"},
	})
	from := got["from"].(map[string]any)
	if from["number"] != nil {
		t.Errorf("number = %v, want null for an unmanaged account", from["number"])
	}
	if from["email"] != "unmanaged@example.com" {
		t.Errorf("email = %v", from["email"])
	}
}

func TestTimestampSpelling(t *testing.T) {
	if got := Timestamp(now); got != "2026-06-15T12:00:00Z" {
		t.Errorf("Timestamp = %q", got)
	}
	// Rendered in UTC whatever zone it arrives in, so two machines agree.
	tokyo := time.FixedZone("JST", 9*3600)
	if got := Timestamp(now.In(tokyo)); got != "2026-06-15T12:00:00Z" {
		t.Errorf("Timestamp in another zone = %q", got)
	}
}

func TestRound1(t *testing.T) {
	tests := []struct {
		in   float64
		want float64
	}{
		{12.34, 12.3}, {12.35, 12.4}, {12.0, 12.0}, {-12.34, -12.3}, {0, 0},
	}
	for _, tt := range tests {
		if got := round1(tt.in); got != tt.want {
			t.Errorf("round1(%v) = %v, want %v", tt.in, got, tt.want)
		}
	}
}
