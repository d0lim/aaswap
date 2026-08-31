package claudeapi

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"strings"
)

// Identity is who a credential belongs to, as the server reports it.
type Identity struct {
	// UUID is the account's stable identifier. Always non-empty in a returned
	// Identity — see [parseIdentity] for why that is the whole boundary.
	UUID string
	// Email and OrganizationUUID are optional; the server omits them on some
	// responses, and a uuid-only identity still resolves.
	Email            string
	OrganizationUUID string
}

// parseIdentity extracts an account identity from an endpoint response,
// returning nil when the response carries none usable.
//
// The boundary is strict in one direction and lenient in the other, and the
// asymmetry is the design. Strict on uuid: an identity counts as resolved only
// when it carries a non-empty string uuid, because consumers backfill and
// compare by uuid, so a schema change that renamed it must land on the
// fail-open path rather than silently degrading every comparison. Lenient on
// everything else: the objects are decoded as raw JSON and inspected member by
// member, so a malformed optional field drops that field instead of failing the
// grant or the profile lookup that carried it.
//
// emailKey names the member holding the email, because the two endpoints spell
// it differently: "email_address" on a token grant, "email" on the profile.
func parseIdentity(account, organization jsontext.Value, emailKey string) *Identity {
	fields, ok := decodeObject(account)
	if !ok {
		return nil
	}
	uuid, _ := fields["uuid"].(string)
	uuid = strings.TrimSpace(uuid)
	if uuid == "" {
		return nil
	}

	identity := &Identity{UUID: uuid}
	identity.Email, _ = fields[emailKey].(string)
	if org, ok := decodeObject(organization); ok {
		identity.OrganizationUUID, _ = org["uuid"].(string)
	}
	return identity
}

// decodeObject leniently decodes a raw JSON value as an object.
func decodeObject(raw jsontext.Value) (map[string]any, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return nil, false
	}
	return out, true
}

// profileResponse is the profile endpoint's body.
type profileResponse struct {
	Account      jsontext.Value `json:"account"`
	Organization jsontext.Value `json:"organization"`
}

// Profile resolves an OAuth access token to its account identity, reporting nil
// when it cannot be resolved.
//
// This answers the one question the credential bytes cannot: *whose* token is
// this. The oracle is strictly advisory — a switch proceeds without it — so
// every failure, transport or schema, is "unresolvable" rather than an error,
// and no kind is returned for a caller to act on.
//
// Must not be called while any credential or config lock is held.
func (c *Client) Profile(ctx context.Context, accessToken string) *Identity {
	req, err := c.newRequest(http.MethodGet, c.ProfileURL, nil, map[string]string{
		"Authorization": "Bearer " + accessToken,
		"Content-Type":  "application/json",
	})
	if err != nil {
		return nil
	}

	raw, err := c.do(ctx, req, c.ProfileTimeout)
	if err != nil {
		if httpErr, ok := errors.AsType[*httpError](err); ok && httpErr.status == http.StatusUnauthorized {
			// Evidence, not proof: the live access token cannot authenticate.
			// A freshly rotated own-credential would carry a fresh token, but
			// this also fires benignly — the family rotated, then the access
			// token expired on an idle machine. Log-file only: the caller
			// falls back to proceeding without identity and the user sees
			// nothing.
			slog.Warn("OAuth profile returned 401 while resolving credential ownership; " +
				"proceeding without identity")
		} else {
			slog.Debug("OAuth profile fetch failed", "error", err)
		}
		return nil
	}

	var resp profileResponse
	if err := decodeJSON(raw, &resp); err != nil {
		slog.Debug("OAuth profile returned an undecodable body", "error", err)
		return nil
	}
	identity := parseIdentity(resp.Account, resp.Organization, "email")
	if identity == nil {
		slog.Debug("OAuth profile response carried no usable account.uuid")
	}
	return identity
}
