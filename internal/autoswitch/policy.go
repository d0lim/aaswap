// Package autoswitch moves the live login off an account that is running out
// of rate-limit headroom, and onto one that has some.
//
// # The shape of a tick
//
// Poll the accounts that are due, look at where the user is, and decide. Most
// ticks decide to do nothing, which is the point: switching costs a Keychain
// write and a Claude Code restart's worth of confusion, so the engine only
// moves when it can show the move is an improvement.
//
// # Every threshold here is an anti-flap margin
//
// Two accounts hovering near a limit will trade places forever if a move needs
// only to be a strict improvement — one point moves the engine, the target
// burns it back, and it ping-pongs. So every comparison carries a margin, and
// each margin is in the unit of the axis it guards: percentage points where
// headroom decides, seconds where a recovery time does, and a RATIO where a
// point is meaningless because both accounts are nearly spent.
package autoswitch

import (
	"time"

	"github.com/realiti4/claude-swap/internal/claudeapi"
	"github.com/realiti4/claude-swap/internal/pollpolicy"
)

// StateFileName holds the engine's memory between runs.
const StateFileName = "autoswitch_state.json"

// StateSchemaVersion is the state file's format.
const StateSchemaVersion = 1

// FreshenBuffer is how close to expiry a candidate's token may be before the
// engine refreshes it ahead of activating.
//
// Twice Claude Code's own refresh buffer, so its post-lock "abort if not
// expired" re-read still holds with margin after the swap lands.
const FreshenBuffer = 10 * time.Minute

// MaxSleep caps a wait around a known quota reset.
//
// Rechecked at the exhausted-account cadence rather than slept through: a
// provider can grant quota before the reset it advertised, and a long sleep
// would suppress the very fetch that discovers it.
var MaxSleep = pollpolicy.ExhaustedInterval

// NoResetFallback is how long to wait when there is nothing to wait FOR — no
// candidates, or no reset time known.
const NoResetFallback = 5 * time.Minute

// IdleHoldMax bounds how long the engine will treat an expired active token as
// "Claude Code is idle and will self-heal".
//
// Measured as elapsed time rather than ticks, because the hold itself slows the
// cadence. An owned-and-expired token normally does mean an idle editor — but a
// DEAD refresh token with an active user looks identical forever, so after this
// long the engine resumes counting toward failover.
const IdleHoldMax = 30 * time.Minute

// RecoveryHysteresis is the anti-flap margin when ranking by recovery time.
//
// Five minutes is comfortably longer than one poll cycle, so two accounts whose
// windows roll over close together cannot trade places on measurement jitter.
// The percentage-point margin is unmeetable in this state by construction —
// everything is within a few points of its limit — which is why this axis needs
// its own unit.
const RecoveryHysteresis = 5 * time.Minute

// RecoveryHorizon is how far out a reset can be and still be worth real
// headroom.
//
// The recovery ranking was measured on minutes-scale resets; a days-scale one
// is the opposite trade, because neither account returns within the session.
// Four hours keeps most of a five-hour cycle on the recovery ranking while
// falling back to headroom for anything further out — the conservative side of
// that boundary, because ranking by headroom an hour early costs at most one
// extra move, while a wider horizon would rank by a reset the session never
// sees.
const RecoveryHorizon = 4 * time.Hour

// HorizonHeadroomRatio is the anti-flap margin on the headroom axis, as a ratio
// rather than points.
//
// Strictly-more is no margin at all. A ratio makes the move one-way: a target
// that burns back down to half of what it beat can qualify in reverse, which
// takes a real collapse rather than the one point a strict comparison needs.
const HorizonHeadroomRatio = 2.0

// SpentHeadroom is where headroom comparisons stop meaning anything.
//
// Below this, a percentage point is under ten minutes of work — less than two
// poll intervals — so comparing two spent accounts by headroom compares noise.
// When every candidate is down here the engine ranks by recovery instead: sit
// where quota returns first, however far out, rather than parking on whichever
// account happens to be held.
const SpentHeadroom = 3.0

// systemicRefusals are freshen failures that every candidate hits identically
// and that keep happening until something outside this process changes.
//
// The ORDER is the precedence order, most actionable first, because reporting
// the wrong one is how a cause that needs a human hides behind one that clears
// itself. The first three stay until somebody unsets a variable, fixes a client
// registration, or unlocks a Keychain; the last is gone by the next pass.
var systemicRefusals = []struct {
	Kind    claudeapi.ErrorKind
	Message string
}{
	{claudeapi.KindStoreUnmirrored,
		"CLAUDE_SECURESTORAGE_CONFIG_DIR is set — unset it or run cswap from a normal shell"},
	{claudeapi.KindInvalidClient,
		"cswap's OAuth client was rejected — systemic, not this account"},
	{claudeapi.KindStashUnreadable,
		"a stashed successor is unreadable — unlock the keychain or fix the file, then " +
			"retry; `cswap unclaimed` inspects it"},
	{claudeapi.KindConsumeBusy,
		"another cswap surface holds the slot — retries next pass"},
}

// systemicMessage explains a systemic refusal, reporting false for anything
// else.
func systemicMessage(kind claudeapi.ErrorKind) (string, bool) {
	for _, refusal := range systemicRefusals {
		if refusal.Kind == kind {
			return refusal.Message, true
		}
	}
	return "", false
}

// mostActionable picks the refusal a tick should report when several candidates
// failed for different systemic reasons.
func mostActionable(kinds map[claudeapi.ErrorKind]bool) (claudeapi.ErrorKind, string, bool) {
	for _, refusal := range systemicRefusals {
		if kinds[refusal.Kind] {
			return refusal.Kind, refusal.Message, true
		}
	}
	return "", "", false
}

// recoveryIsUseful decides which axis a candidate is judged on.
//
// Ranking by recovery time only helps when the recovery is close enough to
// matter within a session AND there is no real headroom to prefer instead.
// Deciding this per candidate, in one place, is deliberate: deciding it once
// and globally is how the same question got answered four different ways at
// four scattered gates.
func recoveryIsUseful(candidateRecovery, activeRecovery, activeHeadroom, bestCandidateHeadroom float64, now float64) bool {
	// Nothing worth having anywhere: rank by who comes back first, however far
	// out that is.
	if activeHeadroom <= SpentHeadroom && bestCandidateHeadroom <= SpentHeadroom {
		return true
	}
	// A recovery nobody will see within the session is not a reason to move.
	if candidateRecovery <= 0 || candidateRecovery-now > RecoveryHorizon.Seconds() {
		return false
	}
	// The active account comes back first anyway.
	if activeRecovery > 0 && activeRecovery <= candidateRecovery {
		return false
	}
	return true
}
