package claudeapi

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"
)

// fakeRefresher stands in for the switcher's consume gate.
type fakeRefresher struct {
	calls    int
	lastSeen string
	outcome  func(call int) RefreshOutcome
}

func (f *fakeRefresher) Refresh(_ context.Context, _, _, credentials string) RefreshOutcome {
	f.calls++
	f.lastSeen = credentials
	return f.outcome(f.calls)
}

func always(o RefreshOutcome) func(int) RefreshOutcome {
	return func(int) RefreshOutcome { return o }
}

// expiredCreds is a slot whose access token expired while the machine was idle.
func expiredCreds(t *testing.T) string {
	t.Helper()
	return oauthCreds(t, map[string]any{
		"accessToken":  "EXPIRED",
		"refreshToken": "r",
		"expiresAt":    testNow.Add(-24 * time.Hour).UnixMilli(),
	})
}

func freshCreds(t *testing.T) string {
	t.Helper()
	return oauthCreds(t, map[string]any{
		"accessToken":  "FRESH",
		"refreshToken": "r",
		"expiresAt":    testNow.Add(2 * time.Hour).UnixMilli(),
	})
}

const usageBody = `{"five_hour":{"utilization":22},"seven_day":{"utilization":61}}`

func TestFetchRefreshesAnExpiredTokenFirst(t *testing.T) {
	var sawToken string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/usage") {
			sawToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			respondJSON(http.StatusOK, usageBody)(w, r)
			return
		}
		respondJSON(http.StatusOK, `{"access_token":"ROTATED","expires_in":3600}`)(w, r)
	})

	var persisted string
	out := c.FetchUsageForAccount(t.Context(), FetchRequest{
		AccountNum:  "2",
		Email:       "user@example.com",
		Credentials: expiredCreds(t),
		Now:         testNow,
		Persist: func(_, _, credentials string) error {
			persisted = credentials
			return nil
		},
	})

	if !out.OK() {
		t.Fatalf("fetch failed: %q", out.Error)
	}
	if sawToken != "ROTATED" {
		t.Errorf("the usage endpoint saw %q, want the rotated token", sawToken)
	}
	// The rotated credential has to reach disk, or the next pass POSTs a spent
	// grant and the account looks permanently broken.
	if AccessToken(persisted) != "ROTATED" {
		t.Errorf("persisted = %q, want the rotated credential", persisted)
	}
}

// Claude Code owns the active credential. Rotating one underneath a running
// editor invalidates the token it is holding.
func TestTheActiveAccountIsNeverRefreshed(t *testing.T) {
	tests := []struct {
		name       string
		usageState int
	}{
		{"even when the token is expired", http.StatusOK},
		{"even on a 401", http.StatusUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var grants int
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/token") {
					grants++
				}
				respondJSON(tt.usageState, usageBody)(w, r)
			})

			gate := &fakeRefresher{outcome: always(RefreshOutcome{Credentials: "x"})}
			out := c.FetchUsageForAccount(t.Context(), FetchRequest{
				AccountNum: "1", Credentials: expiredCreds(t), IsActive: true,
				Now: testNow, Refresher: gate,
			})

			if grants != 0 || gate.calls != 0 {
				t.Errorf("the active account was refreshed (%d direct, %d gated)", grants, gate.calls)
			}
			if tt.usageState == http.StatusUnauthorized && out.Error != HTTPKind(401) {
				t.Errorf("Error = %q, want http-401", out.Error)
			}
		})
	}
}

func TestFetchSkipsTheRefreshWhenTheTokenIsFresh(t *testing.T) {
	var grants int
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			grants++
		}
		respondJSON(http.StatusOK, usageBody)(w, r)
	})

	out := c.FetchUsageForAccount(t.Context(), FetchRequest{
		AccountNum: "2", Credentials: freshCreds(t), Now: testNow,
	})
	if !out.OK() {
		t.Fatalf("fetch failed: %q", out.Error)
	}
	if grants != 0 {
		t.Errorf("%d grants were spent on a token that had not expired", grants)
	}
}

