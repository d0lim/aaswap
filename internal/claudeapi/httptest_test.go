package claudeapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// handlerFunc records the request it received so a test can assert on what went
// over the wire, not just on what came back.
type recorder struct {
	calls   int
	method  string
	path    string
	header  http.Header
	body    string
	respond func(w http.ResponseWriter, r *http.Request)
}

func (rec *recorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec.calls++
	rec.method = r.Method
	rec.path = r.URL.Path
	rec.header = r.Header.Clone()
	buf := make([]byte, r.ContentLength)
	if r.ContentLength > 0 {
		_, _ = r.Body.Read(buf)
	}
	rec.body = string(buf)
	rec.respond(w, r)
}

// newTestClient points every endpoint at one httptest server. The real hosts
// are unreachable from a test binary — see guardRealEndpoint — so this is the
// only way to exercise the transport.
func newTestClient(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) (*Client, *recorder) {
	t.Helper()
	rec := &recorder{respond: respond}
	server := httptest.NewServer(rec)
	t.Cleanup(server.Close)

	c := New()
	c.TokenURL = server.URL + "/v1/oauth/token"
	c.ProfileURL = server.URL + "/api/oauth/profile"
	c.UsageURL = server.URL + "/api/oauth/usage"
	// Short budgets so a test that means to time out does not sit for ten
	// seconds doing it.
	c.RefreshTimeout = 2 * time.Second
	c.ProfileTimeout = 2 * time.Second
	c.UsageTimeout = 2 * time.Second
	return c, rec
}

// respondJSON writes a status and a raw body.
func respondJSON(status int, body string, headers ...[2]string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		for _, h := range headers {
			w.Header().Set(h[0], h[1])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}
