package cli

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/keychain"
	"github.com/d0lim/aaswap/internal/paths"
	"github.com/d0lim/aaswap/internal/platform"
	"github.com/d0lim/aaswap/internal/settings"
	"github.com/d0lim/aaswap/internal/swap"
	"github.com/d0lim/aaswap/internal/usage"
	"github.com/d0lim/aaswap/internal/usagestore"
)

func TestMain(m *testing.M) {
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	os.Exit(m.Run())
}

var testNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

// refusingKeychain stands in for a host with no Keychain, so credentials live
// in files a test can inspect.
type refusingKeychain struct{}

func (refusingKeychain) Run(context.Context, []string, string) (keychain.Result, error) {
	return keychain.Result{}, os.ErrNotExist
}

// fakeFetcher answers usage fetches without a network.
type fakeFetcher struct{ byNumber map[string]*usage.Result }

func (f *fakeFetcher) FetchUsageForAccount(_ context.Context, req claudeapi.FetchRequest) claudeapi.UsageOutcome {
	if result, ok := f.byNumber[req.AccountNum]; ok {
		return claudeapi.UsageOutcome{Usage: result}
	}
	return claudeapi.UsageOutcome{Error: claudeapi.KindTimeout}
}

// harness runs the CLI against a temporary store.
//
// Nothing here can reach the developer's real accounts: the home directory is a
// temp dir, the Keychain refuses every call, and there is no network client at
// all unless a test supplies a fake one.
type harness struct {
	t        *testing.T
	app      *App
	switcher *swap.Switcher
	out, err bytes.Buffer
	in       bytes.Buffer
	now      time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, ".local", "share", paths.BackupDirName)
	resolver := paths.New(home, platform.Linux)
	if err := os.MkdirAll(resolver.ClaudeConfigHome(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}

	h := &harness{t: t, now: testNow}
	h.switcher = &swap.Switcher{
		FetchStagger: time.Millisecond,
		Paths:        resolver,
		Creds:        credstore.New(resolver, root, keychain.NewWithRunner(refusingKeychain{}, 0)),
		Usage:        usagestore.New(resolver.CacheDir()),
		Settings:     settings.Defaults(),
	}
	h.switcher.SetClock(func() time.Time { return h.now })

	h.app = &App{
		Out:         &h.out,
		Err:         &h.err,
		In:          &h.in,
		NewSwitcher: func() (*swap.Switcher, error) { return h.switcher, nil },
	}
	return h
}

// run executes one command line and returns the exit code.
func (h *harness) run(args ...string) int {
	h.t.Helper()
	h.out.Reset()
	h.err.Reset()
	// A fresh App per run, so persistent flags from one invocation cannot leak
	// into the next — which is what a real process guarantees.
	app := &App{
		Out: &h.out, Err: &h.err, In: h.app.In,
		NewSwitcher: h.app.NewSwitcher,
		Confirm:     h.app.Confirm,
		awaitTuning: h.app.awaitTuning,
	}
	return app.Execute(h.t.Context(), args)
}

func (h *harness) stdout() string { return h.out.String() }
func (h *harness) stderr() string { return h.err.String() }

// decodeJSON parses stdout as one JSON object.
func (h *harness) decodeJSON() map[string]any {
	h.t.Helper()
	var out map[string]any
	if err := json.Unmarshal(h.out.Bytes(), &out); err != nil {
		h.t.Fatalf("stdout is not one JSON object: %v\n%s", err, h.out.String())
	}
	return out
}

// seed registers accounts with stored credentials and configs.
func (h *harness) seed(accounts map[string]string) {
	h.t.Helper()
	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		h.t.Fatal(err)
	}
	for num, email := range accounts {
		roster.Insert(num, &swap.Account{Email: email, UUID: "acct-" + num}, h.now)
		if err := h.switcher.Creds.WriteAccount(num, email,
			`{"claudeAiOauth":{"accessToken":"tok-`+num+`","refreshToken":"r-`+num+`"}}`); err != nil {
			h.t.Fatal(err)
		}
		if err := h.switcher.WriteAccountConfig(num, email,
			`{"oauthAccount":{"emailAddress":"`+email+`"}}`); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := h.switcher.WriteRoster(roster); err != nil {
		h.t.Fatal(err)
	}
}

// login makes an account the live one.
func (h *harness) login(num, email string) {
	h.t.Helper()
	config := `{"oauthAccount":{"emailAddress":"` + email + `","accountUuid":"acct-` + num + `"},"projects":{}}`
	if err := os.WriteFile(h.switcher.Paths.GlobalConfigPath(), []byte(config), 0o600); err != nil {
		h.t.Fatal(err)
	}
	if err := h.switcher.Creds.WriteActive(
		`{"claudeAiOauth":{"accessToken":"tok-` + num + `","refreshToken":"r-` + num + `"}}`); err != nil {
		h.t.Fatal(err)
	}
	roster, err := h.switcher.RosterOrEmpty()
	if err != nil {
		h.t.Fatal(err)
	}
	roster.SetActive(num, h.now)
	if err := h.switcher.WriteRoster(roster); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) measuring(byNumber map[string]*usage.Result) {
	h.switcher.Fetcher = &fakeFetcher{byNumber: byNumber}
}

func measured(pct float64) *usage.Result {
	return &usage.Result{
		FiveHour: &usage.Window{Pct: pct / 2},
		SevenDay: &usage.Window{Pct: pct},
	}
}

func wantContains(t *testing.T, text string, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(text, fragment) {
			t.Errorf("output does not contain %q:\n%s", fragment, text)
		}
	}
}
