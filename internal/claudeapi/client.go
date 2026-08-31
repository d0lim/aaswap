// Package claudeapi is the only place ccswap talks to Anthropic over the
// network: the OAuth token endpoint, the profile endpoint that resolves a token
// to an account identity, and the usage endpoint that reports rate-limit
// windows.
//
// # Locks and the network
//
// No call in this package may be made while a credential or config lock is
// held. Those locks are contended with Claude Code itself, and a network
// exchange can block for the full timeout; holding one across a request would
// stall the editor for seconds at a time. Callers acquire, read, release, then
// call.
//
// # Classification is the point
//
// Every exchange here reports an [ErrorKind] rather than a bare error, because
// what callers do next depends entirely on *why* it failed: a dead refresh
// lineage quarantines a slot, a 429 widens the poll interval for every account
// on the token, and a network blip changes nothing. The classification rules
// lean deliberately toward "transient" — a misclassified transient costs one
// retry, while a misclassified permanent locks a user out of a live account.
package claudeapi

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Endpoints. Refresh goes to the platform host, everything else to the API
// host; that split is the server's, not ours.
const (
	DefaultTokenURL   = "https://platform.claude.com/v1/oauth/token"
	DefaultProfileURL = "https://api.anthropic.com/api/oauth/profile"
	DefaultUsageURL   = "https://api.anthropic.com/api/oauth/usage"

	// ClientID identifies ccswap to the token endpoint. It is not a secret —
	// public OAuth clients have no secret to keep — and is the same value
	// Claude Code itself presents.
	ClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	// BetaHeader opts the usage endpoint into the OAuth-authenticated surface.
	BetaHeader = "oauth-2025-04-20"

	userAgent = "claude-swap/1.0"
)

// Default per-request budgets.
//
// The refresh budget is the widest because a token grant is the one exchange
// with nothing to fall back on. Callers that hold locks other processes contend
// for must pass a budget comfortably inside those contenders' acquire timeout.
const (
	DefaultRefreshTimeout = 10 * time.Second
	DefaultProfileTimeout = 5 * time.Second
	DefaultUsageTimeout   = 5 * time.Second
)

// maxBody bounds how much of a response is read.
//
// Usage and profile payloads are a few kilobytes; a megabyte is far past any
// legitimate response and stops a misbehaving proxy from streaming into memory
// while a lock-free but time-bounded call waits on it.
const maxBody = 1 << 20

// Client talks to the Anthropic endpoints.
//
// The zero value is not usable; call [New]. Every field is exported so tests
// can point the client at an httptest server, which is the only supported way
// to exercise the transport — see the guard in guard.go.
type Client struct {
	HTTP       *http.Client
	TokenURL   string
	ProfileURL string
	UsageURL   string

	RefreshTimeout time.Duration
	ProfileTimeout time.Duration
	UsageTimeout   time.Duration
}

// New returns a client pointed at the production endpoints.
func New() *Client {
	return &Client{
		// No client-wide timeout: each call sets its own budget through the
		// context, and a single shared deadline would silently cap the widest.
		HTTP:           &http.Client{},
		TokenURL:       DefaultTokenURL,
		ProfileURL:     DefaultProfileURL,
		UsageURL:       DefaultUsageURL,
		RefreshTimeout: DefaultRefreshTimeout,
		ProfileTimeout: DefaultProfileTimeout,
		UsageTimeout:   DefaultUsageTimeout,
	}
}

// httpError is a non-2xx response, carrying what classification needs.
type httpError struct {
	status int
	header http.Header
	body   []byte
}

func (e *httpError) Error() string {
	return fmt.Sprintf("%s: %s", http.StatusText(e.status), truncate(string(e.body), 500))
}

// do performs one request under the given budget and returns the response body.
//
// A non-2xx status comes back as an *httpError rather than a body, so callers
// never have to remember to check the status themselves.
func (c *Client) do(ctx context.Context, req *http.Request, timeout time.Duration) ([]byte, error) {
	guardRealEndpoint(req.URL)

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	resp, err := c.HTTP.Do(req.WithContext(ctx))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &httpError{status: resp.StatusCode, header: resp.Header, body: body}
	}
	return body, nil
}

func (c *Client) newRequest(method, endpoint string, body []byte, headers map[string]string) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	return req, nil
}

// classify maps a failed exchange to a kind and, for a rate-limited one, the
// server's Retry-After.
//
// Ambiguity resolves toward the retryable kinds by design; see the package doc.
func classify(err error) (ErrorKind, *time.Duration) {
	if httpErr, ok := errors.AsType[*httpError](err); ok {
		return HTTPKind(httpErr.status), retryAfter(httpErr.header)
	}
	// A context deadline is this client's own budget expiring, which is a
	// timeout from the caller's point of view even though net/http reports it
	// as a cancellation.
	if errors.Is(err, context.DeadlineExceeded) {
		return KindTimeout, nil
	}
	if netErr, ok := errors.AsType[net.Error](err); ok && netErr.Timeout() {
		return KindTimeout, nil
	}
	if errors.Is(err, errBadResponse) {
		return KindBadResponse, nil
	}
	if _, ok := errors.AsType[*url.Error](err); ok {
		return KindNetwork, nil
	}
	return KindUnknown, nil
}

// errBadResponse marks a body the endpoint sent that ccswap could not decode.
//
// The decode is wrapped rather than classified by inspecting the JSON package's
// own error types, so this stays correct across encoding/json changes and does
// not depend on which of them a given malformation produces.
var errBadResponse = errors.New("undecodable response body")

func decodeJSON(body []byte, out any) error {
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("%w: %w", errBadResponse, err)
	}
	return nil
}

// parseSeconds parses a Retry-After delay-seconds value. The header is defined
// as an integer, but a float is accepted rather than discarded — the value only
// has to be usable as a duration.
func parseSeconds(raw string) (float64, error) {
	return strconv.ParseFloat(raw, 64)
}

// retryAfter parses the Retry-After header's delay-seconds form.
//
// The HTTP-date form is ignored: the endpoint has never been observed to send
// it, and a misparsed date would produce a wildly wrong backoff. A negative
// value clamps to zero rather than being discarded — the server is saying "now",
// not "never".
func retryAfter(header http.Header) *time.Duration {
	raw := strings.TrimSpace(header.Get("Retry-After"))
	if raw == "" {
		return nil
	}
	seconds, err := parseSeconds(raw)
	if err != nil {
		return nil
	}
	d := max(time.Duration(seconds*float64(time.Second)), 0)
	return &d
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
