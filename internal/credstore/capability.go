package credstore

import (
	"errors"
	"sync"
	"time"

	"github.com/realiti4/claude-swap/internal/keychain"
	"github.com/realiti4/claude-swap/internal/platform"
)

// RecheckCooldown is how long after a Keychain failure the store waits before
// re-probing.
//
// After a failure the store drops to file mode so a single CLI invocation
// cannot split-brain between backends. A long-running daemon — the menu bar or
// the TUI — instead re-probes once this elapses: far longer than any CLI
// command runs, so a sub-second command never re-probes and the guarantee
// holds, yet short enough that a transient security(1) timeout self-heals
// within a minute instead of disabling the Keychain for the whole process.
const RecheckCooldown = 60 * time.Second

// capability tracks whether the macOS Keychain may be used, and — separately —
// what has actually been observed about it.
//
// # Why six pieces of state and not one flag
//
// Two different kinds of fact live here, and collapsing them has caused real
// bugs more than once:
//
//   - A *routing decision*: which backend should operations use? This is
//     deliberately sticky and is overwritten on purpose (see pinFileMode).
//   - An *observation*: did a Keychain operation actually fail? An observation
//     cannot be undone by a later choice, and a choice must not masquerade as
//     one.
//
// The distinction matters because an empty read means opposite things in the
// two worlds. If the Keychain simply could not be asked, an empty result proves
// nothing and the user must not be told to re-add an account whose backup is
// alive but unread. If ccswap deliberately wrote the credential to the file,
// nothing failed, the file is the authority, and an empty read means the slot
// really is empty.
type capability struct {
	platform platform.Platform
	now      func() time.Time

	// mu guards every field below. The Python original leaned on the GIL for
	// this; Go has no such umbrella, and the TUI drives three workers against
	// one store while the auto-switch engine polls concurrently.
	mu sync.Mutex

	// usable is where operations should be ROUTED. nil means "not yet probed";
	// once an operation has run it is true or false. It is a decision, not an
	// observation — pinFileMode sets it deliberately.
	usable *bool

	// disabledUntil is when to re-probe after a real failure. The zero time
	// means no re-probe is pending: either nothing ever failed, or file mode
	// was pinned deliberately.
	disabledUntil time.Time

	// fileModeIsOurs records that file mode was CHOSEN (a write fell back and
	// the Keychain item was deleted) rather than forced by a failed read. Both
	// stick, but only a failure means the file may be behind Claude Code's own
	// writes.
	fileModeIsOurs bool

	// opFailed records that a Keychain operation FAILED and has not since
	// succeeded. Distinct from usable, which is where operations are routed:
	// a routing choice must not erase an observation.
	opFailed bool

	// activeReadFailed records that the ACTIVE OAuth read could not reach the
	// Keychain. Per-item, not per-process: a successful backup read for an idle
	// slot clears opFailed, and that once erased the verdict recorded when the
	// active read failed — flipping degraded to false while the plaintext file
	// still held the superseded generation. "The Keychain answers" and "this
	// active read succeeded" are different facts.
	activeReadFailed bool

	// residualVerdict is what pinFileMode OBSERVED about the residual active
	// Keychain item. nil: never pinned. true: the delete returned, so nothing
	// can shadow the file. false: the delete could not run, so the file may be
	// the superseded generation. A fact about THIS item, which is why neither
	// flag above can stand in for it.
	residualVerdict *bool
}

func newCapability(p platform.Platform) *capability {
	return &capability{platform: p, now: time.Now}
}

// observe runs a Keychain operation, learning usability from the outcome.
//
// A success — including a lookup that reports "no such item" — marks the
// Keychain usable, but only flips the routing decision from "unprobed" to
// "usable", never from "unusable" back to "usable". Once a call has failed this
// run, operations stay in file mode so one invocation cannot split-brain
// between backends.
//
// A failure marks the Keychain unusable, arms the re-probe cooldown, and
// returns the error so the caller can fall back.
//
// Do NOT route an existence check through here: it reports false for both
// "absent" and "failed", so a timeout would be misread as a usable Keychain.
func (c *capability) observe[T any](fn func() (T, error)) (T, error) {
	// The operation itself runs UNLOCKED: it spawns security(1), which takes
	// 10-50ms, and holding the lock across it would serialize every concurrent
	// reader behind the slowest Keychain call. Only the bookkeeping is guarded.
	value, err := fn()

	c.mu.Lock()
	defer c.mu.Unlock()

	if err != nil && errors.Is(err, keychain.ErrUnavailable) {
		// Recorded separately from the routing decision because that decision
		// is one others overwrite — pinFileMode clears it deliberately — while
		// this is an observation, and an observation cannot be undone by a
		// later choice.
		c.opFailed = true
		c.setUsable(false)
		c.disabledUntil = c.now().Add(RecheckCooldown)
		var zero T
		return zero, err
	}
	if err != nil {
		// Not a Keychain-availability failure: a programming error, which must
		// stay loud rather than silently routing to the file backend.
		var zero T
		return zero, err
	}

	// A SUCCESS is an observation too, and it is the newer one.
	//
	// Recording only failures made opFailed monotone, and the cooldown
	// re-probe that normally masks a stale one is zeroed by pinFileMode — so
	// one transient timeout became permanent for the process the moment any
	// later write fell back. Measured: a read times out, the Keychain recovers
	// and the cooldown lapses, then a write pins and the verdict is stuck
	// forever, with degraded=true, "keychain unavailable" on every usage pass,
	// and `ccswap add` refused.
	//
	// Cleared unconditionally rather than only when the decision was unprobed:
	// the decision is about which backend to use and is deliberately sticky,
	// while this is a fact about whether the Keychain answers. A call that just
	// returned is proof that it does.
	c.opFailed = false
	if c.usable == nil {
		c.setUsable(true)
	}
	return value, nil
}

