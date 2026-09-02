package provider

import (
	"encoding/base64"
	json "encoding/json/v2"
	"strings"
)

// Identity is who a credential belongs to, in the shape every layer above this
// package compares.
//
// The field names are Claude's because Claude came first, but the roles are
// general: OrganizationUUID is whatever a quota belongs to, and for Codex that
// is the ChatGPT account rather than an organization.
type Identity struct {
	Email            string
	OrganizationUUID string
	OrganizationName string
	AccountUUID      string

	// Fingerprint is a digest of the credential this identity was read from.
	//
	// It names the GENERATION, not the account: a refresh changes it while the
	// account stays the same. That makes it the answer to "has someone logged
	// in outside aaswap", and — for a provider nobody has written a parser for
	// — the account's name until a person picks a better one.
	Fingerprint string
}

// Handle is what a person calls this account.
//
// The address when there is one, because a person recognises it; the
// fingerprint otherwise. Empty only for an identity that is not one.
func (i Identity) Handle() string {
	if i.Email != "" {
		return i.Email
	}
	return i.Fingerprint
}

// CodexIdentity reads who a Codex credential belongs to.
//
// Codex carries its identity in the id_token rather than in a config field, so
// unlike Claude there is nothing to read beside the credential — the credential
// IS the identity document.
//
// # The signature is deliberately not verified
//
// This file is already on the machine, under the user's own account, and aaswap
// is not authenticating anyone with it: it is reading a label off a credential
// it already holds in order to say whose it is. Verifying would mean fetching
// and pinning OpenAI's signing keys to learn something the file's own presence
// already established. A tampered token can make aaswap mislabel an account in
// a listing; it cannot make aaswap hand anyone a credential they did not have.
//
// Reports false when there is no identity to read: an API-key install, a
// half-written file, a token whose payload is not JSON. An account with no
// address is not an identity, and treating one as such would let every
// comparison against it succeed vacuously.
func CodexIdentity(credentials string) (Identity, bool) {
	var auth struct {
		Tokens struct {
			IDToken string `json:"id_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(credentials), &auth); err != nil {
		return Identity{}, false
	}
	claims, ok := decodeJWTClaims(auth.Tokens.IDToken)
	if !ok {
		return Identity{}, false
	}

	var payload struct {
		Email string `json:"email"`
		Sub   string `json:"sub"`
		// The claim key is a URL, which is how OIDC namespaces private claims.
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
			PlanType  string `json:"chatgpt_plan_type"`
			UserID    string `json:"chatgpt_user_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(claims, &payload); err != nil {
		return Identity{}, false
	}
	email := strings.ToLower(strings.TrimSpace(payload.Email))
	if email == "" {
		return Identity{}, false
	}
	accountUUID := payload.Auth.UserID
	if accountUUID == "" {
		accountUUID = payload.Sub
	}
	return Identity{
		Email: email,
		// The ChatGPT account is what a plan and its quota belong to, which is
		// the role an organization plays for Claude.
		OrganizationUUID: payload.Auth.AccountID,
		OrganizationName: payload.Auth.PlanType,
		AccountUUID:      accountUUID,
	}, true
}

// decodeJWTClaims returns a token's payload segment, decoded.
//
// Base64url without padding, per RFC 7515. Nothing else about the token is
// inspected — see CodexIdentity for why the signature is not.
func decodeJWTClaims(token string) ([]byte, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	claims, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	return claims, true
}
