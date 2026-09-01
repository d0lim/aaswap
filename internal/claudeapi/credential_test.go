package claudeapi

import (
	json "encoding/json/v2"
	"maps"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 3, 23, 12, 0, 0, 0, time.UTC)

// oauthCreds builds a credential document whose OAuth payload carries the given
// members.
func oauthCreds(t *testing.T, payload map[string]any, siblings ...map[string]any) string {
	t.Helper()
	doc := map[string]any{oauthKey: payload}
	for _, s := range siblings {
		maps.Copy(doc, s)
	}
	encoded, err := json.Marshal(doc, json.Deterministic(true))
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestAccessToken(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"a valid credential", `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-abc"}}`, "sk-ant-oat01-abc"},
		{"no OAuth payload", `{"other":{"accessToken":"x"}}`, ""},
		{"no access token", `{"claudeAiOauth":{"refreshToken":"r"}}`, ""},
		{"a non-string token", `{"claudeAiOauth":{"accessToken":42}}`, ""},
		{"invalid JSON", `not json`, ""},
		{"empty", "", ""},
		// A managed API key is a bare string, not a document.
		{"a raw API key", `sk-ant-api03-abc`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AccessToken(tt.in); got != tt.want {
				t.Errorf("AccessToken(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestExpired(t *testing.T) {
	tests := []struct {
		name      string
		expiresAt any
		want      bool
	}{
		{"an hour out", testNow.Add(time.Hour).UnixMilli(), false},
		{"long past", testNow.Add(-24 * time.Hour).UnixMilli(), true},
		{"exactly now", testNow.UnixMilli(), true},
		// The buffer is what makes the refresh happen before a request goes
		// out rather than after it comes back 401.
		{"inside the expiry buffer", testNow.Add(ExpiryBuffer - time.Minute).UnixMilli(), true},
		{"exactly at the buffer edge", testNow.Add(ExpiryBuffer).UnixMilli(), true},
		{"just outside the buffer", testNow.Add(ExpiryBuffer + time.Minute).UnixMilli(), false},
		// An absent or unusable expiry is NOT expired: refreshing on every pass
		// for a shape that simply states no expiry spends a grant to learn
		// nothing.
		{"absent", nil, false},
		{"a non-numeric expiry", "soon", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			payload := map[string]any{"accessToken": "a"}
			if tt.expiresAt != nil {
				payload["expiresAt"] = tt.expiresAt
			}
			decoded, ok := OAuthPayload(oauthCreds(t, payload))
			if !ok {
				t.Fatal("the credential did not round-trip")
			}
			if got := Expired(decoded, testNow); got != tt.want {
				t.Errorf("Expired = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFingerprint(t *testing.T) {
	// The fingerprint tracks the refresh lineage, so rotating the access token
	// must leave it alone: the two generations are the same credential.
	t.Run("stable across access-token rotation", func(t *testing.T) {
		first := oauthCreds(t, map[string]any{"accessToken": "a1", "refreshToken": "r"})
		second := oauthCreds(t, map[string]any{"accessToken": "a2", "refreshToken": "r"})
		if Fingerprint(first) != Fingerprint(second) {
			t.Error("the fingerprint changed when only the access token rotated")
		}
	})

	t.Run("differs across refresh-token rotation", func(t *testing.T) {
		first := oauthCreds(t, map[string]any{"refreshToken": "r1"})
		second := oauthCreds(t, map[string]any{"refreshToken": "r2"})
		if Fingerprint(first) == Fingerprint(second) {
			t.Error("the fingerprint survived a refresh-token rotation")
		}
	})

	// API keys and setup tokens never rotate, so content identity is lineage
	// identity for them.
	t.Run("full-content fallback without a refresh token", func(t *testing.T) {
		key := "sk-ant-api03-abc"
		if got := Fingerprint(key); !strings.HasPrefix(got, fingerprintFull) {
			t.Errorf("Fingerprint = %q, want the full-content scheme", got)
		}
		if Fingerprint(key) == Fingerprint(key+"x") {
			t.Error("different bytes produced the same full-content fingerprint")
		}
	})

	// Distinct prefixes are what stop a full-content hash of one credential
	// from ever equalling the refresh hash of another.
	t.Run("the two schemes cannot collide", func(t *testing.T) {
		token := "r"
		withRefresh := Fingerprint(oauthCreds(t, map[string]any{"refreshToken": token}))
		bare := Fingerprint(token)
		if withRefresh == bare {
			t.Error("a refresh-token fingerprint collided with a full-content one")
		}
		if !strings.HasPrefix(withRefresh, fingerprintRefresh) {
			t.Errorf("Fingerprint = %q, want the refresh scheme", withRefresh)
		}
	})

	// Empty is reserved for empty input. A caller asking "did this change?"
	// must never get an empty answer for real bytes.
	t.Run("only empty input is empty", func(t *testing.T) {
		if Fingerprint("") != "" {
			t.Error("empty input produced a fingerprint")
		}
		for _, in := range []string{"{}", "garbage", `{"claudeAiOauth":{}}`} {
			if Fingerprint(in) == "" {
				t.Errorf("Fingerprint(%q) was empty for real bytes", in)
			}
		}
	})
}

func TestTokenStatus(t *testing.T) {
	tests := []struct {
		name    string
		payload map[string]any
		want    string
		wantOK  bool
	}{
		{
			name:    "fresh with a refresh token",
			payload: map[string]any{"refreshToken": "r", "expiresAt": testNow.Add(2 * time.Hour).UnixMilli()},
			want:    "oauth: fresh, refresh token yes, expires 14:00 in 2h 0m",
			wantOK:  true,
		},
		{
			name:    "expired without one",
			payload: map[string]any{"expiresAt": testNow.Add(-time.Hour).UnixMilli()},
			want:    "oauth: expired, refresh token no, expires 11:00 in 0m",
			wantOK:  true,
		},
		{
			name:    "no stated expiry",
			payload: map[string]any{"refreshToken": "r"},
			want:    "oauth: unknown expiry, refresh token yes",
			wantOK:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := TokenStatus(oauthCreds(t, tt.payload), testNow)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if got != tt.want {
				t.Errorf("TokenStatus =\n  %q\nwant\n  %q", got, tt.want)
			}
		})
	}

	t.Run("a credential with no OAuth payload has no status", func(t *testing.T) {
		for _, in := range []string{"", "sk-ant-api03-abc", `{"other":1}`} {
			if _, ok := TokenStatus(in, testNow); ok {
				t.Errorf("TokenStatus(%q) reported a status", in)
			}
		}
	})
}

// A millisecond epoch has to survive the decode/encode round trip exactly: it
// is compared against the clock, and an exponent-formatted or rounded value
// would make a live token read as expired.
func TestExpiryRoundTripsExactly(t *testing.T) {
	ms := int64(1774267200123)
	creds := oauthCreds(t, map[string]any{"accessToken": "a", "expiresAt": ms})

	doc, ok := ParseDocument(creds)
	if !ok {
		t.Fatal("the credential did not parse")
	}
	encoded, err := doc.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(encoded, "1774267200123") {
		t.Errorf("the expiry did not survive the round trip verbatim: %s", encoded)
	}

	payload, _ := OAuthPayload(encoded)
	got, ok := ExpiresAt(payload)
	if !ok || got.UnixMilli() != ms {
		t.Errorf("ExpiresAt = (%v, %v), want %d ms", got, ok, ms)
	}
}