// The server can rotate past a token the local expiry still calls fresh. One
// refresh and one retry is the whole recovery.
func TestA401RetriesOnceAfterRefreshing(t *testing.T) {
	var usageCalls int
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			respondJSON(http.StatusOK, `{"access_token":"ROTATED","expires_in":3600}`)(w, r)
			return
		}
		usageCalls++
		if usageCalls == 1 {
			respondJSON(http.StatusUnauthorized, `{}`)(w, r)
			return
		}
		respondJSON(http.StatusOK, usageBody)(w, r)
	})

	out := c.FetchUsageForAccount(t.Context(), FetchRequest{
		AccountNum: "2", Credentials: freshCreds(t), Now: testNow,
	})
	if !out.OK() {
		t.Fatalf("fetch failed: %q", out.Error)
	}
	if out.Usage == nil || out.Usage.SevenDay.Pct != 61 {
		t.Errorf("usage = %+v, want the retried measurement", out.Usage)
	}
	if usageCalls != 2 {
		t.Errorf("usage calls = %d, want exactly one retry", usageCalls)
	}
}

// A retry that fails again is not retried a third time: the budget is finite
// and the evidence is in.
func TestThe401RetryHappensOnlyOnce(t *testing.T) {
	var usageCalls, grants int
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			grants++
			respondJSON(http.StatusOK, `{"access_token":"ROTATED","expires_in":3600}`)(w, r)
			return
		}
		usageCalls++
		respondJSON(http.StatusUnauthorized, `{}`)(w, r)
	})

	out := c.FetchUsageForAccount(t.Context(), FetchRequest{
		AccountNum: "2", Credentials: freshCreds(t), Now: testNow,
	})
	if out.Error != HTTPKind(401) {
		t.Errorf("Error = %q, want http-401", out.Error)
	}
	if usageCalls != 2 || grants != 1 {
		t.Errorf("usage calls = %d, grants = %d; want 2 and 1", usageCalls, grants)
	}
}

// A permanent verdict short-circuits. Calling the usage endpoint with a token
// already known to be dead only adds a 401 to a lost cause — and spends a
// request from an hourly budget shared with every other account on the token.
func TestAPermanentRefreshVerdictSpendsNoRequest(t *testing.T) {
	tests := []ErrorKind{KindInvalidGrant, KindNoRefreshToken}
	for _, kind := range tests {
		t.Run(string(kind), func(t *testing.T) {
			var usageCalls int
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				usageCalls++
				respondJSON(http.StatusOK, usageBody)(w, r)
			})

			creds := expiredCreds(t)
			out := c.FetchUsageForAccount(t.Context(), FetchRequest{
				AccountNum: "2", Credentials: creds, Now: testNow,
				Refresher: &fakeRefresher{outcome: always(RefreshOutcome{Error: kind})},
			})

			if out.Error != kind {
				t.Errorf("Error = %q, want %q", out.Error, kind)
			}
			if usageCalls != 0 {
				t.Error("a doomed request was spent on a dead lineage")
			}
			// The strike has to name a generation, or the store cannot tell a
			// dead credential from the one that replaced it.
			if out.StruckFP != Fingerprint(creds) {
				t.Errorf("StruckFP = %q, want the fingerprint of the credential POSTed", out.StruckFP)
			}
		})
	}
}

// The gate may substitute a fresher re-read for the snapshot it was handed. The
// strike must bind to the bytes actually spent, not to what the caller thought
// it was spending.
func TestTheStrikeBindsToTheGenerationTheGateSpent(t *testing.T) {
	c, _ := newTestClient(t, respondJSON(http.StatusOK, usageBody))
	out := c.FetchUsageForAccount(t.Context(), FetchRequest{
		AccountNum: "2", Credentials: expiredCreds(t), Now: testNow,
		Refresher: &fakeRefresher{outcome: always(RefreshOutcome{
			Error:      KindInvalidGrant,
			ConsumedFP: "sha256:the-generation-actually-posted",
		})},
	})
	if out.StruckFP != "sha256:the-generation-actually-posted" {
		t.Errorf("StruckFP = %q, want the gate's consumed fingerprint", out.StruckFP)
	}
}

