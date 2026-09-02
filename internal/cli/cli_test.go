package cli

import (
	"bytes"
	"cmp"
	"context"
	json "encoding/json/v2"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/session"

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
	// byProvider holds one Switcher per provider, so a test addressing a
	// second one gets a store scoped to it rather than the default's.
	byProvider map[string]*swap.Switcher
	out, err   bytes.Buffer
	in         bytes.Buffer
	now        time.Time
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

	h := &harness{t: t, now: testNow, byProvider: map[string]*swap.Switcher{}}
	// Pinned the way a shell session pins it. Production has no default
	// provider — it asks when the store cannot say — and a harness whose
	// every test had to answer that question would drown the thing each test
	// is about. Tests OF the choosing unpin it with t.Setenv(ProviderEnv, "").
	t.Setenv(ProviderEnv, swap.ProviderClaude)

	// One Switcher per provider, BUILT for it rather than relabelled. The
	// credential store and the profile store are scoped by provider, so a
	// harness that reused one Switcher across providers would let every test
	// pass while the real wiring read the wrong tool's files.
	build := func(name string) *swap.Switcher {
		if existing, ok := h.byProvider[name]; ok {
			return existing
		}
		spec := provider.MustLookup(cmp.Or(name, provider.Claude))
		s := &swap.Switcher{
			Provider:     name,
			FetchStagger: time.Millisecond,
			Paths:        resolver,
			Creds: credstore.NewForProvider(resolver, root,
				keychain.NewWithRunner(refusingKeychain{}, 0), name,
				swap.LiveLayout(resolver, name)),
			// Built the way production builds it. A fixture that leaves this
			// nil disables every profile-credential check, and the session
			// tests then pass without exercising the thing they name.
			Profiles: provider.NewProfiles(spec, resolver.Platform, nil),
			Usage:    usagestore.NewForProvider(resolver.CacheDir(), name),
			Settings: settings.Defaults(),
		}
		s.SetClock(func() time.Time { return h.now })
		h.byProvider[name] = s
		return s
	}
	h.switcher = build(swap.ProviderClaude)

	h.app = &App{
		Out: &h.out,
		Err: &h.err,
		In:  &h.in,
		NewSwitcher: func(provider string) (*swap.Switcher, error) {
			return build(provider), nil
		},
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
		Choose:      h.app.Choose,
		// Without this a `run` test exec()s the real binary and REPLACES the
		// test process, which reads as a pass because the replacement exits 0.
		HandOver:    h.app.HandOver,
		provider:    h.app.provider,
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
		roster.Insert(num, &swap.Account{Email: email, UUID: "acct-" + num})
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
	roster.SetActive(num)
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

// launched records a handover instead of performing it, so a test can assert
// which binary `run` chose and what environment it built.
type launched struct {
	binary string
	args   []string
	env    []string
	called bool
}

// capturing makes `run` record its handover rather than exec into it.
func (h *harness) capturing() *launched {
	record := &launched{}
	h.app.HandOver = func(binary string, args, env []string) error {
		record.binary, record.args, record.env, record.called = binary, args, env, true
		return nil
	}
	return record
}

// env reads one variable out of a recorded launch.
func (l *launched) env_(name string) (string, bool) {
	for _, entry := range l.env {
		if key, value, found := strings.Cut(entry, "="); found && key == name {
			return value, true
		}
	}
	return "", false
}

// onPath puts a stub executable named binary on PATH for this test, so
// exec.LookPath resolves it without the real tool being installed.
//
// On Windows LookPath resolves a bare name only through PATHEXT, so the stub
// needs one of those extensions — and a shell script would not run there
// anyway. The auth probe does run the stub (`claude auth status`), and an empty
// exit-0 answer reads as Unknown on every platform, which is what these tests
// rely on.
func (h *harness) onPath(t *testing.T, binary string) {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, binary)
	body := "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		stub += ".cmd"
		body = "@exit /b 0\r\n"
	}
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// sessionDir is where a provider's account keeps its isolated profile.
func (h *harness) sessionDir(t *testing.T, provider, name, email string) string {
	t.Helper()
	s, err := h.app.NewSwitcher(provider)
	if err != nil {
		t.Fatal(err)
	}
	return session.DirFor(s.BackupRoot(), s.Spec().Name, name, email)
}

// stashUnclaimed preserves a credential that belongs to no managed account,
// which is the state `account unclaimed` reports on.
func (h *harness) stashUnclaimed(t *testing.T, credentials string) {
	t.Helper()
	if _, err := h.switcher.Creds.WriteUnclaimed(credentials,
		credstore.StashEntry{Reason: "test"}, h.now); err != nil {
		t.Fatal(err)
	}
}
