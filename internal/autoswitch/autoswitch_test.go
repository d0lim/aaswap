package autoswitch

import (
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

type refusingKeychain struct{}

func (refusingKeychain) Run(context.Context, []string, string) (keychain.Result, error) {
	return keychain.Result{}, os.ErrNotExist
}

// fakeFetcher answers usage fetches without a network.
type fakeFetcher struct{ byNumber map[string]*usage.Result }

func (f *fakeFetcher) FetchUsageForAccount(_ context.Context, req claudeapi.FetchRequest) claudeapi.UsageOutcome {
	if result, ok := f.byNumber[req.AccountNum]; ok && result != nil {
		return claudeapi.UsageOutcome{Usage: result}
	}
	return claudeapi.UsageOutcome{Error: claudeapi.KindTimeout}
}

// collector records every event a tick emitted.
type collector struct{ events []Event }

func (c *collector) Emit(event Event) { c.events = append(c.events, event) }

// find returns the first event of a kind.
func (c *collector) find(kind string) (Event, bool) {
	for _, event := range c.events {
		if event.Kind == kind {
			return event, true
		}
	}
	return Event{}, false
}

func (c *collector) reasons() []string {
	var out []string
	for _, event := range c.events {
		if event.Kind == KindNoSwitch {
			out = append(out, event.Reason)
		}
	}
	return out
}

type fixture struct {
	// Not embedded: a test reads as "the engine does X" rather than "the
	// fixture does X".
	engine *Engine
	t      *testing.T
	home   string
	now    time.Time
	events *collector
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, ".local", "share", paths.BackupDirName)
	resolver := paths.New(home, platform.Linux)
	for _, dir := range []string{resolver.ClaudeConfigHome(), root} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	f := &fixture{t: t, home: home, now: testNow, events: &collector{}}
	switcher := &swap.Switcher{
		FetchStagger: time.Millisecond,
		Paths:        resolver,
		Creds:        credstore.New(resolver, root, keychain.NewWithRunner(refusingKeychain{}, 0)),
		Usage:        usagestore.New(resolver.CacheDir()),
		Settings:     settings.Defaults(),
	}
	switcher.SetClock(func() time.Time { return f.now })

	f.engine = &Engine{
		Switcher: switcher,
		State:    NewStore(root),
		Events:   f.events,
		Settings: settings.Defaults().AutoSwitch,
		Now:      func() time.Time { return f.now },
		// No jitter: a test asserting on a delay should not have to allow for
		// it.
		Rand: func() float64 { return 0.5 },
	}
	return f
}

func (f *fixture) advance(d time.Duration) { f.now = f.now.Add(d) }

// seed registers accounts and makes one of them the live login.
func (f *fixture) seed(accounts map[string]string, active string) {
	f.t.Helper()
	roster, err := f.engine.Switcher.RosterOrEmpty()
	if err != nil {
		f.t.Fatal(err)
	}
	for num, email := range accounts {
		roster.Insert(num, &swap.Account{Email: email, UUID: "acct-" + num}, f.now)
		if err := f.engine.Switcher.Creds.WriteAccount(num, email,
			`{"claudeAiOauth":{"accessToken":"tok-`+num+`","refreshToken":"r-`+num+`"}}`); err != nil {
			f.t.Fatal(err)
		}
		if err := f.engine.Switcher.WriteAccountConfig(num, email,
			`{"oauthAccount":{"emailAddress":"`+email+`"}}`); err != nil {
			f.t.Fatal(err)
		}
	}
	if active != "" {
		roster.SetActive(active, f.now)
	}
	if err := f.engine.Switcher.WriteRoster(roster); err != nil {
		f.t.Fatal(err)
	}
	if active != "" {
		f.login(active, accounts[active])
	}
}

func (f *fixture) login(num, email string) {
	f.t.Helper()
	config := `{"oauthAccount":{"emailAddress":"` + email + `","accountUuid":"acct-` + num + `"}}`
	if err := os.WriteFile(f.engine.Switcher.Paths.GlobalConfigPath(), []byte(config), 0o600); err != nil {
		f.t.Fatal(err)
	}
	if err := f.engine.Switcher.Creds.WriteActive(
		`{"claudeAiOauth":{"accessToken":"tok-` + num + `","refreshToken":"r-` + num + `"}}`); err != nil {
		f.t.Fatal(err)
	}
}

