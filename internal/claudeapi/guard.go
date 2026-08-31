package claudeapi

import (
	"net/url"
	"os"
	"strings"
	"testing"
)

// AllowRealNetworkEnv opts a test into reaching Anthropic's real endpoints. It
// exists for a live smoke test run deliberately by hand, and must never be set
// in CI or for any other test.
const AllowRealNetworkEnv = "CSWAP_ALLOW_REAL_NETWORK"

// realHosts are the production endpoints a test must never reach.
var realHosts = []string{"anthropic.com", "claude.com", "claude.ai"}

// guardRealEndpoint stops a test binary from talking to Anthropic.
//
// A test that reached the token endpoint would spend the developer's own
// refresh token — a single-use grant — and rotate the credential out from under
// their running Claude Code; one that reached the usage endpoint would burn
// requests from the very hourly budget this package exists to ration, and could
// leave the developer rate-limited by their own test suite.
//
// This is the network arm of the same safety net paths.guardRealStore provides
// for the filesystem and keychain.guardRealKeychain for the Keychain: point the
// Client at an httptest server instead.
func guardRealEndpoint(u *url.URL) {
	if !testing.Testing() || os.Getenv(AllowRealNetworkEnv) != "" {
		return
	}
	host := strings.ToLower(u.Hostname())
	for _, real := range realHosts {
		if host == real || strings.HasSuffix(host, "."+real) {
			panic("claude-swap test safety net: a test tried to reach " + u.String() +
				", which would spend the developer's own refresh token or usage budget.\n" +
				"Point the Client's URLs at an httptest server, or — only for a test that " +
				"genuinely needs the live endpoint — set " + AllowRealNetworkEnv + "=1 for that test alone.")
		}
	}
}
