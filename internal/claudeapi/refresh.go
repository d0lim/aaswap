package claudeapi

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
)

// RefreshOutcome is the result of a refresh-token grant attempt.
type RefreshOutcome struct {
	// Credentials is the full rotated credentials JSON on success, empty
	// otherwise.
	Credentials string

	// Error classifies a failure so callers can tell a dead refresh lineage
	// (permanent: quarantine, stop retrying) from a network blip (transient:
	// retry later). Empty on success.
	Error ErrorKind

	// TokenAccount is the account identity the token endpoint optionally
	// includes alongside a successful grant — a zero-request identity source
	// for the credential just refreshed. Nil when the server omitted it or on
	// failure; strictly opportunistic, never required.
	TokenAccount *Identity

	// ConsumedFP fingerprints the generation actually POSTed. A caller-supplied
	// gate may substitute a fresher re-read or a session profile for the
	// snapshot it was handed, and a strike must bind to the bytes that were
	// spent, not to the snapshot.
	ConsumedFP string

	// Stashed reports whether a consumed successor actually reached the stash.
	// Only meaningful on a transient outcome that nonetheless carries
	// credentials: false there means both the persist and the stash write
	// failed, so the successor survives only in Credentials and a retry would
	// POST the spent predecessor. A caller telling the user what to do next
	// must not promise a stash that never happened.
	Stashed bool
}

// OK reports whether the grant produced usable credentials.
func (o RefreshOutcome) OK() bool { return o.Credentials != "" && o.Error == "" }

// Permanent reports whether the failure is a verdict about the refresh lineage
// itself, rather than something a retry could clear.
func (o RefreshOutcome) Permanent() bool { return slices.Contains(permanentRefreshKinds, o.Error) }

// Deterministic reports whether the failure will not resolve within this pass,
// so the caller must not fall through to another request carrying the same
// known-expired token.
func (o RefreshOutcome) Deterministic() bool {
	return slices.Contains(deterministicRefreshKinds, o.Error)
}

// Refresher exchanges a credential snapshot for a rotated one.
//
// The interface exists so the switcher can substitute its consume gate, which
// re-reads the freshest copy under the slot lock, persists via a fingerprint
// compare-and-swap, and never spends a superseded snapshot. A refresh token is
// single-use: POSTing a stale snapshot invalidates the live one.
type Refresher interface {
	Refresh(ctx context.Context, accountNum, email, credentials string) RefreshOutcome
}

// tokenResponse is the token endpoint's success body.
type tokenResponse struct {
	AccessToken  string  `json:"access_token"`
	ExpiresIn    float64 `json:"expires_in"`
	RefreshToken string  `json:"refresh_token"`
	Scope        string  `json:"scope"`

	// The identity objects stay raw so a malformed one can be dropped without
	// failing the grant that carried it — see [parseIdentity].
	Account      jsontext.Value `json:"account"`
	Organization jsontext.Value `json:"organization"`
}

// Refresh exchanges a credential's refresh token for a rotated credential.
//
// The rotated document is the caller's own, with only the four OAuth members
// the grant updates rewritten — every sibling Claude Code owns passes through
// untouched.
func (c *Client) Refresh(ctx context.Context, credentials string, now time.Time) RefreshOutcome {
	// KindNoRefreshToken is a PERMANENT verdict, so it demands a structurally
	// complete OAuth object that genuinely lacks the field. An unparseable or
	// non-object blob is far more likely a torn read than a real credential
	// shape, and is transient: the next pass re-reads and either succeeds or
	// sees the true shape.
	doc, ok := ParseDocument(credentials)
	if !ok {
		return RefreshOutcome{Error: KindTransient}
	}
	payload, ok := doc.OAuth()
	if !ok {
		return RefreshOutcome{Error: KindNoRefreshToken}
	}
	token := RefreshToken(payload)
	if token == "" {
		return RefreshOutcome{Error: KindNoRefreshToken}
	}

	body, err := json.Marshal(map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": token,
		"client_id":     ClientID,
	}, json.Deterministic(true))
	if err != nil {
		return RefreshOutcome{Error: KindTransient}
	}

	req, err := c.newRequest(http.MethodPost, c.TokenURL, body,
		map[string]string{"Content-Type": "application/json"})
	if err != nil {
		return RefreshOutcome{Error: KindTransient}
	}

	raw, err := c.do(ctx, req, c.RefreshTimeout)
	if err != nil {
		return RefreshOutcome{Error: classifyRefresh(err)}
	}

	var resp tokenResponse
	if err := decodeJSON(raw, &resp); err != nil {
		slog.Debug("OAuth refresh returned an undecodable body", "error", err)
		return RefreshOutcome{Error: KindTransient}
	}
	if resp.AccessToken == "" {
		// A 200 with no token is not a grant. Transient rather than permanent:
		// it is a server-side anomaly, not a verdict about this lineage.
		slog.Debug("OAuth refresh returned no access token")
		return RefreshOutcome{Error: KindTransient}
	}

	payload["accessToken"] = resp.AccessToken
	payload["expiresAt"] = float64(now.UnixMilli() + int64(resp.ExpiresIn*1000))
	// The endpoint rotates the refresh token on some grants and not others;
	// keeping the old one when none comes back is what makes the next grant
	// work.
	if resp.RefreshToken != "" {
		payload["refreshToken"] = resp.RefreshToken
	}
	if resp.Scope != "" {
		payload["scopes"] = strings.Fields(resp.Scope)
	}
	doc[oauthKey] = payload

	rotated, err := doc.Encode()
	if err != nil {
		return RefreshOutcome{Error: KindTransient}
	}
	// The grant's own account object spells the email "email_address"; the
	// profile endpoint spells the same field "email".
	return RefreshOutcome{
		Credentials:  rotated,
		TokenAccount: parseIdentity(resp.Account, resp.Organization, "email_address"),
	}
}

// classifyRefresh maps a failed grant to a verdict.
//
// Permanent only when the server itself rejected the grant: a 4xx AND an
// explicit RFC 6749 §5.2 marker as the body's top-level "error" member.
// Anything ambiguous stays transient.
func classifyRefresh(err error) ErrorKind {
	var httpErr *httpError
	if !errors.As(err, &httpErr) {
		slog.Debug("OAuth refresh failed", "error", err)
		return KindTransient
	}
	slog.Debug("OAuth refresh failed", "status", httpErr.status, "body", truncate(string(httpErr.body), 500))

	if httpErr.status != http.StatusBadRequest &&
		httpErr.status != http.StatusUnauthorized &&
		httpErr.status != http.StatusForbidden {
		return KindTransient
	}
	// The verdict is the top-level "error" member. A substring scan over the
	// body misclassifies: the marker can appear inside another envelope's
	// detail text, and a dead-token verdict quarantines the slot on the spot.
	// An unparseable body stays transient.
	var envelope struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(httpErr.body, &envelope); err != nil {
		return KindTransient
	}
	switch ErrorKind(envelope.Error) {
	case KindInvalidGrant:
		// This slot's refresh lineage is dead.
		return KindInvalidGrant
	case KindInvalidClient:
		// OUR client credential was rejected — systemic, and evidence about no
		// particular slot, so it keeps its own kind and lands no strike.
		return KindInvalidClient
	}
	return KindTransient
}