// measuring wires the fetcher and clears any stored measurements, so a test's
// numbers take effect immediately.
func (f *fixture) measuring(byNumber map[string]*usage.Result) {
	f.t.Helper()
	f.engine.Switcher.Fetcher = &fakeFetcher{byNumber: byNumber}
	_ = os.Remove(f.engine.Switcher.Usage.Path())
}

// used builds a measurement at a given utilization, with an optional reset.
func used(pct float64, resetIn time.Duration) *usage.Result {
	window := &usage.Window{Pct: pct}
	if resetIn > 0 {
		window.ResetsAt = testNow.Add(resetIn).Format(time.RFC3339)
	}
	return &usage.Result{
		FiveHour: &usage.Window{Pct: pct / 2},
		SevenDay: window,
	}
}

func (f *fixture) tick() Outcome {
	f.t.Helper()
	f.events.events = nil
	return f.engine.Tick(f.t.Context())
}

func (f *fixture) activeSlot() string {
	f.t.Helper()
	roster, err := f.engine.Switcher.RosterOrEmpty()
	if err != nil {
		f.t.Fatal(err)
	}
	num, _ := f.engine.Switcher.CurrentNumber(roster)
	return num
}

// A healthy account is left alone: the engine's default answer is to do
// nothing.
func TestABelowThresholdAccountIsLeftAlone(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"}, "1")
	f.measuring(map[string]*usage.Result{"1": used(50, 0), "2": used(10, 0)})

	if outcome := f.tick(); outcome != NoAction {
		t.Fatalf("outcome = %v, want %v", outcome, NoAction)
	}
	event, ok := f.events.find(KindNoSwitch)
	if !ok || event.Reason != "below-threshold" {
		t.Errorf("events = %v", f.events.reasons())
	}
	if !strings.Contains(event.Detail, "50%") || !strings.Contains(event.Detail, "90%") {
		t.Errorf("detail = %q, want both sides shown", event.Detail)
	}
	if f.activeSlot() != "1" {
		t.Error("a healthy account was switched away from")
	}
}

// Above the threshold, with a clearly better account available, it moves.
func TestAnOverThresholdAccountSwitches(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"}, "1")
	f.measuring(map[string]*usage.Result{"1": used(95, 0), "2": used(10, 0)})

	if outcome := f.tick(); outcome != Switched {
		t.Fatalf("outcome = %v, want %v: %v", outcome, Switched, f.events.reasons())
	}
	event, ok := f.events.find(KindSwitch)
	if !ok {
		t.Fatal("no switch event")
	}
	if event.Trigger != TriggerProactive {
		t.Errorf("trigger = %q, want %q", event.Trigger, TriggerProactive)
	}
	if event.To == nil || *event.To.Number != 2 {
		t.Errorf("to = %+v", event.To)
	}
	if f.activeSlot() != "2" {
		t.Errorf("the live login is on %q", f.activeSlot())
	}
}

// A candidate that would itself re-trigger on the next tick is not a landing
// spot.
func TestAnAlreadyStressedCandidateIsNotATarget(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"}, "1")
	// Both above the threshold, and neither reports a reset — so there is no
	// recovery axis to fall back to either.
	f.measuring(map[string]*usage.Result{"1": used(92, 0), "2": used(95, 0)})

	if outcome := f.tick(); outcome != Blocked {
		t.Fatalf("outcome = %v, want %v: %v", outcome, Blocked, f.events.reasons())
	}
	if f.activeSlot() != "1" {
		t.Error("the engine moved onto an account that would re-trigger")
	}
}

// The margin is what stops two accounts near the line from trading places.
func TestTheHysteresisMarginBlocksAMarginalMove(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"}, "1")
	// Slot 1 is over the threshold; slot 2 is better, but not by the margin.
	f.engine.Settings.HysteresisPct = 15
	f.measuring(map[string]*usage.Result{"1": used(92, 0), "2": used(85, 0)})

	if outcome := f.tick(); outcome != Blocked {
		t.Fatalf("outcome = %v, want %v: %v", outcome, Blocked, f.events.reasons())
	}
	event, _ := f.events.find(KindNoSwitch)
	if !strings.Contains(event.Detail, "15") {
		t.Errorf("detail = %q, want the margin named", event.Detail)
	}

	// Widen the gap past the margin and it moves.
	f.measuring(map[string]*usage.Result{"1": used(92, 0), "2": used(70, 0)})
	if outcome := f.tick(); outcome != Switched {
		t.Fatalf("outcome = %v, want %v: %v", outcome, Switched, f.events.reasons())
	}
}

