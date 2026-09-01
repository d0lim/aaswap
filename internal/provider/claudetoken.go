package provider

import (
	json "encoding/json/v2"
	"fmt"

	"github.com/d0lim/aaswap/internal/credstore"
)

// claudeToken stores an account from a token pasted into `aaswap login --token`.
//
// Two formats, told apart by the value itself rather than by a flag: a managed
// API key is stored RAW, because that is what Claude Code's API-key axis reads,
// and an OAuth setup token is wrapped in the object its OAuth axis reads. Asking
// the user which one they have would be asking them to restate what the prefix
// already says, and getting the answer wrong stores a working token in the axis
// that will never look at it.
type claudeToken struct{}

// setupTokenScopes are what a setup token carries. Recorded so the stored
// credential has the shape Claude Code expects, even though nothing here
// verifies it.
var setupTokenScopes = []string{"user:inference"}

func (claudeToken) Hint() string { return "sk-ant-oat01-… or sk-ant-api03-…" }

func (claudeToken) APIKey(token string) bool { return credstore.LooksLikeAPIKey(token) }

// Material builds the stored credential and config for a raw token.
//
// The synthesized config is the same for either format: neither token carries
// real organization metadata, and the config exists so the account has an
// identity block to switch on at all.
func (t claudeToken) Material(token, email string) (credentials, config string, err error) {
	if t.APIKey(token) {
		credentials = token
	} else {
		encoded, marshalErr := json.Marshal(map[string]any{
			"claudeAiOauth": map[string]any{
				"accessToken": token,
				"scopes":      setupTokenScopes,
			},
		}, json.Deterministic(true))
		if marshalErr != nil {
			return "", "", fmt.Errorf("encoding the token credential: %w", marshalErr)
		}
		credentials = string(encoded)
	}

	encodedConfig, err := json.Marshal(map[string]any{
		"oauthAccount": map[string]any{
			"emailAddress":     email,
			"accountUuid":      "",
			"organizationUuid": nil,
			"organizationName": nil,
		},
	}, json.Deterministic(true))
	if err != nil {
		return "", "", fmt.Errorf("encoding the token config: %w", err)
	}
	return credentials, string(encodedConfig), nil
}
