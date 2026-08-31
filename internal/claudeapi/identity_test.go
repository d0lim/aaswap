package claudeapi

import (
	"net/http"
	"testing"
	"time"
)

func TestProfileResolvesIdentity(t *testing.T) {
	c, rec := newTestClient(t, respondJSON(http.StatusOK, `{
		"account": {"uuid": "acct-1", "email": "user@example.com"},
		"organization": {"uuid": "org-1", "name": "Example"}
	}`))

	got := c.Profile(t.Context(), "sk-ant-oat01-abc")
	if got == nil {
		t.Fatal("Profile did not resolve a well-formed response")
	}
	want := Identity{UUID: "acct-1", Email: "user@example.com", OrganizationUUID: "org-1"}
	if *got != want {
		t.Errorf("Profile = %+v, want %+v", *got, want)
	}
	if auth := rec.header.Get("Authorization"); auth != "Bearer sk-ant-oat01-abc" {
		t.Errorf("Authorization = %q", auth)
	}
}

// The oracle is advisory: a switch proceeds without it. So every failure is
// "unresolvable" rather than an error, and the drift lands on the fail-open
// path instead of silently degrading a comparison.
func TestProfileIsUnresolvableRatherThanWrong(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{"a 401", 401, `{"error":"unauthorized"}`},
		{"a 500", 500, `{}`},
		{"malformed JSON", 200, `not json`},
		{"no account object", 200, `{"organization":{"uuid":"org-1"}}`},
		{"a null account", 200, `{"account":null}`},
		{"an account that is not an object", 200, `{"account":"acct-1"}`},
		// A schema change that renames uuid must fail open, not resolve to
		// something wrong: consumers backfill and compare by uuid.
		{"no uuid", 200, `{"account":{"email":"user@example.com"}}`},
		{"a non-string uuid", 200, `{"account":{"uuid":123}}`},
		{"a blank uuid", 200, `{"account":{"uuid":"   "}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, respondJSON(tt.status, tt.body))
			if got := c.Profile(t.Context(), "token"); got != nil {
				t.Errorf("Profile = %+v, want nil", *got)
			}
		})
	}

	t.Run("a network failure", func(t *testing.T) {
		c, _ := newTestClient(t, respondJSON(200, `{}`))
		c.ProfileURL = "http://127.0.0.1:1/profile"
		if got := c.Profile(t.Context(), "token"); got != nil {
			t.Errorf("Profile = %+v, want nil", *got)
		}
	})
}

// Strict on uuid, lenient on everything else: a malformed optional field drops
// that field rather than failing the lookup that carried it.
func TestProfileToleratesMalformedOptionalFields(t *testing.T) {
	tests := []struct {
		name string
		body string
		want Identity
	}{
		{
			"a uuid alone still resolves",
			`{"account":{"uuid":"acct-1"}}`,
			Identity{UUID: "acct-1"},
		},
		{
			"a non-string email is dropped, not fatal",
			`{"account":{"uuid":"acct-1","email":42}}`,
			Identity{UUID: "acct-1"},
		},
		{
			"a non-object organization is dropped",
			`{"account":{"uuid":"acct-1"},"organization":"org-1"}`,
			Identity{UUID: "acct-1"},
		},
		{
			"a non-string organization uuid is dropped",
			`{"account":{"uuid":"acct-1"},"organization":{"uuid":[1]}}`,
			Identity{UUID: "acct-1"},
		},
		{
			"surrounding whitespace is normalized away",
			`{"account":{"uuid":"  acct-1  ","email":"user@example.com"}}`,
			Identity{UUID: "acct-1", Email: "user@example.com"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, respondJSON(http.StatusOK, tt.body))
			got := c.Profile(t.Context(), "token")
			if got == nil {
				t.Fatal("Profile did not resolve")
			}
			if *got != tt.want {
				t.Errorf("Profile = %+v, want %+v", *got, tt.want)
			}
		})
	}
}

func TestProfileIsBounded(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	c.ProfileTimeout = 50 * time.Millisecond

	start := time.Now()
	if got := c.Profile(t.Context(), "token"); got != nil {
		t.Error("a timed-out lookup resolved an identity")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("the lookup took %v; it is meant to be bounded", elapsed)
	}
}

// The token grant carries an opportunistic identity for the credential it just
// rotated — the same answer as a profile lookup, at zero extra requests.
func TestRefreshSurfacesTheTokenAccount(t *testing.T) {
	tests := []struct {
		name string
		body string
		want *Identity
	}{
		{
			// The grant spells the email "email_address"; the profile spells
			// the same field "email".
			name: "present",
			body: `{"access_token":"a","expires_in":1,
			        "account":{"uuid":"acct-1","email_address":"user@example.com"},
			        "organization":{"uuid":"org-1"}}`,
			want: &Identity{UUID: "acct-1", Email: "user@example.com", OrganizationUUID: "org-1"},
		},
		{
			name: "absent",
			body: `{"access_token":"a","expires_in":1}`,
			want: nil,
		},
		{
			name: "no uuid",
			body: `{"access_token":"a","expires_in":1,"account":{"email_address":"user@example.com"}}`,
			want: nil,
		},
		{
			name: "a non-string uuid",
			body: `{"access_token":"a","expires_in":1,"account":{"uuid":123}}`,
			want: nil,
		},
		{
			name: "whitespace normalized",
			body: `{"access_token":"a","expires_in":1,"account":{"uuid":" acct-1 "}}`,
			want: &Identity{UUID: "acct-1"},
		},
		{
			// The identity is opportunistic and must never break the refresh
			// that carried it.
			name: "malformed optionals do not fail the grant",
			body: `{"access_token":"a","expires_in":1,"account":{"uuid":"acct-1","email_address":99}}`,
			want: &Identity{UUID: "acct-1"},
		},
		{
			// The profile endpoint's spelling on a grant response resolves the
			// uuid but not the email, which is the honest answer: this endpoint
			// did not send one.
			name: "the profile spelling is not read here",
			body: `{"access_token":"a","expires_in":1,"account":{"uuid":"acct-1","email":"user@example.com"}}`,
			want: &Identity{UUID: "acct-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestClient(t, respondJSON(http.StatusOK, tt.body))
			out := c.Refresh(t.Context(), oauthCreds(t, map[string]any{"refreshToken": "r"}), testNow)
			if !out.OK() {
				t.Fatalf("Refresh failed: %q", out.Error)
			}
			switch {
			case tt.want == nil && out.TokenAccount != nil:
				t.Errorf("TokenAccount = %+v, want nil", *out.TokenAccount)
			case tt.want != nil && out.TokenAccount == nil:
				t.Errorf("TokenAccount = nil, want %+v", *tt.want)
			case tt.want != nil && *out.TokenAccount != *tt.want:
				t.Errorf("TokenAccount = %+v, want %+v", *out.TokenAccount, *tt.want)
			}
		})
	}
}
