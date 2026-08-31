package claudeapi

import (
	json "encoding/json/v2"
	"net/http"
	"reflect"
	"slices"
	"testing"
	"time"
)

func TestRefreshRotatesTheCredential(t *testing.T) {
	c, rec := newTestClient(t, respondJSON(http.StatusOK, `{
		"access_token": "NEW-ACCESS",
		"refresh_token": "NEW-REFRESH",
		"expires_in": 3600,
		"scope": "user:inference user:profile"
	}`))

	// A sibling Claude Code owns, and a member inside the OAuth payload this
	// version has never heard of: both must survive untouched.
	original := oauthCreds(t,
		map[string]any{"accessToken": "OLD-ACCESS", "refreshToken": "OLD-REFRESH", "futureField": "keep me"},
		map[string]any{"trustedDeviceToken": "device-token"},
	)

	out := c.Refresh(t.Context(), original, testNow)
	if !out.OK() {
		t.Fatalf("Refresh failed: %q", out.Error)
	}

	var doc map[string]any
	if err := json.Unmarshal([]byte(out.Credentials), &doc); err != nil {
		t.Fatal(err)
	}
	payload := doc[oauthKey].(map[string]any)

	if payload["accessToken"] != "NEW-ACCESS" {
		t.Errorf("accessToken = %v, want the rotated one", payload["accessToken"])
	}
	if payload["refreshToken"] != "NEW-REFRESH" {
		t.Errorf("refreshToken = %v, want the rotated one", payload["refreshToken"])
	}
	if want := float64(testNow.Add(time.Hour).UnixMilli()); payload["expiresAt"] != want {
		t.Errorf("expiresAt = %v, want %v", payload["expiresAt"], want)
	}
	if got, want := payload["scopes"], []any{"user:inference", "user:profile"}; !reflect.DeepEqual(got, want) {
		t.Errorf("scopes = %v, want %v", got, want)
	}
	// The whole reason the document is a map and not a struct.
	if payload["futureField"] != "keep me" {
		t.Error("an unknown OAuth member was dropped by the refresh")
	}
	if doc["trustedDeviceToken"] != "device-token" {
		t.Error("a sibling Claude Code owns was dropped by the refresh")
	}

	// The grant body itself.
	var sent map[string]string
	if err := json.Unmarshal([]byte(rec.body), &sent); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": "OLD-REFRESH",
		"client_id":     ClientID,
	}
	if !reflect.DeepEqual(sent, want) {
		t.Errorf("grant body = %v, want %v", sent, want)
	}
	if rec.method != http.MethodPost {
		t.Errorf("method = %s, want POST", rec.method)
	}
	if got := rec.header.Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	if got := rec.header.Get("User-Agent"); got != userAgent {
		t.Errorf("User-Agent = %q, want %q", got, userAgent)
	}
}

// The endpoint rotates the refresh token on some grants and not others. Keeping
// the old one when none comes back is what makes the next grant work at all.
func TestRefreshKeepsTheOldRefreshTokenWhenNoneIsReturned(t *testing.T) {
	c, _ := newTestClient(t, respondJSON(http.StatusOK,
		`{"access_token": "NEW", "expires_in": 3600}`))

	out := c.Refresh(t.Context(), oauthCreds(t, map[string]any{
		"accessToken": "OLD", "refreshToken": "KEEP-ME", "scopes": []any{"user:inference"},
	}), testNow)
	if !out.OK() {
		t.Fatalf("Refresh failed: %q", out.Error)
	}

	payload, _ := OAuthPayload(out.Credentials)
	if payload["refreshToken"] != "KEEP-ME" {
		t.Errorf("refreshToken = %v, want the original", payload["refreshToken"])
	}
	// Same for the scopes: an omitted scope string is "unchanged", not "none".
	if got, want := payload["scopes"], []any{"user:inference"}; !reflect.DeepEqual(got, want) {
		t.Errorf("scopes = %v, want %v", got, want)
	}
	// The fingerprint tracks the refresh lineage, so an unrotated refresh token
	// means the same fingerprint before and after.
	if got := Fingerprint(out.Credentials); got != Fingerprint(oauthCreds(t, map[string]any{"refreshToken": "KEEP-ME"})) {
		t.Error("the fingerprint moved even though the refresh token did not")
	}
}

