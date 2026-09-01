package credstore

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/d0lim/ccswap/internal/keychain"
	"github.com/d0lim/ccswap/internal/platform"
)

// fakeClock lets a test walk the re-probe cooldown without sleeping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time          { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestCapability(t *testing.T, p platform.Platform) (*capability, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	c := newCapability(p)
	c.now = clock.Now
	return c, clock
}

// unavailable is what a real Keychain failure looks like to observe.
var errUnavailable = fmt.Errorf("locked: %w", keychain.ErrUnavailable)

func succeed(c *capability, value string) (string, error) {
	return c.observe(func() (string, error) { return value, nil })
}

func fail(c *capability) (string, error) {
	return c.observe(func() (string, error) { return "", errUnavailable })
}

// ---------------------------------------------------------------- Platform

// Off macOS there is no Keychain at all, so there is nothing to be unreadable
// and nothing to route to.
func TestOffMacOSThereIsNoKeychain(t *testing.T) {
	for _, p := range []platform.Platform{platform.Linux, platform.WSL, platform.Windows, platform.Unknown} {
		t.Run(p.String(), func(t *testing.T) {
			c, _ := newTestCapability(t, p)
			if c.useKeychain() {
				t.Error("useKeychain = true off macOS")
			}
			if c.unreadable() {
				t.Error("unreadable = true off macOS; there is no Keychain to be unreadable")
			}
		})
	}
}

func TestUnprobedMacOSUsesTheKeychain(t *testing.T) {
	c, _ := newTestCapability(t, platform.MacOS)
	if !c.useKeychain() {
		t.Error("useKeychain = false before anything has been probed")
	}
	if c.unreadable() {
		t.Error("unreadable = true before any operation has failed")
	}
}

// ---------------------------------------------------------------- observe

func TestObserveRecordsSuccess(t *testing.T) {
	c, _ := newTestCapability(t, platform.MacOS)

	got, err := succeed(c, "value")
	if err != nil || got != "value" {
		t.Fatalf("observe = (%q, %v), want the value through", got, err)
	}
	if c.usable == nil || !*c.usable {
		t.Error("a success did not mark the Keychain usable")
	}
	if c.opFailed {
		t.Error("opFailed set after a success")
	}
}

// A lookup that reports "no such item" is a success: the Keychain answered.
func TestObserveTreatsAMissAsASuccess(t *testing.T) {
	c, _ := newTestCapability(t, platform.MacOS)
	if _, err := succeed(c, ""); err != nil {
		t.Fatal(err)
	}
	if c.unreadable() {
		t.Error("a miss was recorded as an unreadable Keychain")
	}
}

func TestObserveRecordsFailure(t *testing.T) {
	c, clock := newTestCapability(t, platform.MacOS)

	if _, err := fail(c); !errors.Is(err, keychain.ErrUnavailable) {
		t.Fatalf("observe = %v, want the error returned so the caller can fall back", err)
	}
	if c.useKeychain() {
		t.Error("useKeychain = true after a failure; operations must drop to file mode")
	}
	if !c.unreadable() {
		t.Error("unreadable = false after a failure")
	}
	if want := clock.t.Add(RecheckCooldown); !c.disabledUntil.Equal(want) {
		t.Errorf("re-probe deadline = %v, want %v", c.disabledUntil, want)
	}
}

// A programming error must stay loud rather than silently routing to the file
// backend mid-invocation.
func TestObserveDoesNotRouteOnANonAvailabilityError(t *testing.T) {
	c, _ := newTestCapability(t, platform.MacOS)
	bug := errors.New("nil map write")

	if _, err := c.observe(func() (string, error) { return "", bug }); !errors.Is(err, bug) {
		t.Fatalf("observe = %v, want the error through", err)
	}
	if !c.useKeychain() {
		t.Error("a non-availability error flipped routing to file mode")
	}
	if c.unreadable() {
		t.Error("a non-availability error was recorded as a Keychain failure")
	}
}

