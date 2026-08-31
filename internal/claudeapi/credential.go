package claudeapi

import (
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"fmt"
	"time"

	"github.com/realiti4/claude-swap/internal/usage"
)

// oauthKey is the member of a Claude Code credential document that holds the
// login itself. Its siblings are shared integrations and device tokens, which
// this package never touches.
const oauthKey = "claudeAiOauth"

// ExpiryBuffer is how far ahead of the stated expiry a token counts as expired,
// so a refresh happens before a request goes out rather than after it comes
// back 401.
const ExpiryBuffer = 5 * time.Minute

// Document is a parsed credential JSON object.
//
// It is a map and not a struct because a refresh rewrites four members and must
// hand back everything else byte-for-byte: unknown siblings belong to Claude
// Code, and a struct would silently drop the ones this version has not heard
// of.
type Document map[string]any

// ParseDocument parses a credential JSON object, reporting false when the input
// is not one.
func ParseDocument(credentials string) (Document, bool) {
	if credentials == "" {
		return nil, false
	}
	var doc Document
	if err := json.Unmarshal([]byte(credentials), &doc); err != nil || doc == nil {
		return nil, false
	}
	return doc, true
}

// Encode serializes a document back to JSON.
//
// Deterministic so the same inputs always produce the same bytes; JSON member
// order carries no meaning to Claude Code, which only parses this.
func (d Document) Encode() (string, error) {
	encoded, err := json.Marshal(map[string]any(d), json.Deterministic(true))
	if err != nil {
		return "", fmt.Errorf("encoding credentials: %w", err)
	}
	return string(encoded), nil
}

// OAuth returns the Claude AI OAuth payload, reporting false when the document
// carries none.
func (d Document) OAuth() (map[string]any, bool) {
	payload, ok := d[oauthKey].(map[string]any)
	return payload, ok
}

// OAuthPayload extracts the Claude AI OAuth payload from a credentials JSON
// string, reporting false for anything that is not one.
func OAuthPayload(credentials string) (map[string]any, bool) {
	doc, ok := ParseDocument(credentials)
	if !ok {
		return nil, false
	}
	return doc.OAuth()
}

// AccessToken returns the OAuth access token held in a credentials JSON string,
// or empty when there is none.
func AccessToken(credentials string) string {
	payload, ok := OAuthPayload(credentials)
	if !ok {
		return ""
	}
	token, _ := payload["accessToken"].(string)
	return token
}

// RefreshToken returns the OAuth refresh token held in a payload, or empty.
func RefreshToken(payload map[string]any) string {
	token, _ := payload["refreshToken"].(string)
	return token
}

// ExpiresAt returns a payload's stated expiry, reporting false when it carries
// none or carries a non-numeric one.
func ExpiresAt(payload map[string]any) (time.Time, bool) {
	ms, ok := payload["expiresAt"].(float64)
	if !ok {
		return time.Time{}, false
	}
	return time.UnixMilli(int64(ms)).UTC(), true
}

// Expired reports whether a payload's access token is expired or will be within
// [ExpiryBuffer].
//
// An absent or non-numeric expiry reports false — not expired. The alternative
// would refresh on every pass for a credential shape that simply does not state
// an expiry, spending a grant to learn nothing.
func Expired(payload map[string]any, now time.Time) bool {
	expiry, ok := ExpiresAt(payload)
	if !ok {
		return false
	}
	return !now.Add(ExpiryBuffer).Before(expiry)
}

// Fingerprint prefixes.
const (
	// fingerprintRefresh hashes the refresh token, so the fingerprint survives
	// access-token rotation and two generations of one OAuth lineage compare
	// equal.
	fingerprintRefresh = "sha256:"
	// fingerprintFull hashes the whole credential, for shapes that carry no
	// refresh token. API keys and setup tokens never rotate, so content
	// identity is lineage identity for them.
	fingerprintFull = "sha256-full:"
)

// Fingerprint returns a stable identity fingerprint for a stored credential, or
// empty for empty input.
//
// The two schemes are prefixed distinctly so a full-content hash can never
// collide with a refresh-token hash of different bytes.
//
// Empty is returned only for empty input. A caller asking "did this credential
// change?" must never get an empty answer for real bytes, or every comparison
// against it would degenerate to "changed".
func Fingerprint(credentials string) string {
	if credentials == "" {
		return ""
	}
	if payload, ok := OAuthPayload(credentials); ok {
		if token := RefreshToken(payload); token != "" {
			return fingerprintRefresh + hash(token)
		}
	}
	return fingerprintFull + hash(credentials)
}

func hash(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TokenStatus returns a short debug summary of stored OAuth token state, or
// false when the credential carries no OAuth payload at all.
func TokenStatus(credentials string, now time.Time) (string, bool) {
	payload, ok := OAuthPayload(credentials)
	if !ok {
		return "", false
	}

	refresh := "no"
	if RefreshToken(payload) != "" {
		refresh = "yes"
	}

	expiry, ok := ExpiresAt(payload)
	if !ok {
		return fmt.Sprintf("oauth: unknown expiry, refresh token %s", refresh), true
	}

	state := "fresh"
	if Expired(payload, now) {
		state = "expired"
	}
	return fmt.Sprintf("oauth: %s, refresh token %s, expires %s in %s",
		state, refresh, usage.ResetClock(expiry, now), usage.Countdown(expiry, now)), true
}