func TestRefreshClassification(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   ErrorKind
	}{
		{
			// RFC 6749 §5.2: the verdict is the body's top-level "error".
			name: "invalid_grant on a 400 is permanent", status: 400,
			body: `{"error":"invalid_grant","error_description":"expired"}`, want: KindInvalidGrant,
		},
		{
			name: "invalid_grant on a 401 is permanent too", status: 401,
			body: `{"error":"invalid_grant"}`, want: KindInvalidGrant,
		},
		{
			// Systemic: our client was rejected, which is evidence about no
			// particular account, so it keeps its own kind and lands no strike.
			name: "invalid_client is its own kind", status: 400,
			body: `{"error":"invalid_client"}`, want: KindInvalidClient,
		},
		{
			// A substring scan would call this permanent and quarantine a live
			// account on the strength of someone else's detail text.
			name: "the marker inside another envelope is transient", status: 400,
			body: `{"error":"invalid_request","error_description":"not invalid_grant"}`, want: KindTransient,
		},
		{
			name: "a 400 with no marker is transient", status: 400,
			body: `{"detail":"something went wrong"}`, want: KindTransient,
		},
		{
			// A misclassified transient costs one retry; a misclassified
			// permanent locks a user out of a live account.
			name: "an unparseable body is transient", status: 400,
			body: `<html>gateway error</html>`, want: KindTransient,
		},
		{
			name: "a 5xx is transient even carrying the marker", status: 500,
			body: `{"error":"invalid_grant"}`, want: KindTransient,
		},
		{
			name: "a 429 is transient", status: 429, body: `{}`, want: KindTransient,
		},
		{
			// A 200 that is not a grant is a server anomaly, not a verdict.
			name: "a success with no access token is transient", status: 200,
			body: `{"expires_in":3600}`, want: KindTransient,
		},
		{
			name: "an undecodable success body is transient", status: 200,
			body: `not json`, want: KindTransient,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, respondJSON(tt.status, tt.body))
			out := c.Refresh(t.Context(), oauthCreds(t, map[string]any{"refreshToken": "r"}), testNow)
			if out.Error != tt.want {
				t.Errorf("Error = %q, want %q", out.Error, tt.want)
			}
			if out.Credentials != "" {
				t.Error("a failed grant returned credentials")
			}
		})
	}
}

func TestRefreshWithoutAUsableRefreshToken(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want ErrorKind
	}{
		// Permanent, so it demands a structurally complete OAuth object that
		// genuinely lacks the field.
		{"a complete payload with no refresh token",
			`{"claudeAiOauth":{"accessToken":"a"}}`, KindNoRefreshToken},
		{"an empty refresh token",
			`{"claudeAiOauth":{"refreshToken":""}}`, KindNoRefreshToken},
		{"a document with no OAuth payload",
			`{"other":1}`, KindNoRefreshToken},
		// A torn or partial read is far more likely than a real credential of
		// this shape, and permanence would quarantine a live account over it.
		{"an unparseable blob", `{"claudeAiOauth":`, KindTransient},
		{"a non-object payload", `[1,2,3]`, KindTransient},
		{"a bare string", `sk-ant-api03-abc`, KindTransient},
		{"empty", "", KindTransient},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, rec := newTestClient(t, respondJSON(http.StatusOK, `{"access_token":"x","expires_in":1}`))
			out := c.Refresh(t.Context(), tt.in, testNow)
			if out.Error != tt.want {
				t.Errorf("Error = %q, want %q", out.Error, tt.want)
			}
			if rec.calls != 0 {
				t.Error("a grant was POSTed for a credential with no refresh token")
			}
		})
	}
}

func TestRefreshTransportFailures(t *testing.T) {
	t.Run("a network failure is transient", func(t *testing.T) {
		c, _ := newTestClient(t, respondJSON(http.StatusOK, `{}`))
		c.TokenURL = "http://127.0.0.1:1/token" // nothing listening
		out := c.Refresh(t.Context(), oauthCreds(t, map[string]any{"refreshToken": "r"}), testNow)
		if out.Error != KindTransient {
			t.Errorf("Error = %q, want %q", out.Error, KindTransient)
		}
	})

	t.Run("a timeout is transient", func(t *testing.T) {
		c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
			<-r.Context().Done()
		})
		c.RefreshTimeout = 50 * time.Millisecond
		out := c.Refresh(t.Context(), oauthCreds(t, map[string]any{"refreshToken": "r"}), testNow)
		if out.Error != KindTransient {
			t.Errorf("Error = %q, want %q", out.Error, KindTransient)
		}
	})
}

func TestRefreshOutcomeVerdicts(t *testing.T) {
	tests := []struct {
		kind              ErrorKind
		permanent, determ bool
	}{
		{KindInvalidGrant, true, false},
		{KindNoRefreshToken, true, false},
		{KindInvalidClient, false, true},
		{KindConsumeBusy, false, true},
		{KindStoreUnmirrored, false, true},
		{KindStashUnreadable, false, true},
		{KindTransient, false, false},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			out := RefreshOutcome{Error: tt.kind}
			if out.Permanent() != tt.permanent {
				t.Errorf("Permanent() = %v, want %v", out.Permanent(), tt.permanent)
			}
			if out.Deterministic() != tt.determ {
				t.Errorf("Deterministic() = %v, want %v", out.Deterministic(), tt.determ)
			}
			if out.OK() {
				t.Error("a failed outcome reported OK")
			}
		})
	}
}

// Keeping a deterministic kind distinct from the generic one is only worth
// doing because of the remedy it carries. A kind with no note renders as a bare
// identifier, which is strictly worse than the generic string it displaced.
func TestEveryDeterministicKindHasANote(t *testing.T) {
	for _, kind := range deterministicRefreshKinds {
		note := Note(kind)
		if note == string(kind) {
			t.Errorf("%s has no note, so it renders as a bare identifier", kind)
		}
	}
	// An unrecognized kind still renders something rather than nothing.
	if got := Note(HTTPKind(429)); got != "http-429" {
		t.Errorf("Note(http-429) = %q, want the kind itself", got)
	}
}

func TestPermanentAndDeterministicKindsAreDisjoint(t *testing.T) {
	for _, kind := range permanentRefreshKinds {
		if slices.Contains(deterministicRefreshKinds, kind) {
			t.Errorf("%s is both permanent and deterministic; the outcome verdicts overlap", kind)
		}
	}
}
