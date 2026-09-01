package provider

import (
	"encoding/base64"
	json "encoding/json/v2"
	"strings"
	"testing"
)

// jwt builds an unsigned token carrying the given claims. Unsigned because
// nothing here verifies one — see CodexIdentity.
func jwt(t *testing.T, claims map[string]any) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	enc := base64.RawURLEncoding.EncodeToString
	return strings.Join([]string{
		enc([]byte(`{"alg":"none"}`)), enc(payload), "",
	}, ".")
}

func codexAuth(t *testing.T, claims map[string]any) string {
	t.Helper()
	auth := map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]any{
			"id_token":      jwt(t, claims),
			"access_token":  "at",
			"refresh_token": "rt",
			"account_id":    "acct",
		},
	}
	data, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Codex carries its identity in a signed token rather than a config field, so
// the address has to be read out of the claims.
func TestCodexIdentityFromTheIDToken(t *testing.T) {
	credentials := codexAuth(t, map[string]any{
		"email": "Work@Example.com",
		"https://api.openai.com/auth": map[string]any{
			"chatgpt_account_id": "acct-123",
			"chatgpt_plan_type":  "plus",
			"chatgpt_user_id":    "user-9",
		},
		"sub": "sub-1",
	})

	got, ok := CodexIdentity(credentials)
	if !ok {
		t.Fatal("the identity did not resolve")
	}
	// Lowercased, because an address is not case-sensitive in the half that
	// matters and every comparison above this treats it as a key.
	if got.Email != "work@example.com" {
		t.Errorf("email = %q", got.Email)
	}
	// The ChatGPT account is what a plan and its quota belong to, which is the
	// role the organization plays for Claude.
	if got.OrganizationUUID != "acct-123" {
		t.Errorf("organizationUuid = %q, want the chatgpt account", got.OrganizationUUID)
	}
	if got.OrganizationName != "plus" {
		t.Errorf("organizationName = %q, want the plan", got.OrganizationName)
	}
	if got.AccountUUID != "user-9" {
		t.Errorf("accountUuid = %q", got.AccountUUID)
	}
}

// An API-key install has no token to read, and neither does a half-written
// file. Neither is an identity, and treating one as such would let every
// comparison against it succeed vacuously.
func TestCodexIdentityRefusesWhatItCannotRead(t *testing.T) {
	tests := []struct {
		name        string
		credentials string
	}{
		{"empty", ""},
		{"not JSON", "{"},
		{"no tokens at all", `{"auth_mode":"apikey","OPENAI_API_KEY":"sk-x"}`},
		{"a token that is not a JWT", `{"tokens":{"id_token":"not.a.jwt.at.all"}}`},
		{"a JWT with no address", codexAuthNoEmail(t)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, ok := CodexIdentity(tt.credentials); ok {
				t.Errorf("CodexIdentity = (%+v, true), want no identity", got)
			}
		})
	}
}

func codexAuthNoEmail(t *testing.T) string {
	t.Helper()
	return codexAuth(t, map[string]any{"sub": "sub-1"})
}

// The signature is never checked, and that has to be a deliberate reading
// rather than an oversight: this file is already on the machine, under the
// user's own account, and aaswap is not authenticating anyone with it.
func TestCodexIdentityDoesNotVerifyTheSignature(t *testing.T) {
	credentials := codexAuth(t, map[string]any{"email": "work@example.com"})
	// A header claiming a real algorithm over a token that carries no
	// signature at all. Anything verifying would refuse this outright.
	forged := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","kid":"nope"}`))
	original := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	tampered := strings.Replace(credentials, original, forged, 1)
	if tampered == credentials {
		t.Fatal("the fixture did not change, so this proves nothing")
	}

	got, ok := CodexIdentity(tampered)
	if !ok {
		t.Fatal("a token with no valid signature was refused; nothing here " +
			"authenticates anyone, and refusing would break every real read")
	}
	if got.Email != "work@example.com" {
		t.Errorf("email = %q, want the claim read regardless of the header", got.Email)
	}
}
