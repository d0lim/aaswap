package provider

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"strings"
)

// ClaudeIdentity reads who a Claude Code login belongs to.
//
// The address lives in the CONFIG rather than in the credential — the opposite
// of Codex, where the credential is the identity document. That asymmetry is
// the reason identity extraction is a declared capability instead of a shared
// helper with a path argument.
//
// Parsed leniently into a map first: this is the user's own file, it accretes
// keys aaswap has never heard of, and a strict decode that failed on one of
// them would report "nobody is logged in" for a perfectly good login.
//
// Reports false when there is no identity to read. An account with no address
// is not an identity, and treating one as such would let every comparison
// against it succeed vacuously.
func ClaudeIdentity(config string) (Identity, bool) {
	if strings.TrimSpace(config) == "" {
		return Identity{}, false
	}
	var object map[string]jsontext.Value
	if err := json.Unmarshal([]byte(config), &object); err != nil || object == nil {
		return Identity{}, false
	}
	raw, ok := object["oauthAccount"]
	if !ok {
		return Identity{}, false
	}

	var account struct {
		EmailAddress     string `json:"emailAddress"`
		OrganizationUUID string `json:"organizationUuid"`
		OrganizationName string `json:"organizationName"`
		AccountUUID      string `json:"accountUuid"`
	}
	if err := json.Unmarshal(raw, &account); err != nil {
		return Identity{}, false
	}
	email := strings.ToLower(strings.TrimSpace(account.EmailAddress))
	if email == "" {
		return Identity{}, false
	}
	return Identity{
		Email:            email,
		OrganizationUUID: account.OrganizationUUID,
		OrganizationName: account.OrganizationName,
		AccountUUID:      account.AccountUUID,
	}, true
}
