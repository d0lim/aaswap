package claudeapi

import (
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/d0lim/ccswap/internal/usage"
)

func TestFetchUsageNormalizesTheResponse(t *testing.T) {
	c, rec := newTestClient(t, respondJSON(http.StatusOK, `{
		"five_hour": {"utilization": 22.0, "resets_at": "2026-03-23T17:00:00Z"},
		"seven_day": {"utilization": 61.0, "resets_at": "2026-03-28T00:00:00Z"},
		"extra_usage": {"is_enabled": true, "used_credits": 72900,
		                "monthly_limit": 500000, "utilization": 14.58, "currency": "USD"},
		"limits": [
			{"kind": "session", "percent": 22, "scope": null},
			{"kind": "weekly_all", "percent": 61, "scope": null},
			{"kind": "weekly_scoped", "percent": 100, "resets_at": "2026-03-28T00:00:00Z",
			 "scope": {"model": {"id": null, "display_name": "Fable"}, "surface": null}}
		]
	}`))

	got, err := c.FetchUsage(t.Context(), "token")
	if err != nil {
		t.Fatal(err)
	}
	want := &usage.Result{
		FiveHour: &usage.Window{Pct: 22, ResetsAt: "2026-03-23T17:00:00Z"},
		SevenDay: &usage.Window{Pct: 61, ResetsAt: "2026-03-28T00:00:00Z"},
		// Credits arrive as hundredths of a currency unit.
		Spend:  &usage.Spend{Used: 729, Limit: 5000, Pct: 14.58, Currency: "USD"},
		Scoped: []usage.Scoped{{Name: "Fable", Pct: 100, ResetsAt: "2026-03-28T00:00:00Z"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FetchUsage =\n  %+v\nwant\n  %+v", got, want)
	}

	if auth := rec.header.Get("Authorization"); auth != "Bearer token" {
		t.Errorf("Authorization = %q", auth)
	}
	if beta := rec.header.Get("anthropic-beta"); beta != BetaHeader {
		t.Errorf("anthropic-beta = %q, want %q", beta, BetaHeader)
	}
}

// Only model-scoped entries become scoped windows. The session and weekly_all
// entries duplicate five_hour and seven_day, and counting them twice would make
// the binding window look tighter than it is.
func TestOnlyModelScopedLimitsSurface(t *testing.T) {
	tests := []struct {
		name  string
		limit string
		want  int
	}{
		{"a model-scoped entry", `{"percent":100,"scope":{"model":{"display_name":"Fable"}}}`, 1},
		{"a null scope", `{"percent":61,"scope":null}`, 0},
		{"a scope with no model", `{"percent":61,"scope":{"surface":null}}`, 0},
		{"a model with no display name", `{"percent":61,"scope":{"model":{"id":"x"}}}`, 0},
		{"an entry with no percent", `{"scope":{"model":{"display_name":"Fable"}}}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, respondJSON(http.StatusOK,
				`{"five_hour":{"utilization":1},"limits":[`+tt.limit+`]}`))
			got, err := c.FetchUsage(t.Context(), "token")
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Scoped) != tt.want {
				t.Errorf("scoped windows = %d, want %d (%+v)", len(got.Scoped), tt.want, got.Scoped)
			}
		})
	}
}

// A malformed section is dropped and the rest goes through. A partial answer
// about the windows that DO gate this account beats spending a request from the
// hourly budget and storing nothing.
func TestOneBadSectionDoesNotSinkTheRest(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantSpend bool
	}{
		{
			"spend complete",
			`{"is_enabled":true,"used_credits":72900,"monthly_limit":500000,"utilization":14.58}`,
			true,
		},
		// monthly_limit null means unlimited, which has no percentage to show.
		{
			"unlimited drops only the spend line",
			`{"is_enabled":true,"used_credits":72900,"monthly_limit":null,"utilization":null}`,
			false,
		},
		{
			"a null in used_credits drops only the spend line",
			`{"is_enabled":true,"used_credits":null,"monthly_limit":500000,"utilization":14.58}`,
			false,
		},
		{
			"disabled suppresses spend even with valid numbers",
			`{"is_enabled":false,"used_credits":72900,"monthly_limit":500000,"utilization":14.58}`,
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, respondJSON(http.StatusOK,
				`{"five_hour":{"utilization":22},"seven_day":{"utilization":61},"extra_usage":`+tt.body+`}`))
			got, err := c.FetchUsage(t.Context(), "token")
			if err != nil {
				t.Fatal(err)
			}
			if (got.Spend != nil) != tt.wantSpend {
				t.Errorf("spend present = %v, want %v", got.Spend != nil, tt.wantSpend)
			}
			// Whatever happened to spend, the gating windows are intact.
			if got.FiveHour == nil || got.FiveHour.Pct != 22 || got.SevenDay == nil || got.SevenDay.Pct != 61 {
				t.Errorf("the gating windows were disturbed: %+v", got)
			}
		})
	}
}

// A window object with no utilization is malformed. Reading it as zero would
// tell the user they have a full budget they may not have.
func TestAWindowWithNoUtilizationIsDroppedNotZeroed(t *testing.T) {
	c, _ := newTestClient(t, respondJSON(http.StatusOK,
		`{"five_hour":{"resets_at":"2026-03-23T17:00:00Z"},"seven_day":{"utilization":61}}`))
	got, err := c.FetchUsage(t.Context(), "token")
	if err != nil {
		t.Fatal(err)
	}
	if got.FiveHour != nil {
		t.Errorf("five_hour = %+v, want it dropped rather than read as 0%%", got.FiveHour)
	}
	if got.SevenDay == nil || got.SevenDay.Pct != 61 {
		t.Errorf("seven_day = %+v, want it intact", got.SevenDay)
	}
	// The binding window is the seven-day one, not a phantom 0%.
	if pct, ok := got.BindingPct(nil); !ok || pct != 61 {
		t.Errorf("BindingPct = (%v, %v), want (61, true)", pct, ok)
	}
}

func TestFetchUsageEdgeShapes(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"an empty response", `{}`},
		{"every section null", `{"five_hour":null,"seven_day":null,"extra_usage":null,"limits":null}`},
		{"limits present but yielding nothing", `{"limits":[{"percent":1,"scope":null}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, respondJSON(http.StatusOK, tt.body))
			got, err := c.FetchUsage(t.Context(), "token")
			// A round trip that carried no window data is a success with no
			// result, not a failure: the account simply has no limits to report.
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != nil {
				t.Errorf("FetchUsage = %+v, want nil", got)
			}
		})
	}
}

// A window with no resets_at still reports its percentage. The percentage is
// what gates the account; the reset is what schedules the next poll.
func TestAWindowWithNoResetStillReportsAPercentage(t *testing.T) {
	c, _ := newTestClient(t, respondJSON(http.StatusOK,
		`{"five_hour":{"utilization":0.0,"resets_at":null},"seven_day":{"utilization":100.0,"resets_at":"2026-03-24T10:00:00Z"}}`))
	got, err := c.FetchUsage(t.Context(), "token")
	if err != nil {
		t.Fatal(err)
	}
	if got.FiveHour == nil || got.FiveHour.Pct != 0 || got.FiveHour.ResetsAt != "" {
		t.Errorf("five_hour = %+v", got.FiveHour)
	}
	if _, ok := got.FiveHour.ResetTime(); ok {
		t.Error("a window with no reset reported a reset time")
	}
	if _, ok := got.SevenDay.ResetTime(); !ok {
		t.Error("a window with a reset did not report one")
	}
}

func TestClassifyTransportFailures(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		headers    [][2]string
		wantKind   ErrorKind
		wantRetry  *time.Duration
		checkRetry bool
	}{
		{name: "a 429", status: 429, body: `{}`, wantKind: HTTPKind(429), checkRetry: true},
		{name: "a 500", status: 500, body: `{}`, wantKind: HTTPKind(500), checkRetry: true},
		{name: "a 401", status: 401, body: `{}`, wantKind: HTTPKind(401), checkRetry: true},
		{
			name: "Retry-After in seconds", status: 429, body: `{}`,
			headers:  [][2]string{{"Retry-After", "30"}},
			wantKind: HTTPKind(429), wantRetry: new(30 * time.Second), checkRetry: true,
		},
		{
			// The HTTP-date form is ignored rather than misparsed into a wildly
			// wrong backoff.
			name: "Retry-After as a date is ignored", status: 429, body: `{}`,
			headers:  [][2]string{{"Retry-After", "Fri, 04 Jul 2026 12:00:00 GMT"}},
			wantKind: HTTPKind(429), checkRetry: true,
		},
		{
			// The server is saying "now", not "never".
			name: "a negative Retry-After clamps to zero", status: 429, body: `{}`,
			headers:  [][2]string{{"Retry-After", "-5"}},
			wantKind: HTTPKind(429), wantRetry: new(time.Duration(0)), checkRetry: true,
		},
		{
			name: "an undecodable body", status: 200, body: `not json`,
			wantKind: KindBadResponse, checkRetry: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, respondJSON(tt.status, tt.body, tt.headers...))
			_, err := c.FetchUsage(t.Context(), "token")
			if err == nil {
				t.Fatal("the fetch succeeded")
			}
			kind, retry := classify(err)
			if kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", kind, tt.wantKind)
			}
			if !tt.checkRetry {
				return
			}
			switch {
			case tt.wantRetry == nil && retry != nil:
				t.Errorf("retry-after = %v, want none", *retry)
			case tt.wantRetry != nil && retry == nil:
				t.Errorf("retry-after = none, want %v", *tt.wantRetry)
			case tt.wantRetry != nil && *retry != *tt.wantRetry:
				t.Errorf("retry-after = %v, want %v", *retry, *tt.wantRetry)
			}
		})
	}

	t.Run("a network failure", func(t *testing.T) {
		c, _ := newTestClient(t, respondJSON(200, `{}`))
		c.UsageURL = "http://127.0.0.1:1/usage"
		_, err := c.FetchUsage(t.Context(), "token")
		if kind, _ := classify(err); kind != KindNetwork {
			t.Errorf("kind = %q, want %q", kind, KindNetwork)
		}
	})

	t.Run("a timeout", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		})
		c.UsageTimeout = 50 * time.Millisecond
		_, err := c.FetchUsage(t.Context(), "token")
		if kind, _ := classify(err); kind != KindTimeout {
			t.Errorf("kind = %q, want %q", kind, KindTimeout)
		}
	})
}

// A Retry-After of zero and no Retry-After at all mean different things, so the
// two must stay distinguishable at the type level.
func TestAbsentAndZeroRetryAfterAreDistinct(t *testing.T) {
	absent := respondJSON(429, `{}`)
	zero := respondJSON(429, `{}`, [2]string{"Retry-After", "0"})

	c, _ := newTestClient(t, absent)
	_, err := c.FetchUsage(t.Context(), "token")
	if _, retry := classify(err); retry != nil {
		t.Errorf("an absent Retry-After produced %v", *retry)
	}

	c, _ = newTestClient(t, zero)
	_, err = c.FetchUsage(t.Context(), "token")
	_, retry := classify(err)
	if retry == nil || *retry != 0 {
		t.Errorf("a zero Retry-After produced %v, want a present zero", retry)
	}
}