// A busy gate means the token in hand is known-expired and the retry would
// re-enter the same gate. Falling through would spend a guaranteed 401 per pass
// to learn nothing, and would arrive as the generic kind — hiding the remedy.
func TestADeterministicRefusalSpendsNoRequestAndKeepsItsKind(t *testing.T) {
	for _, kind := range deterministicRefreshKinds {
		t.Run(string(kind), func(t *testing.T) {
			var usageCalls int
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				usageCalls++
				respondJSON(http.StatusOK, usageBody)(w, r)
			})

			out := c.FetchUsageForAccount(t.Context(), FetchRequest{
				AccountNum: "2", Credentials: expiredCreds(t), Now: testNow,
				Refresher: &fakeRefresher{outcome: always(RefreshOutcome{Error: kind})},
			})
			if out.Error != kind {
				t.Errorf("Error = %q, want %q", out.Error, kind)
			}
			if usageCalls != 0 {
				t.Error("a doomed request was spent on a deterministic refusal")
			}
			if out.StruckFP != "" {
				t.Errorf("StruckFP = %q; a deterministic refusal is not evidence about the lineage", out.StruckFP)
			}
		})
	}
}

// A transient refresh failure is not evidence about the token, so the expired
// one is tried anyway: the server may disagree with the local clock.
func TestATransientRefreshFailureFallsThrough(t *testing.T) {
	var usageCalls int
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		usageCalls++
		respondJSON(http.StatusOK, usageBody)(w, r)
	})

	out := c.FetchUsageForAccount(t.Context(), FetchRequest{
		AccountNum: "2", Credentials: expiredCreds(t), Now: testNow,
		Refresher: &fakeRefresher{outcome: always(RefreshOutcome{Error: KindTransient})},
	})
	if !out.OK() {
		t.Fatalf("fetch failed: %q", out.Error)
	}
	if usageCalls != 1 {
		t.Errorf("usage calls = %d, want the expired token to have been tried", usageCalls)
	}
}

// On the 401 path a permanent or deterministic verdict keeps its own kind.
// Collapsing it to the generic one would hide the remedy exactly where it is
// needed most.
func TestThe401PathKeepsDistinctRefreshKinds(t *testing.T) {
	tests := []struct {
		kind         ErrorKind
		want         ErrorKind
		wantStruckFP bool
	}{
		{KindInvalidGrant, KindInvalidGrant, true},
		{KindNoRefreshToken, KindNoRefreshToken, true},
		{KindConsumeBusy, KindConsumeBusy, false},
		{KindInvalidClient, KindInvalidClient, false},
		{KindStashUnreadable, KindStashUnreadable, false},
		{KindStoreUnmirrored, KindStoreUnmirrored, false},
		// Only a genuinely unclassified failure becomes the generic kind.
		{KindTransient, KindRefreshFailed, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			c, _ := newTestClient(t, respondJSON(http.StatusUnauthorized, `{}`))
			out := c.FetchUsageForAccount(t.Context(), FetchRequest{
				AccountNum: "2", Credentials: freshCreds(t), Now: testNow,
				Refresher: &fakeRefresher{outcome: always(RefreshOutcome{Error: tt.kind})},
			})
			if out.Error != tt.want {
				t.Errorf("Error = %q, want %q", out.Error, tt.want)
			}
			if (out.StruckFP != "") != tt.wantStruckFP {
				t.Errorf("StruckFP = %q, want present = %v", out.StruckFP, tt.wantStruckFP)
			}
		})
	}
}

func TestFetchWithoutAnAccessToken(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"no OAuth payload", `{"other":1}`},
		{"no access token", `{"claudeAiOauth":{"refreshToken":"r"}}`},
		{"an empty access token", `{"claudeAiOauth":{"accessToken":""}}`},
		{"a managed API key", "sk-ant-api03-abc"},
		{"garbage", "not json"},
		{"empty", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls int
			c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
				calls++
				respondJSON(http.StatusOK, usageBody)(w, r)
			})
			out := c.FetchUsageForAccount(t.Context(), FetchRequest{
				AccountNum: "2", Credentials: tt.in, Now: testNow,
			})
			if out.Error != KindNoAccessToken {
				t.Errorf("Error = %q, want %q", out.Error, KindNoAccessToken)
			}
			if calls != 0 {
				t.Error("a request went out with no token to send")
			}
		})
	}
}