// An account at its hard limit is an escape, not an optimization: the margins
// and the cooldown do not apply.
func TestAtItsLimitEscapesWithoutTheMargin(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"}, "1")
	f.engine.Settings.HysteresisPct = 50
	f.measuring(map[string]*usage.Result{"1": used(100, 0), "2": used(80, 0)})

	if outcome := f.tick(); outcome != Switched {
		t.Fatalf("outcome = %v, want %v: %v", outcome, Switched, f.events.reasons())
	}
	event, _ := f.events.find(KindSwitch)
	if event.Trigger != TriggerAtLimit {
		t.Errorf("trigger = %q, want %q", event.Trigger, TriggerAtLimit)
	}
}

// A cooldown stops a proactive move from following another too closely.
func TestTheCooldownGatesProactiveMoves(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com", "3": "three@example.com"}, "1")
	f.engine.Settings.CooldownSeconds = 600
	f.measuring(map[string]*usage.Result{"1": used(95, 0), "2": used(10, 0), "3": used(15, 0)})

	if outcome := f.tick(); outcome != Switched {
		t.Fatalf("the first switch: outcome = %v: %v", outcome, f.events.reasons())
	}

	// Immediately after, a proactive move is refused.
	f.advance(time.Minute)
	f.measuring(map[string]*usage.Result{"1": used(10, 0), "2": used(95, 0), "3": used(15, 0)})
	if outcome := f.tick(); outcome != NoAction {
		t.Fatalf("outcome = %v, want %v: %v", outcome, NoAction, f.events.reasons())
	}
	event, _ := f.events.find(KindNoSwitch)
	if event.Reason != "cooldown" {
		t.Errorf("reason = %q, want %q", event.Reason, "cooldown")
	}

	// Past the cooldown it moves again.
	f.advance(11 * time.Minute)
	if outcome := f.tick(); outcome != Switched {
		t.Fatalf("outcome = %v, want %v: %v", outcome, Switched, f.events.reasons())
	}
}

// An account at its limit escapes even inside a cooldown: making the user wait
// there leaves them stuck on a dead account.
func TestTheCooldownDoesNotGateAnEscape(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com", "3": "three@example.com"}, "1")
	f.engine.Settings.CooldownSeconds = 3600
	f.measuring(map[string]*usage.Result{"1": used(95, 0), "2": used(10, 0), "3": used(20, 0)})
	if outcome := f.tick(); outcome != Switched {
		t.Fatalf("the first switch: %v", f.events.reasons())
	}

	f.advance(time.Minute)
	f.measuring(map[string]*usage.Result{"1": used(20, 0), "2": used(100, 0), "3": used(10, 0)})
	if outcome := f.tick(); outcome != Switched {
		t.Fatalf("outcome = %v, want an escape from a spent account: %v", outcome, f.events.reasons())
	}
}

// Unmeasurable usage is not a reason to switch on its own — it takes several
// consecutive ticks, so one bad round trip does not move the user.
func TestFailoverTakesSeveralUnhealthyTicks(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"}, "1")
	f.engine.Settings.UnhealthyTicks = 3
	// Slot 1 is unmeasurable; slot 2 is fine.
	f.measuring(map[string]*usage.Result{"2": used(10, 0)})

	for i := 1; i < 3; i++ {
		if outcome := f.tick(); outcome != NoAction {
			t.Fatalf("tick %d: outcome = %v, want %v: %v", i, outcome, NoAction, f.events.reasons())
		}
		event, _ := f.events.find(KindNoSwitch)
		if event.Reason != "active-usage-unknown" {
			t.Errorf("tick %d: reason = %q", i, event.Reason)
		}
		if !strings.Contains(event.Detail, "/3") {
			t.Errorf("tick %d: detail = %q, want the count", i, event.Detail)
		}
		f.advance(5 * time.Minute)
	}

	if outcome := f.tick(); outcome != Switched {
		t.Fatalf("outcome = %v, want a failover: %v", outcome, f.events.reasons())
	}
	event, _ := f.events.find(KindSwitch)
	if event.Trigger != TriggerFailover {
		t.Errorf("trigger = %q, want %q", event.Trigger, TriggerFailover)
	}
}