// Within one invocation the routing decision is sticky: a later success clears
// the *observation* but must not send operations back to the Keychain, or a
// single command could split-brain between backends.
func TestASuccessClearsTheObservationButNotTheRouting(t *testing.T) {
	c, _ := newTestCapability(t, platform.MacOS)
	if _, err := fail(c); err == nil {
		t.Fatal("setup: expected a failure")
	}

	if _, err := succeed(c, "value"); err != nil {
		t.Fatal(err)
	}
	if c.opFailed {
		t.Error("opFailed survived a later success; a call that just returned proves the Keychain answers")
	}
	if c.usable == nil || *c.usable {
		t.Error("a success flipped routing back to the Keychain inside one invocation")
	}
}

// Recording only failures made the observation monotone, so one transient
// timeout became permanent for the process the moment any later write fell
// back.
func TestAStaleFailureDoesNotCondemnAWorkingRead(t *testing.T) {
	c, _ := newTestCapability(t, platform.MacOS)
	if _, err := fail(c); err == nil {
		t.Fatal("setup: expected a failure")
	}
	if _, err := succeed(c, "value"); err != nil {
		t.Fatal(err)
	}
	if c.unreadable() {
		t.Error("unreadable = true after the Keychain answered again")
	}
}

// ---------------------------------------------------------------- Cooldown

// A CLI command runs well inside the cooldown, so it can never re-probe — which
// is what keeps one invocation on one backend.
func TestCooldownHoldsWithinOneInvocation(t *testing.T) {
	c, clock := newTestCapability(t, platform.MacOS)
	if _, err := fail(c); err == nil {
		t.Fatal("setup: expected a failure")
	}

	clock.Advance(RecheckCooldown - time.Second)
	if c.useKeychain() {
		t.Error("re-probed before the cooldown elapsed")
	}
	if !c.unreadable() {
		t.Error("unreadable went false before the cooldown elapsed")
	}
}

// A long-running daemon re-probes once the cooldown elapses, so a transient
// security(1) timeout self-heals instead of disabling the Keychain for the
// whole process.
func TestALapsedCooldownReProbes(t *testing.T) {
	c, clock := newTestCapability(t, platform.MacOS)
	if _, err := fail(c); err == nil {
		t.Fatal("setup: expected a failure")
	}

	clock.Advance(RecheckCooldown)
	if !c.useKeychain() {
		t.Error("did not re-probe after the cooldown elapsed")
	}
	if !c.disabledUntil.IsZero() {
		t.Error("the re-probe deadline was not cleared")
	}
}

// A genuinely empty slot kept reporting "unreadable, do not re-add" — a dead
// end — because the verdict was read raw instead of through the re-probe.
func TestALapsedCooldownClearsTheUnreadableVerdict(t *testing.T) {
	c, clock := newTestCapability(t, platform.MacOS)
	if _, err := fail(c); err == nil {
		t.Fatal("setup: expected a failure")
	}
	if !c.unreadable() {
		t.Fatal("setup: expected unreadable")
	}

	clock.Advance(RecheckCooldown)
	if c.unreadable() {
		t.Error("unreadable = true after the cooldown lapsed and the Keychain is askable again")
	}
}

// ---------------------------------------------------------------- pinFileMode

// A write that falls back must never re-probe: its best-effort delete of the old
// Keychain item may have failed, and re-probing could then read that residual
// and show the wrong account.
func TestPinFileModeStopsReProbing(t *testing.T) {
	c, clock := newTestCapability(t, platform.MacOS)
	if _, err := fail(c); err == nil {
		t.Fatal("setup: expected a failure")
	}

	c.pinFileMode(false)
	if !c.disabledUntil.IsZero() {
		t.Error("pinning left a re-probe deadline armed")
	}
	if !c.fileModeIsOurs {
		t.Error("fileModeIsOurs was not set")
	}

	clock.Advance(10 * RecheckCooldown)
	if c.useKeychain() {
		t.Error("a pinned file mode re-probed after the old cooldown would have lapsed")
	}
}

