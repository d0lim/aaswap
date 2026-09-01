package claudeapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

// FetchRequest is one account's usage fetch, with the collaborators that decide
// how a refresh is performed and where a rotated credential goes.
type FetchRequest struct {
	// AccountNum and Email name the slot. AccountNum appears in log lines;
	// Email never does — those lines are what users paste into public issues.
	AccountNum string
	Email      string

	// Credentials is the slot's credential snapshot.
	Credentials string

	// IsActive marks the credential Claude Code is currently using. Active
	// credentials are never refreshed: Claude Code owns them, and rotating one
	// underneath a running editor invalidates the token it holds.
	IsActive bool

	// Now is the reference time for expiry checks.
	Now time.Time

	// Refresher supersedes the direct token-endpoint POST when set. The
	// switcher passes its consume gate, which re-reads the freshest copy under
	// the slot lock, persists via a fingerprint compare-and-swap, and never
	// spends a superseded snapshot. Persist is then unused for the refresh:
	// the gate persists internally.
	Refresher Refresher

	// Persist stores a rotated credential. Only consulted on the direct-POST
	// path.
	Persist func(accountNum, email, credentials string) error

	// Warn is the user-facing channel for the one failure a user must act on
	// themselves. Optional; failures are always logged regardless.
	Warn func(string)
}

// FetchUsageForAccount fetches one account's usage, refreshing an expired token
// first when the account is not the active one.
//
// The refresh is not an optimization. A slot whose access token expired while
// idle would 401 on every pass forever, so the token is rotated first when the
// expiry says it must be — but only for inactive slots, and only when there is
// a refresh token to spend.
//
// Refresh failures split three ways, and the split is what keeps a live account
// from being locked out. A permanent verdict short-circuits: the usage endpoint
// is not called with a token already known to be dead, because that only adds a
// 401 to a lost cause. A deterministic refusal short-circuits for the same
// reason, keeping its own kind so the remedy can be shown. Everything else
// falls through to try the expired token anyway — the server may disagree with
// the local clock, and the 401 path below retries the refresh once.
func (c *Client) FetchUsageForAccount(ctx context.Context, req FetchRequest) UsageOutcome {
	// No email: these lines are paste-safe for public issues.
	logContext := fmt.Sprintf("account %s", req.AccountNum)

	payload, ok := OAuthPayload(req.Credentials)
	if !ok {
		return UsageOutcome{Error: KindNoAccessToken}
	}
	accessToken, _ := payload["accessToken"].(string)
	if accessToken == "" {
		return UsageOutcome{Error: KindNoAccessToken}
	}

	working := req.Credentials

	if !req.IsActive && RefreshToken(payload) != "" && Expired(payload, req.Now) {
		outcome := c.refresh(ctx, req, working)
		switch {
		case outcome.OK():
			working = outcome.Credentials
			c.persist(req, working)
			if refreshed, ok := OAuthPayload(working); ok {
				payload = refreshed
				if token, _ := payload["accessToken"].(string); token != "" {
					accessToken = token
				}
			}
		case outcome.Permanent():
			// The refresh lineage is server-rejected or structurally absent.
			// The strike binds to the bytes actually POSTed — a gate may have
			// substituted a fresher re-read for this snapshot.
			return UsageOutcome{
				Error:    outcome.Error,
				StruckFP: cmp.Or(outcome.ConsumedFP, Fingerprint(working)),
			}
		case outcome.Deterministic():
			return UsageOutcome{Error: outcome.Error}
		}
		// A transient refresh failure falls through with the expired token.
	}

	result, err := c.FetchUsage(ctx, accessToken)
	if err == nil {
		return UsageOutcome{Usage: result}
	}

	kind, retry := classify(err)
	httpErr, isHTTP := errors.AsType[*httpError](err)
	unauthorized := isHTTP && httpErr.status == http.StatusUnauthorized
	if !unauthorized || req.IsActive || RefreshToken(payload) == "" {
		logUsageFailure(logContext, kind, retry, err)
		return UsageOutcome{Error: kind, RetryAfter: retry}
	}

	// A 401 on an inactive account with a refresh token in hand: the server
	// rotated past this token even though the local expiry said it was fresh.
	// Refresh once and retry.
	outcome := c.refresh(ctx, req, working)
	if !outcome.OK() {
		logUsageFailure(logContext, kind, nil, err)
		// A permanent or deterministic verdict keeps its own kind here too.
		// Collapsing it to KindRefreshFailed would hide the remedy exactly on
		// the path that most needs it.
		reported := KindRefreshFailed
		if outcome.Permanent() || outcome.Deterministic() {
			reported = outcome.Error
		}
		result := UsageOutcome{Error: reported}
		if outcome.Permanent() {
			result.StruckFP = cmp.Or(outcome.ConsumedFP, Fingerprint(working))
		}
		return result
	}

	working = outcome.Credentials
	c.persist(req, working)
	newToken := AccessToken(working)
	if newToken == "" {
		return UsageOutcome{Error: KindRefreshFailed}
	}

	result, err = c.FetchUsage(ctx, newToken)
	if err == nil {
		return UsageOutcome{Usage: result}
	}
	kind, retry = classify(err)
	logUsageFailure(logContext+" after refresh", kind, retry, err)
	return UsageOutcome{Error: kind, RetryAfter: retry}
}

// refresh runs the request's gate when it has one, else POSTs directly.
func (c *Client) refresh(ctx context.Context, req FetchRequest, credentials string) RefreshOutcome {
	if req.Refresher != nil {
		return req.Refresher.Refresh(ctx, req.AccountNum, req.Email, credentials)
	}
	return c.Refresh(ctx, credentials, req.Now)
}

// persist stores a rotated credential on the direct-POST path.
//
// A gate persists internally, so this is a no-op when one ran — persisting
// again would write behind its compare-and-swap.
//
// A failure here is loud on purpose. The refresh token on disk is now the spent
// predecessor, so the next pass POSTs a dead grant and the account looks
// permanently broken for a reason that has nothing to do with the account.
func (c *Client) persist(req FetchRequest, credentials string) {
	if req.Refresher != nil || req.Persist == nil {
		return
	}
	if err := req.Persist(req.AccountNum, req.Email, credentials); err != nil {
		slog.Warn("refreshed the OAuth token but failed to persist it; "+
			"the refresh token on disk may now be stale",
			"account", req.AccountNum, "error", err)
		if req.Warn != nil {
			req.Warn(fmt.Sprintf(
				"Warning: failed to save refreshed token for account %s (%s). "+
					"If the next refresh fails, re-run `ccswap add` after logging in.",
				req.AccountNum, req.Email))
		}
	}
}