// One measurable tick resets the count, so intermittent trouble never
// accumulates into a switch.
func TestAMeasurableTickResetsTheFailoverCount(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"}, "1")
	f.engine.Settings.UnhealthyTicks = 3
	f.measuring(map[string]*usage.Result{"2": used(10, 0)})

	f.tick()
	f.advance(5 * time.Minute)
	f.tick()

	// A good measurement lands.
	f.measuring(map[string]*usage.Result{"1": used(20, 0), "2": used(10, 0)})
	f.advance(5 * time.Minute)
	if outcome := f.tick(); outcome != NoAction {
		t.Fatalf("outcome = %v: %v", outcome, f.events.reasons())
	}

	// The count restarted, so one more bad tick is not enough.
	f.measuring(map[string]*usage.Result{"2": used(10, 0)})
	f.advance(5 * time.Minute)
	if outcome := f.tick(); outcome != NoAction {
		t.Fatalf("outcome = %v, want the count to have restarted: %v", outcome, f.events.reasons())
	}
}

// An expired token normally means an idle editor that will heal itself, so the
// engine holds rather than spending failover ticks.
func TestAnExpiredActiveTokenHoldsBeforeFailingOver(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"}, "1")
	f.engine.Settings.UnhealthyTicks = 2
	// The live credential is expired, which the collector surfaces as a
	// sentinel rather than a measurement.
	if err := f.engine.Switcher.Creds.WriteActive(
		`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":1}}`); err != nil {
		t.Fatal(err)
	}
	f.measuring(map[string]*usage.Result{"2": used(10, 0)})

	if outcome := f.tick(); outcome != NoAction {
		t.Fatalf("outcome = %v: %v", outcome, f.events.reasons())
	}
	event, _ := f.events.find(KindNoSwitch)
	if event.Reason != "active-idle" {
		t.Errorf("reason = %q, want the idle hold", event.Reason)
	}
	if !f.engine.idleHoldSlow {
		t.Error("the tick did not ask to crawl")
	}

	// Held far longer than any idle nap needs, it resumes counting — a DEAD
	// refresh token with an active user looks identical forever otherwise.
	f.advance(IdleHoldMax + time.Minute)
	if outcome := f.tick(); outcome != NoAction {
		t.Fatalf("outcome = %v: %v", outcome, f.events.reasons())
	}
	event, _ = f.events.find(KindNoSwitch)
	if event.Reason != "active-usage-unknown" {
		t.Errorf("reason = %q, want normal counting to have resumed", event.Reason)
	}
}

// A live login aaswap does not manage must never be switched away from: there is
// no backup of it anywhere.
func TestAnUnmanagedLoginIsNeverTouched(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"}, "1")
	f.login("9", "stranger@example.com")
	f.measuring(map[string]*usage.Result{"1": used(95, 0), "2": used(10, 0)})

	if outcome := f.tick(); outcome != NoAction {
		t.Fatalf("outcome = %v, want %v: %v", outcome, NoAction, f.events.reasons())
	}
	event, _ := f.events.find(KindNoSwitch)
	if event.Reason != "unmanaged-active-account" {
		t.Errorf("reason = %q", event.Reason)
	}
	if got := f.engine.Switcher.Creds.ReadActive().Value; !strings.Contains(got, "tok-9") {
		t.Errorf("the unmanaged login was replaced: %q", got)
	}
}

// With nowhere to go, the engine says so with the exit code a wrapper can alert
// on.
func TestNoCandidatesIsBlocked(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com"}, "1")
	f.measuring(map[string]*usage.Result{"1": used(95, 0)})

	if outcome := f.tick(); outcome != Blocked {
		t.Fatalf("outcome = %v, want %v: %v", outcome, Blocked, f.events.reasons())
	}
	event, _ := f.events.find(KindNoSwitch)
	if event.Reason != "no-candidates" {
		t.Errorf("reason = %q", event.Reason)
	}
	// Nothing will change until the user acts, so the next wait is long.
	if !f.engine.blockedWaitLong {
		t.Error("the engine did not ask to wait long")
	}
}

