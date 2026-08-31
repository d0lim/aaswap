package updatecheck

import (
	"context"
	json "encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

func newChecker(t *testing.T, handler http.HandlerFunc) (*Checker, *int) {
	t.Helper()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		handler(w, r)
	}))
	t.Cleanup(server.Close)

	return &Checker{
		HTTP: server.Client(), URL: server.URL,
		CacheDir: t.TempDir(),
		Now:      func() time.Time { return testNow },
	}, &calls
}

func TestLatestReadsTheRelease(t *testing.T) {
	checker, _ := newChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.3","html_url":"https://example.com/releases/v1.2.3"}`))
	})

	version, url, ok := checker.Latest(context.Background())
	if !ok {
		t.Fatal("the release was not read")
	}
	// The leading v is a tag convention, not part of the version.
	if version != "1.2.3" {
		t.Errorf("version = %q", version)
	}
	if url != "https://example.com/releases/v1.2.3" {
		t.Errorf("url = %q", url)
	}
}

// A check is a courtesy: it must never turn a working command into an error.
func TestAFailedCheckIsQuiet(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"a server error", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}},
		{"a rate limit", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusForbidden)
		}},
		{"malformed JSON", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`not json`))
		}},
		{"no tag", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker, _ := newChecker(t, tt.handler)
			if _, _, ok := checker.Latest(context.Background()); ok {
				t.Error("a failed check reported a version")
			}
		})
	}

	t.Run("nothing listening", func(t *testing.T) {
		checker := &Checker{
			HTTP: &http.Client{}, URL: "http://127.0.0.1:1/releases",
			CacheDir: t.TempDir(), Now: func() time.Time { return testNow },
		}
		if _, _, ok := checker.Latest(context.Background()); ok {
			t.Error("an unreachable endpoint reported a version")
		}
	})
}

// The result is cached so an interactive command never pays for the check
// twice, and so an offline machine does not retry on every invocation.
func TestTheCheckIsCached(t *testing.T) {
	t.Run("a success", func(t *testing.T) {
		checker, calls := newChecker(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"tag_name":"v1.2.3"}`))
		})
		checker.Latest(context.Background())
		checker.Latest(context.Background())
		if *calls != 1 {
			t.Errorf("requests = %d, want the second served from cache", *calls)
		}
	})

	t.Run("a failure", func(t *testing.T) {
		checker, calls := newChecker(t, func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		})
		checker.Latest(context.Background())
		checker.Latest(context.Background())
		if *calls != 1 {
			t.Errorf("requests = %d, want an offline machine not to retry every time", *calls)
		}
	})

	t.Run("and it expires", func(t *testing.T) {
		now := testNow
		checker, calls := newChecker(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"tag_name":"v1.2.3"}`))
		})
		checker.Now = func() time.Time { return now }

		checker.Latest(context.Background())
		now = now.Add(CacheTTL + time.Hour)
		checker.Latest(context.Background())
		if *calls != 2 {
			t.Errorf("requests = %d, want the stale cache refreshed", *calls)
		}
	})
}

// A corrupt cache costs one request, not an error.
func TestACorruptCacheIsIgnored(t *testing.T) {
	checker, calls := newChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v1.2.3"}`))
	})
	if err := os.WriteFile(filepath.Join(checker.CacheDir, CacheFileName),
		[]byte(`{oops`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := checker.Latest(context.Background()); !ok {
		t.Error("a corrupt cache broke the check")
	}
	if *calls != 1 {
		t.Errorf("requests = %d", *calls)
	}
}

// Compared as numbers, because a string comparison gets "0.10.0" against
// "0.9.0" backwards.
func TestNewer(t *testing.T) {
	tests := []struct {
		candidate, current string
		want               bool
	}{
		{"1.2.4", "1.2.3", true},
		{"1.3.0", "1.2.9", true},
		{"2.0.0", "1.99.99", true},
		{"0.10.0", "0.9.0", true},
		{"1.2.3", "1.2.3", false},
		{"1.2.2", "1.2.3", false},
		{"0.9.0", "0.10.0", false},
		// A tag's leading v is not part of the version.
		{"v1.2.4", "1.2.3", true},
		// A shorter version is padded with zeros, not treated as smaller.
		{"1.3", "1.2.9", true},
		{"1.2", "1.2.0", false},
		// A pre-release suffix is dropped rather than ordered: guessing at
		// "beta" against "rc" and being wrong means nagging about a downgrade.
		{"1.2.4-beta.1", "1.2.3", true},
		{"1.2.3-beta.1", "1.2.3", false},
		// Anything unreadable is not evidence of an upgrade.
		{"", "1.2.3", false},
		{"unknown", "1.2.3", false},
		{"1.2.3", "dev", false},
	}
	for _, tt := range tests {
		if got := Newer(tt.candidate, tt.current); got != tt.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", tt.candidate, tt.current, got, tt.want)
		}
	}
}

// The notice names the command for however this copy got here, because "a new
// version exists" without that is a chore rather than help.
func TestNoticeNamesTheUpgradeCommand(t *testing.T) {
	tests := []struct {
		method Method
		want   string
	}{
		{Homebrew, "brew upgrade"},
		{GoInstall, "go install"},
		{Unknown, "releases page"},
	}
	for _, tt := range tests {
		notice := Notice("1.2.4", "1.2.3", tt.method)
		if !strings.Contains(notice, tt.want) {
			t.Errorf("Notice for %q = %q, want it to mention %q", tt.method, notice, tt.want)
		}
		if !strings.Contains(notice, "1.2.4") || !strings.Contains(notice, "1.2.3") {
			t.Errorf("Notice = %q, want both versions", notice)
		}
	}
}

func TestDetectMethod(t *testing.T) {
	// Whatever this test binary is, detection must answer without failing.
	if method := DetectMethod(); method == "" {
		t.Error("DetectMethod returned nothing")
	}
	// An unknown method has no command to offer, and must not invent one.
	if Unknown.UpgradeCommand() != "" {
		t.Errorf("Unknown.UpgradeCommand = %q, want nothing",
			Unknown.UpgradeCommand())
	}
}

// The cache is written even when the check fails, which is the whole point of
// caching a failure.
func TestTheCacheRecordsAFailure(t *testing.T) {
	checker, _ := newChecker(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	checker.Latest(context.Background())

	data, err := os.ReadFile(filepath.Join(checker.CacheDir, CacheFileName))
	if err != nil {
		t.Fatal(err)
	}
	var cached map[string]any
	if err := json.Unmarshal(data, &cached); err != nil {
		t.Fatal(err)
	}
	if cached["checkedAt"] == "" {
		t.Errorf("the cache records no time: %v", cached)
	}
	if _, present := cached["version"]; present {
		t.Errorf("a failed check cached a version: %v", cached)
	}
}
