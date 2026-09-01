// Package updatecheck tells the user when a newer ccswap exists, and how to get
// it.
//
// It does NOT upgrade in place. A Go binary installed by a package manager, a
// tap, or `go install` belongs to that installer, and replacing it from
// underneath would leave the installer's records wrong and the next `upgrade`
// through it confused. So this reports the release and names the command for
// however the running binary got here.
package updatecheck

import (
	"context"
	json "encoding/json/v2"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/d0lim/ccswap/internal/fsutil"
)

// ReleasesURL is where the latest release is published.
const ReleasesURL = "https://api.github.com/repos/d0lim/ccswap/releases/latest"

// CacheTTL is how long a check is reused.
//
// Long enough that an interactive command never pays for the check twice in a
// session, short enough that a release is noticed the same day. The result is
// cached whether or not the request succeeded, so a machine with no network
// does not retry on every invocation.
const CacheTTL = 24 * time.Hour

// CacheFileName holds the last check.
const CacheFileName = "update-check.json"

// Timeout bounds the request. This runs alongside a command the user is waiting
// on, so it must never be the reason one feels slow.
const Timeout = 2 * time.Second

// Method is how the running binary was installed.
type Method string

const (
	// Homebrew: installed from the tap.
	Homebrew Method = "homebrew"
	// GoInstall: built into the Go binary directory.
	GoInstall Method = "go-install"
	// Unknown: downloaded by hand, or through something not recognized here.
	Unknown Method = "unknown"
)

// UpgradeCommand names how to upgrade, given how the binary got here.
func (m Method) UpgradeCommand() string {
	switch m {
	case Homebrew:
		return "brew upgrade ccswap"
	case GoInstall:
		return "go install github.com/d0lim/ccswap/cmd/ccswap@latest"
	}
	return ""
}

// DetectMethod works out how the running binary was installed, from where it
// sits.
//
// Path-based rather than recorded at build time: one binary can be distributed
// several ways, and the build has no idea which one this copy came through.
func DetectMethod() Method {
	executable, err := os.Executable()
	if err != nil {
		return Unknown
	}
	if resolved, err := filepath.EvalSymlinks(executable); err == nil {
		executable = resolved
	}

	// A Homebrew install lives under the Cellar, whatever the prefix.
	if strings.Contains(executable, string(filepath.Separator)+"Cellar"+string(filepath.Separator)) {
		return Homebrew
	}
	if gobin := os.Getenv("GOBIN"); gobin != "" && strings.HasPrefix(executable, gobin) {
		return GoInstall
	}
	if gopath := os.Getenv("GOPATH"); gopath != "" {
		if strings.HasPrefix(executable, filepath.Join(gopath, "bin")) {
			return GoInstall
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if strings.HasPrefix(executable, filepath.Join(home, "go", "bin")) {
			return GoInstall
		}
	}
	return Unknown
}

// release is the part of a release payload this cares about.
type release struct {
	TagName string `json:"tag_name"`
	HTMLURL string `json:"html_url"`
}

// cache is the last check's result.
type cache struct {
	CheckedAt string `json:"checkedAt"`
	// Version is empty when the check itself failed, which is still worth
	// caching: a machine with no network should not retry on every command.
	Version string `json:"version,omitzero"`
	URL     string `json:"url,omitzero"`
}

// Checker looks up the latest release.
type Checker struct {
	HTTP *http.Client
	URL  string
	// CacheDir holds the last result. Empty disables caching, which only a test
	// should want.
	CacheDir string
	Now      func() time.Time
}

// New returns a checker pointed at the real releases endpoint.
func New(cacheDir string) *Checker {
	return &Checker{
		HTTP:     &http.Client{},
		URL:      ReleasesURL,
		CacheDir: cacheDir,
		Now:      time.Now,
	}
}

func (c *Checker) now() time.Time {
	if c.Now == nil {
		return time.Now()
	}
	return c.Now()
}

// Latest returns the newest published version, reporting false when it could
// not be established.
//
// Never fails outward: an update check is a courtesy, and it must not turn a
// working command into an error because a machine is offline.
func (c *Checker) Latest(ctx context.Context) (version, url string, ok bool) {
	if cached, fresh := c.readCache(); fresh {
		return cached.Version, cached.URL, cached.Version != ""
	}

	version, url = c.fetch(ctx)
	// Written whether or not the fetch worked: caching the failure is what
	// stops an offline machine retrying on every invocation.
	c.writeCache(cache{
		CheckedAt: c.now().UTC().Format(time.RFC3339),
		Version:   version, URL: url,
	})
	return version, url, version != ""
}

func (c *Checker) fetch(ctx context.Context) (version, url string) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return "", ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "ccswap")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}

	var latest release
	if err := json.UnmarshalRead(resp.Body, &latest); err != nil {
		return "", ""
	}
	return strings.TrimPrefix(latest.TagName, "v"), latest.HTMLURL
}

func (c *Checker) cachePath() string {
	if c.CacheDir == "" {
		return ""
	}
	return filepath.Join(c.CacheDir, CacheFileName)
}

func (c *Checker) readCache() (cache, bool) {
	path := c.cachePath()
	if path == "" {
		return cache{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return cache{}, false
	}
	var cached cache
	if err := json.Unmarshal(data, &cached); err != nil {
		return cache{}, false
	}
	checkedAt, err := time.Parse(time.RFC3339, cached.CheckedAt)
	if err != nil {
		return cache{}, false
	}
	return cached, c.now().Sub(checkedAt) < CacheTTL
}

func (c *Checker) writeCache(entry cache) {
	path := c.cachePath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return
	}
	// Best effort throughout: a cache that cannot be written costs one extra
	// request, which is not worth telling the user about.
	_ = fsutil.WriteFileAtomic(path, data)
}

// Newer reports whether one version supersedes another.
//
// Compared field by field as numbers, so "0.10.0" beats "0.9.0" — which a
// string comparison gets backwards. Anything unparseable reports false: a
// version this cannot read is not evidence of an upgrade.
func Newer(candidate, current string) bool {
	a, aOK := parseVersion(candidate)
	b, bOK := parseVersion(current)
	if !aOK || !bOK {
		return false
	}
	for i := range max(len(a), len(b)) {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			return x > y
		}
	}
	return false
}

// parseVersion splits a dotted version, ignoring any pre-release suffix.
func parseVersion(v string) ([]int, bool) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	// A pre-release suffix is dropped rather than compared: ordering "beta"
	// against "rc" is a guess, and being wrong means nagging about a downgrade.
	if cut := strings.IndexAny(v, "-+"); cut >= 0 {
		v = v[:cut]
	}
	if v == "" {
		return nil, false
	}
	var out []int
	for part := range strings.SplitSeq(v, ".") {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		out = append(out, n)
	}
	return out, true
}