// Everything spent: say when quota returns, and wait for it rather than
// re-polling a fleet that has nothing.
func TestEveryAccountExhaustedReportsTheEarliestReset(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"}, "1")
	f.measuring(map[string]*usage.Result{
		"1": used(100, 3*time.Hour),
		"2": used(100, 90*time.Minute),
	})

	if outcome := f.tick(); outcome != Blocked {
		t.Fatalf("outcome = %v, want %v: %v", outcome, Blocked, f.events.reasons())
	}
	event, ok := f.events.find(KindAllExhausted)
	if !ok {
		t.Fatalf("no exhaustion event: %v", f.events.events)
	}
	// The SOONEST reset across the fleet, not the active account's.
	if !strings.Contains(event.EarliestResetAt, "13:30") {
		t.Errorf("earliestResetAt = %q, want the soonest", event.EarliestResetAt)
	}
	if f.engine.sleepUntil.IsZero() {
		t.Error("the engine did not schedule a wait for the reset")
	}
}

// A dry run decides and reports but changes nothing — not the login, not the
// state.
func TestADryRunChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.engine.DryRun = true
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"}, "1")
	f.measuring(map[string]*usage.Result{"1": used(95, 0), "2": used(10, 0)})

	if outcome := f.tick(); outcome != Switched {
		t.Fatalf("outcome = %v, want the decision reported: %v", outcome, f.events.reasons())
	}
	event, _ := f.events.find(KindSwitch)
	if !event.DryRun {
		t.Error("the switch event was not marked as a dry run")
	}
	if f.activeSlot() != "1" {
		t.Error("a dry run moved the live login")
	}
	if _, err := os.Stat(f.engine.State.Path()); err == nil {
		t.Error("a dry run wrote state")
	}
}

// A dead refresh lineage quarantines the account: only a re-login helps, so
// retrying forever just draws rejections.
func TestADeadCandidateIsQuarantined(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"}, "1")
	// Slot 2's stored credential is expired, and the refresh will be rejected.
	if err := f.engine.Switcher.Creds.WriteAccount("2", "two@example.com",
		`{"claudeAiOauth":{"accessToken":"a","refreshToken":"dead","expiresAt":1}}`); err != nil {
		t.Fatal(err)
	}
	f.engine.Switcher.Refresher = deadRefresher{}
	f.measuring(map[string]*usage.Result{"1": used(95, 0), "2": used(10, 0)})

	if outcome := f.tick(); outcome != Blocked {
		t.Fatalf("outcome = %v, want %v: %v", outcome, Blocked, f.events.reasons())
	}
	event, ok := f.events.find(KindQuarantine)
	if !ok || event.Number != "2" {
		t.Fatalf("no quarantine event: %v", f.events.events)
	}
	if event.Reason != "invalid_grant" {
		t.Errorf("reason = %q", event.Reason)
	}

	// A quarantined account is not offered again.
	f.advance(time.Hour)
	if outcome := f.tick(); outcome != Blocked {
		t.Fatalf("outcome = %v: %v", outcome, f.events.reasons())
	}
	if _, requarantined := f.events.find(KindQuarantine); requarantined {
		t.Error("a quarantined account was tried again")
	}
}

// Replacing the credential releases the quarantine on its own, with no command
// to remember.
func TestReplacingACredentialReleasesTheQuarantine(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"}, "1")
	if err := f.engine.Switcher.Creds.WriteAccount("2", "two@example.com",
		`{"claudeAiOauth":{"accessToken":"a","refreshToken":"dead","expiresAt":1}}`); err != nil {
		t.Fatal(err)
	}
	f.engine.Switcher.Refresher = deadRefresher{}
	f.measuring(map[string]*usage.Result{"1": used(95, 0), "2": used(10, 0)})
	f.tick()

	// The user logs in again and re-captures the account.
	if err := f.engine.Switcher.Creds.WriteAccount("2", "two@example.com",
		`{"claudeAiOauth":{"accessToken":"fresh","refreshToken":"r-new"}}`); err != nil {
		t.Fatal(err)
	}
	f.engine.Switcher.Refresher = nil
	f.advance(time.Hour)

	if outcome := f.tick(); outcome != Switched {
		t.Fatalf("outcome = %v, want the released account to be usable: %v",
			outcome, f.events.reasons())
	}
	event, ok := f.events.find(KindUnquarantine)
	if !ok || event.Reason != "credentials-replaced" {
		t.Errorf("unquarantine = %+v", event)
	}
}

