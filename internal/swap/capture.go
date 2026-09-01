package swap

import (
	"context"
	"fmt"
	"strings"

	"github.com/d0lim/ccswap/internal/apperr"
	"github.com/d0lim/ccswap/internal/claudeapi"
)

// CaptureResult is the credential a capture read, plus what could not be
// verified about it.
type CaptureResult struct {
	Credentials string
	// Unverified explains why the ownership check could not complete, and is
	// empty when it did. Registering with the question unanswered is allowed —
	// that is the behavior every version before the check had — but never
	// silently: an unverified capture is exactly the state the field incident
	// that motivated the check was in.
	Unverified string
}

// ReadCaptureCredentials reads the credential a capture would store.
//
// It REFUSES a degraded read rather than capturing what one produced. On macOS,
// Claude Code rotates the Keychain copy alone, so when the Keychain is
// unreadable the only remaining value is a plaintext fallback that may be the
// consumed predecessor. Filing that against a slot books a spent refresh token,
// and the capture path then clears the slot's dead-token strike — manufacturing
// the exact stale-consume the strike existed to record.
//
// The bytes returned are the ones the check itself read. Re-reading would make
// the check and the capture two independent reads, and a store that answers the
// first and fails the second would pass the guard and then capture the stale
// fallback anyway.
func (s *Switcher) ReadCaptureCredentials() (string, error) {
	active := s.Creds.ReadActive()
	if active.Degraded {
		return "", fmt.Errorf("%w: the macOS Keychain is unreadable right now (locked, "+
			"or no GUI session), so the only readable credential is a plaintext fallback "+
			"that may be a superseded generation — capturing it would file a spent refresh "+
			"token against this slot. Retry from a GUI terminal", apperr.ErrCredentialRead)
	}
	return active.Value, nil
}

// VerifyCredentialOwnership checks that the credential about to be stored
// actually belongs to the identity naming it.
//
// A capture fills one slot from two sources — the identity out of Claude Code's
// config, the credential out of its credential store — and nothing makes them
// agree. Measured in the field: a session registered one account's address and
// the slot received another account's token, because the config had been
// renamed onto a new profile while the Keychain still held the original
// account's credential. The slot ends up LABELLED one account and CONTAINING
// another, so every later switch to it logs the wrong user in.
//
// The check is ADVISORY in one direction only. Unresolvable — offline, a 401, a
// renamed field — is a notice and the capture proceeds, because this must not
// block a registration that worked before the check existed. Only a resolved
// identity that DISAGREES refuses.
//
// Comparison is uuid-first: uuids are stable where an email can be recycled
// across accounts, so the address decides only when the config carries no uuid.
// The organization is corroborated on both arms.
func (s *Switcher) VerifyCredentialOwnership(ctx context.Context, credentials string, identity LiveIdentity) (CaptureResult, error) {
	unverified := func(why string) (CaptureResult, error) {
		return CaptureResult{Credentials: credentials, Unverified: why}, nil
	}

	if s.Oracle == nil {
		return unverified("no identity oracle is configured")
	}
	token := claudeapi.AccessToken(credentials)
	if token == "" {
		return unverified("there is no access token to resolve")
	}
	if payload, ok := claudeapi.OAuthPayload(credentials); ok && claudeapi.Expired(payload, s.now()) {
		// Unresolvable, exactly like an offline lookup — deliberately NOT a
		// refresh. Consuming a grant retires its predecessor server-side, and
		// every other caller that spends one holds the store lock and Claude
		// Code's credential locks and persists the successor to both stores. A
		// bare refresh here would strand the active store on the spent
		// generation, and on the refusal path would discard the only live copy
		// of that lineage.
		return unverified("the access token is expired")
	}

	profile := s.Oracle.Profile(ctx, token)
	if profile == nil {
		return unverified("the identity lookup did not resolve")
	}

	if identity.AccountUUID != "" {
		// Falls THROUGH to the organization check on a match rather than
		// returning. Returning here would accept a uuid match under a
		// disagreeing organization, and would make the organization check
		// unreachable for every config Claude Code writes, since those all
		// carry a uuid.
		if profile.UUID != identity.AccountUUID {
			return CaptureResult{}, fmt.Errorf("%w: the stored credential does not belong "+
				"to %s: the token resolves to account %s, not %s. Nothing was changed. This "+
				"happens when the config names one account while the credential store still "+
				"holds another's token — a renamed .claude.json over a live Keychain item, "+
				"for instance. Log in as %s in THIS environment, then re-run",
				apperr.ErrConfig, identity.Email, profile.UUID, identity.AccountUUID, identity.Email)
		}
	} else {
		seen := strings.TrimSpace(profile.Email)
		if seen == "" {
			return unverified("the resolved identity carries no address")
		}
		if !strings.EqualFold(seen, identity.Email) {
			return CaptureResult{}, fmt.Errorf("%w: the stored credential does not belong "+
				"to %s: the token resolves to %s. Nothing was changed. This happens when the "+
				"config names one account while the credential store still holds another's "+
				"token. Log in as %s in THIS environment, then re-run",
				apperr.ErrConfig, identity.Email, seen, identity.Email)
		}
	}

	if profile.OrganizationUUID == "" {
		// Structurally absent: the response carried no organization block at
		// all, which is indistinguishable from a personal account. Unverifiable
		// about the organization alone — never affirmative, never condemning.
		return CaptureResult{Credentials: credentials}, nil
	}
	if profile.OrganizationUUID == identity.OrganizationUUID {
		return CaptureResult{Credentials: credentials}, nil
	}
	// The address agrees, so naming it twice says nothing. Name the two
	// organizations that disagree instead.
	return CaptureResult{}, fmt.Errorf("%w: the stored credential for %s belongs to "+
		"organization %s, not %s. Nothing was changed. Two accounts can share an email "+
		"across organizations. Log in as %s in the %s organization in THIS environment, "+
		"then re-run",
		apperr.ErrConfig, identity.Email, orgLabel(profile.OrganizationUUID),
		orgLabel(identity.OrganizationUUID), identity.Email, orgLabel(identity.OrganizationUUID))
}

func orgLabel(uuid string) string {
	if uuid == "" {
		return "personal"
	}
	return uuid
}