// A 401 with no refresh token in hand has no recovery, so it reports the
// server's answer rather than pretending a refresh was attempted.
func TestA401WithNoRefreshTokenReportsTheHTTPKind(t *testing.T) {
	var usageCalls int
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		usageCalls++
		respondJSON(http.StatusUnauthorized, `{}`)(w, r)
	})
	out := c.FetchUsageForAccount(t.Context(), FetchRequest{
		AccountNum: "2", Now: testNow,
		Credentials: oauthCreds(t, map[string]any{"accessToken": "a"}),
	})
	if out.Error != HTTPKind(401) {
		t.Errorf("Error = %q, want http-401", out.Error)
	}
	if usageCalls != 1 {
		t.Errorf("usage calls = %d, want no retry", usageCalls)
	}
}

// A 429 is the budget speaking, not the token. It carries the server's own
// backoff straight through to the scheduler.
func TestA429CarriesRetryAfterThrough(t *testing.T) {
	c, _ := newTestClient(t, respondJSON(429, `{}`, [2]string{"Retry-After", "45"}))
	out := c.FetchUsageForAccount(t.Context(), FetchRequest{
		AccountNum: "2", Credentials: freshCreds(t), Now: testNow,
	})
	if out.Error != HTTPKind(429) {
		t.Errorf("Error = %q, want http-429", out.Error)
	}
	if out.RetryAfter == nil || *out.RetryAfter != 45*time.Second {
		t.Errorf("RetryAfter = %v, want 45s", out.RetryAfter)
	}
}

// The gate persists internally through a fingerprint compare-and-swap. Writing
// again from here would land behind it.
func TestTheGateSupersedesBothThePostAndThePersist(t *testing.T) {
	var grants int
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			grants++
		}
		respondJSON(http.StatusOK, usageBody)(w, r)
	})

	rotated := oauthCreds(t, map[string]any{"accessToken": "GATE-ROTATED", "refreshToken": "r2"})
	gate := &fakeRefresher{outcome: always(RefreshOutcome{Credentials: rotated})}
	var persisted int

	out := c.FetchUsageForAccount(t.Context(), FetchRequest{
		AccountNum: "2", Credentials: expiredCreds(t), Now: testNow,
		Refresher: gate,
		Persist:   func(_, _, _ string) error { persisted++; return nil },
	})

	if !out.OK() {
		t.Fatalf("fetch failed: %q", out.Error)
	}
	if grants != 0 {
		t.Errorf("%d direct grants were POSTed despite a gate being supplied", grants)
	}
	if gate.calls != 1 {
		t.Errorf("gate calls = %d, want 1", gate.calls)
	}
	if persisted != 0 {
		t.Error("the persist callback wrote behind the gate's compare-and-swap")
	}
}

// A persist failure leaves a spent refresh token on disk, so the next pass
// POSTs a dead grant and the account looks permanently broken for a reason that
// has nothing to do with the account. The user has to be told.
func TestAPersistFailureWarnsTheUserWithARecoveryHint(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/token") {
			respondJSON(http.StatusOK, `{"access_token":"ROTATED","expires_in":3600}`)(w, r)
			return
		}
		respondJSON(http.StatusOK, usageBody)(w, r)
	})

	var warnings []string
	out := c.FetchUsageForAccount(t.Context(), FetchRequest{
		AccountNum: "2", Email: "user@example.com", Credentials: expiredCreds(t), Now: testNow,
		Persist: func(_, _, _ string) error { return context.DeadlineExceeded },
		Warn:    func(message string) { warnings = append(warnings, message) },
	})

	// The fetch itself still succeeds — the rotated token works for this pass.
	if !out.OK() {
		t.Fatalf("fetch failed: %q", out.Error)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", warnings)
	}
	for _, want := range []string{"account 2", "user@example.com", "cswap add"} {
		if !strings.Contains(warnings[0], want) {
			t.Errorf("the warning does not mention %q: %s", want, warnings[0])
		}
	}
}

// A successful round trip whose response carried no window data is not a
// failure: the account simply has no limits to report.
func TestNoWindowDataIsASuccess(t *testing.T) {
	c, _ := newTestClient(t, respondJSON(http.StatusOK, `{}`))
	out := c.FetchUsageForAccount(t.Context(), FetchRequest{
		AccountNum: "2", Credentials: freshCreds(t), Now: testNow,
	})
	if !out.OK() {
		t.Errorf("Error = %q, want success", out.Error)
	}
	if out.Usage != nil {
		t.Errorf("Usage = %+v, want nil", out.Usage)
	}
}
