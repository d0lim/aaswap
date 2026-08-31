package claudeapi

import (
	"net/url"
	"strings"
	"testing"
)

// The safety net that keeps a test suite from spending the developer's own
// refresh token or usage budget. Without it, one careless URL in a fixture
// would rotate their live credential or rate-limit them from their own machine.
func TestTheRealEndpointsAreUnreachableFromATest(t *testing.T) {
	tests := []struct {
		name string
		call func(*Client) any
	}{
		{"the token endpoint", func(c *Client) any {
			return c.Refresh(t.Context(), `{"claudeAiOauth":{"refreshToken":"r"}}`, testNow)
		}},
		{"the profile endpoint", func(c *Client) any {
			return c.Profile(t.Context(), "token")
		}},
		{"the usage endpoint", func(c *Client) any {
			result, _ := c.FetchUsage(t.Context(), "token")
			return result
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				recovered := recover()
				if recovered == nil {
					t.Fatal("a test reached a production endpoint without tripping the guard")
				}
				if message, ok := recovered.(string); !ok || !strings.Contains(message, "test safety net") {
					t.Errorf("panic = %v, want the safety-net message", recovered)
				}
			}()
			tt.call(New())
		})
	}
}

func TestGuardCoversTheRealHostsAndOnlyThose(t *testing.T) {
	blocked := []string{
		"https://api.anthropic.com/x",
		"https://platform.claude.com/x",
		"https://claude.ai/x",
		// A subdomain is the same operator.
		"https://console.anthropic.com/x",
		// Case in a hostname is not significant.
		"https://API.Anthropic.COM/x",
	}
	allowed := []string{
		"http://127.0.0.1:8080/x",
		"http://localhost/x",
		// A lookalike registered by someone else must not be waved through as
		// "close enough", but neither is it ours to block.
		"https://notanthropic.com/x",
	}

	for _, raw := range blocked {
		if !panics(func() { guardRealEndpoint(mustURL(t, raw)) }) {
			t.Errorf("%s was not blocked", raw)
		}
	}
	for _, raw := range allowed {
		if panics(func() { guardRealEndpoint(mustURL(t, raw)) }) {
			t.Errorf("%s was blocked", raw)
		}
	}
}

// The opt-out exists for a deliberate live smoke test and nothing else.
func TestTheGuardCanBeOptedOutOf(t *testing.T) {
	t.Setenv(AllowRealNetworkEnv, "1")
	if panics(func() { guardRealEndpoint(mustURL(t, "https://api.anthropic.com/x")) }) {
		t.Error("the guard blocked a request despite the opt-out")
	}
}

func panics(fn func()) (did bool) {
	defer func() { did = recover() != nil }()
	fn()
	return
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