// deadRefresher rejects every grant, as the server does for a dead lineage.
type deadRefresher struct{}

func (deadRefresher) Refresh(context.Context, string, time.Time) claudeapi.RefreshOutcome {
	return claudeapi.RefreshOutcome{Error: claudeapi.KindInvalidGrant}
}

// The poll event carries the per-window breakdown, because a binding
// percentage alone hides which window binds.
func TestThePollEventNamesTheBindingWindow(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com"}, "1")
	f.measuring(map[string]*usage.Result{"1": {
		FiveHour: &usage.Window{Pct: 10},
		SevenDay: &usage.Window{Pct: 89},
		Scoped:   []usage.Scoped{{Name: "Fable", Pct: 95}},
	}})
	f.engine.Settings.Model = "Fable"

	f.tick()
	event, ok := f.events.find(KindPoll)
	if !ok {
		t.Fatal("no poll event")
	}
	windows := event.WindowsPct["1"]
	for label, want := range map[string]float64{"5h": 10, "7d": 89, "Fable": 95} {
		if windows[label] != want {
			t.Errorf("windowsPct[%q] = %v, want %v", label, windows[label], want)
		}
	}
	// And the human line names them, in a stable order.
	human := event.Human()
	if !strings.Contains(human, "5h 10%") || !strings.Contains(human, "Fable 95%") {
		t.Errorf("human = %q", human)
	}
}

// An unmeasurable account reports as null rather than zero: unknown is not
// exhausted, and a consumer that confused them would skip a working account.
func TestUnknownHeadroomIsNullNotZero(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com", "2": "two@example.com"}, "1")
	f.measuring(map[string]*usage.Result{"1": used(50, 0)})

	f.tick()
	event, _ := f.events.find(KindPoll)
	if event.HeadroomPct["2"] != nil {
		t.Errorf("headroomPct[2] = %v, want null", *event.HeadroomPct["2"])
	}
	if event.FetchErrors["2"] == "" {
		t.Errorf("fetchErrors = %v, want a reason for the unknown", event.FetchErrors)
	}

	// And it round-trips as null through the JSON stream.
	line, err := event.JSONLine()
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(line, &decoded); err != nil {
		t.Fatal(err)
	}
	headroom := decoded["headroomPct"].(map[string]any)
	value, present := headroom["2"]
	if !present || value != nil {
		t.Errorf("headroomPct.2 = (%v, present=%v), want an explicit null", value, present)
	}
}

// A configured model no account reports is inert. Not an error, but not silent
// either: the user set it expecting it to matter.
func TestAnInertModelNameWarnsOnce(t *testing.T) {
	f := newFixture(t)
	f.seed(map[string]string{"1": "one@example.com"}, "1")
	f.engine.Settings.Model = "Nonexistent"
	f.measuring(map[string]*usage.Result{"1": used(50, 0)})

	f.tick()
	event, ok := f.events.find(KindConfigWarning)
	if !ok || !strings.Contains(event.Message, "Nonexistent") {
		t.Fatalf("no warning naming the model: %v", f.events.events)
	}

	// Once, not every tick.
	f.advance(time.Hour)
	f.tick()
	if _, repeated := f.events.find(KindConfigWarning); repeated {
		t.Error("the inert-model warning repeated")
	}
}

// The outcomes double as exit codes, so a cron wrapper can branch on them.
func TestOutcomeCodes(t *testing.T) {
	for outcome, want := range map[Outcome]int{
		Switched: 0, Failed: 1, NoAction: 2, Blocked: 3,
	} {
		if int(outcome) != want {
			t.Errorf("%v = %d, want %d", outcome, int(outcome), want)
		}
	}
}