func (c *capability) setUsable(v bool) { c.usable = &v }

// useKeychain reports whether credential operations should target the macOS
// Keychain right now.
//
// Always false off macOS. On macOS, true until an operation fails, which drops
// to file mode. That failure arms a re-probe deadline: within one CLI
// invocation the deadline never passes, so a command cannot split-brain between
// backends, but a long-running daemon re-probes once the cooldown elapses so a
// transient timeout self-heals instead of sticking for the whole process.
//
// A pinned file mode has no deadline and so stays sticky — see pinFileMode for
// why a write fallback must never re-probe.
func (c *capability) useKeychain() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.useKeychainLocked()
}

func (c *capability) useKeychainLocked() bool {
	if c.platform != platform.MacOS {
		return false
	}
	if c.usable != nil && !*c.usable && !c.disabledUntil.IsZero() && !c.now().Before(c.disabledUntil) {
		c.usable = nil // cooldown elapsed; re-probe
		c.disabledUntil = time.Time{}
	}
	return c.usable == nil || *c.usable
}

// pinFileMode pins file mode for the rest of the process, with no Keychain
// re-probe.
//
// A read timeout is safe to recover from, but an active-credential *write* that
// falls back to the file is not: its best-effort delete of the old Keychain item
// may have failed, leaving a stale entry. Re-probing later could read that
// residual and show the wrong account, so once a write falls back the store
// never re-probes onto a Keychain it could not verify-clear. Any re-probe
// deadline a prior read armed is cleared, since it could otherwise still be
// pending.
//
// residualCleared is the caller's OBSERVATION of that delete. True: nothing can
// shadow the file, so it genuinely is the authority, and the two failure flags
// are settled — whatever failed before cannot bear on a file nothing can
// shadow. False: a residual may survive, and that stays true however many
// unrelated Keychain items answer afterwards.
//
// It is a required argument with no default, because every call site has just
// run the delete, and a default is how an observation gets dropped in favour of
// a flag.
func (c *capability) pinFileMode(residualCleared bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.setUsable(false)
	c.disabledUntil = time.Time{}
	c.fileModeIsOurs = true
	c.residualVerdict = &residualCleared

	if residualCleared {
		// Settle the PAST, not the future. Later failures are the flags'
		// question again: observe re-arms the cooldown on any failure with no
		// pin check, and backup reads reach it without consulting useKeychain,
		// so the active read IS reachable after a pin. Measured: a stored true
		// made a genuine later failure read degraded=false and disarmed the
		// capture guard.
		c.opFailed = false
		c.activeReadFailed = false
	}
}

// unreadable reports that the Keychain cannot be asked, so an empty read proves
// nothing.
//
// True only when an operation FAILED. A file mode that pinFileMode chose is
// excluded: nothing failed there, the credential was written to the file
// deliberately, and that file is the authority — an empty read means the slot
// really is empty.
//
// One predicate, because the two facts have been conflated once per site that
// spelled them out separately. Every caller that means "unreadable" asks here
// rather than reading the routing decision raw. The platform check belongs here
// for the same reason: off macOS there is no Keychain to be unreadable.
//
// It asks through useKeychain rather than the raw decision, because the
// cooldown re-probe lives there and the backup read path never calls it.
// Reading the flag raw made a single transient failure permanent for the
// process on exactly the paths that matter — a genuinely empty slot kept
// reporting "unreadable, do not re-add", a dead end, and the consume gate kept
// deferring long after the Keychain answered again. A pinned file mode is
// unaffected: its deadline is zero, so useKeychain never re-probes it.
func (c *capability) unreadable() bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.platform != platform.MacOS {
		return false
	}
	if c.useKeychainLocked() { // may clear a lapsed cooldown
		return false
	}
	// The question is whether an operation failed and has not since succeeded.
	//
	// This used to also accept "file mode was not ours", which on macOS
	// inverts the fact: the pin is reachable ONLY THROUGH a Keychain operation
	// that just failed, since both pin sites sit in the fallback branch of a
	// write that errored. Measured, identical world, one fallback write apart:
	//
	//	state A   degraded=true    sentinel "keychain unavailable"
	//	state B   degraded=false   sentinel "no credentials"
	//
	// degraded=false disarms the capture guard, and the sentinel sends the user
	// to re-add a slot whose backup is alive but unread. The pin's own
	// best-effort delete failed too, so a residual survives and Claude Code
	// reads Keychain-first: the file is the superseded generation, POSTed with
	// the guard off.
	return c.opFailed
}

// setActiveReadFailed records the verdict of the ACTIVE OAuth read.
//
// Held separately from opFailed because a successful read of some *other* item
// — a backup for an idle slot — must not speak for this one.
func (c *capability) setActiveReadFailed(failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.activeReadFailed = failed
}

// activeReadDidFail reports the last active OAuth read's own verdict.
func (c *capability) activeReadDidFail() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.activeReadFailed
}

// residualUnverified reports that file mode was pinned without being able to
// confirm the residual active Keychain item was gone.
//
// It stands on its own: no later success on some other item speaks for the item
// that may still shadow the file.
func (c *capability) residualUnverified() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.residualVerdict != nil && !*c.residualVerdict
}