// A verified clear settles the past: nothing can shadow the file, so whatever
// failed before cannot bear on it.
func TestAVerifiedClearEndsTheDegradedVerdict(t *testing.T) {
	c, _ := newTestCapability(t, platform.MacOS)
	if _, err := fail(c); err == nil {
		t.Fatal("setup: expected a failure")
	}
	c.activeReadFailed = true

	c.pinFileMode(true)
	if c.opFailed {
		t.Error("opFailed survived a verified clear")
	}
	if c.activeReadFailed {
		t.Error("activeReadFailed survived a verified clear")
	}
	if c.unreadable() {
		t.Error("unreadable = true after a verified clear; the file is the authority")
	}
}

// A verified clear settles the PAST, not the future: a genuine later failure
// must read as degraded again, or the capture guard is disarmed.
func TestAVerifiedClearDoesNotMaskALaterFailure(t *testing.T) {
	c, _ := newTestCapability(t, platform.MacOS)
	c.pinFileMode(true)

	if _, err := fail(c); err == nil {
		t.Fatal("expected a failure")
	}
	if !c.unreadable() {
		t.Error("a failure after a verified clear did not register")
	}
}

// An unverified clear says so on its own: no later success on some *other* item
// speaks for the residual on THIS one.
func TestAFailedClearSurvivesAnUnrelatedSuccess(t *testing.T) {
	c, _ := newTestCapability(t, platform.MacOS)
	if _, err := fail(c); err == nil {
		t.Fatal("setup: expected a failure")
	}
	c.pinFileMode(false)

	// A backup read for an idle slot answers fine — it says nothing about the
	// residual active item.
	if _, err := succeed(c, "backup"); err != nil {
		t.Fatal(err)
	}
	if c.residualVerdict == nil || *c.residualVerdict {
		t.Error("an unrelated success upgraded the residual verdict")
	}
}

// A pin is only reachable through an operation that just failed, so the flags
// it does not clear must still say the Keychain failed.
func TestAnUnverifiedPinStillReportsUnreadable(t *testing.T) {
	c, _ := newTestCapability(t, platform.MacOS)
	if _, err := fail(c); err == nil {
		t.Fatal("setup: expected a failure")
	}

	c.pinFileMode(false)
	if !c.unreadable() {
		t.Error("unreadable = false after an unverified pin; the residual may shadow the file")
	}
}

// ---------------------------------------------------------------- activeReadFailed

// "The Keychain answers" and "this active read succeeded" are different facts.
// A successful backup read for an idle slot once erased the verdict recorded
// when the active read failed, flipping degraded to false while the plaintext
// file still held the superseded generation.
func TestAnIdleBackupReadDoesNotEraseTheActiveVerdict(t *testing.T) {
	c, _ := newTestCapability(t, platform.MacOS)
	c.activeReadFailed = true

	if _, err := succeed(c, "backup-for-another-slot"); err != nil {
		t.Fatal(err)
	}
	if !c.activeReadFailed {
		t.Error("a backup read cleared the active read's own verdict")
	}
}

// The active verdict survives a pin only when the pin verified its clear.
func TestTheActiveVerdictSurvivesAPinOnlyWhenVerified(t *testing.T) {
	tests := []struct {
		name            string
		residualCleared bool
		wantCleared     bool
	}{
		{"verified clear settles it", true, true},
		{"unverified clear leaves it standing", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newTestCapability(t, platform.MacOS)
			c.activeReadFailed = true

			c.pinFileMode(tt.residualCleared)
			if got := !c.activeReadFailed; got != tt.wantCleared {
				t.Errorf("activeReadFailed cleared = %v, want %v", got, tt.wantCleared)
			}
		})
	}
}

// ---------------------------------------------------------------- Constants

func TestRecheckCooldownIsLongerThanAnyCLICommand(t *testing.T) {
	// The guarantee rests on this: a sub-second command must never reach the
	// deadline, so it cannot split-brain between backends.
	if RecheckCooldown < 30*time.Second {
		t.Errorf("RecheckCooldown = %v, too short to outlast a CLI invocation", RecheckCooldown)
	}
	if RecheckCooldown > 5*time.Minute {
		t.Errorf("RecheckCooldown = %v, too long for a daemon to self-heal", RecheckCooldown)
	}
}
