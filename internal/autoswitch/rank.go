package autoswitch

import (
	"math"
	"slices"
	"time"

	"github.com/realiti4/claude-swap/internal/swap"
	"github.com/realiti4/claude-swap/internal/usagestore"
)

// rankInput is everything the ranking needs.
//
// Pure: ranking emits nothing and writes nothing, so it can be run twice in a
// tick — once with the no-return bar and once without.
type rankInput struct {
	Trigger      string
	ConsumeFirst bool
	Candidates   []string
	Snapshot     *swap.Snapshot
	Headroom     map[string]*float64
	Current      string
	// ActiveHeadroom is nil when the active account's usage cannot be read.
	ActiveHeadroom *float64
	Threshold      float64
	HysteresisPct  float64
	Models         []string
	Now            time.Time

	// NoReturn is the account the last switch came FROM, while it is still
	// barred.
	NoReturn string
	// Recovered reports whether that account is genuinely a better proposition
	// than when it was left. Computed once per snapshot and shared with the
	// leaves-nothing retry, so the two cannot disagree.
	Recovered bool
}

// rankCandidates filters and orders the candidates for one tick.
//
// anyKnown reports whether any candidate's usage could be read at all, which is
// what separates "nothing is good enough" from "nothing is measurable" — two
// blocks with different remedies.
func rankCandidates(in rankInput) (ordered []string, anyKnown bool) {
	nowSeconds := float64(in.Now.UnixMilli()) / 1000
	activeHeadroom := 0.0
	if in.ActiveHeadroom != nil {
		activeHeadroom = *in.ActiveHeadroom
	}

	// consume-first ranks by soonest weekly reset, so a proactive move needs
	// the active account's own reset to compare against.
	activeReset := math.Inf(1)
	if in.ConsumeFirst {
		activeReset = weeklyReset(in.Snapshot.Entries[in.Current], in.Now)
	}

	// When NOTHING is below the threshold — the active account and every
	// candidate all nearly spent — "land somewhere healthy" has no answer, and
	// holding out for one costs the user the session. Sitting still means
	// burning to a hard limit while a peer that resets in eight minutes is never
	// tried. So the goal changes from "most headroom" to "soonest back".
	//
	// Deliberately narrow: one healthy peer still wins the normal way, and the
	// recovery margin replaces the percentage-point one, which is unmeetable
	// when everything is within a few points of its limit.
	allAbove := everyAccountAboveThreshold(in)

	// "Is anything worth having?" — the most headroom any candidate with a
	// READABLE row offers. Unknown rows are skipped rather than counted as
	// zero: a row that could not be read is no evidence of an empty account,
	// and one sentinel row counted as zero would park the engine on whichever
	// account resets LAST.
	//
	// The barred account is deliberately included: this asks whether the FLEET
	// has quota, and the bar is about which account to move to, not about what
	// exists.
	bestCandidateHeadroom := 0.0
	for _, num := range in.Candidates {
		if value := in.Headroom[num]; value != nil {
			bestCandidateHeadroom = max(bestCandidateHeadroom, *value)
		}
	}

	activeRecovery := math.Inf(1)
	if allAbove {
		activeRecovery = bindingRecoverySeconds(in.Snapshot.Entries[in.Current], in.Models, in.Now)
	}

	type scored struct {
		// tier keeps the two key shapes comparable: a candidate returning
		// inside the horizon beats one that does not, whatever its headroom.
		// Compared elementwise, a raw headroom against an epoch timestamp, and
		// headroom would win on magnitude alone.
		tier     int
		recovery float64
		headroom float64
		reset    float64
		num      string
		order    int
	}
	var qualifying, fallback []scored

	for order, num := range in.Candidates {
		value := in.Headroom[num]
		if value == nil {
			continue
		}
		anyKnown = true // it exists and is readable, whatever happens next
		headroom := *value
		if headroom <= 0 {
			continue // itself at its limit: never a target
		}
		if num == in.NoReturn {
			continue
		}

		reset := math.Inf(1)
		if in.ConsumeFirst {
			reset = weeklyReset(in.Snapshot.Entries[num], in.Now)
		}
		recovery := math.Inf(1)
		if allAbove {
			recovery = bindingRecoverySeconds(in.Snapshot.Entries[num], in.Models, in.Now)
		}

		byRecovery := false
		if in.Trigger == TriggerProactive || in.Trigger == TriggerConsumeFirst {
			// Landing must be healthy, or the move re-triggers on the very next
			// tick. At-limit and failover skip this block entirely: there, any
			// account with real headroom beats a blocked or dead one.
			if !allAbove && (100.0-headroom) >= in.Threshold {
				continue
			}

			switch {
			case allAbove:
				// Checked before the strategies, because with nothing below the
				// threshold the strategy question is moot: consume-first exists
				// to spend perishable weekly quota, and every account here is
				// blocked on a window that returns in minutes. Both strategies
				// want the same thing — the account that can work again first.
				byRecovery = recoveryIsUseful(recovery, activeRecovery,
					activeHeadroom, bestCandidateHeadroom, nowSeconds)
				if byRecovery {
					// Hysteresis on the axis actually ranked by. It bounds the
					// flap RATE rather than making a reverse move impossible.
					if recovery >= activeRecovery-RecoveryHysteresis.Seconds() {
						continue
					}
				} else {
					// The headroom axis, with a RATIO margin — also a rate
					// bound, not an impossibility: a target that burns down to
					// half of what it beat can qualify in reverse, which takes
					// a real collapse.
					if headroom < activeHeadroom*HorizonHeadroomRatio {
						// One narrow re-admission: when the active is spent,
						// a peer that is no worse AND returns meaningfully
						// sooner is still better than parking here. Used only
						// when nothing else qualifies.
						if activeHeadroom <= SpentHeadroom && headroom >= activeHeadroom &&
							recovery < activeRecovery-RecoveryHysteresis.Seconds() {
							fallback = append(fallback, scored{
								tier: 0, recovery: recovery, headroom: headroom,
								reset: reset, num: num, order: order,
							})
						}
						continue
					}
				}

			case in.ConsumeFirst && in.Trigger == TriggerConsumeFirst:
				// Purely proactive on reset ordering: below the threshold, only
				// move to an account whose weekly window resets SOONER. Above
				// it the move is forced, so any healthy account qualifies and
				// the sort picks the soonest.
				if math.IsInf(reset, 1) || math.IsInf(activeReset, 1) || reset >= activeReset {
					continue
				}

			case in.ActiveHeadroom != nil:
				// The default strategy: beat the active account by the full
				// margin. A one-way move like 99% to 89% qualifies; a near-line
				// pair cannot flap back.
				if headroom-activeHeadroom < in.HysteresisPct {
					continue
				}
			}
		}

		entry := scored{recovery: recovery, headroom: headroom, reset: reset, num: num, order: order}
		switch {
		case allAbove && (in.Trigger == TriggerProactive || in.Trigger == TriggerConsumeFirst):
			// Ranked on the axis its own gate chose, and tiered so the two stay
			// comparable. The recovery time appears in BOTH tiers: two peers
			// with equal headroom past the horizon would otherwise tie and fall
			// through to slot order, when the sooner reset is plainly better.
			if byRecovery {
				entry.tier = 0
			} else {
				entry.tier = 1
			}
		case in.ConsumeFirst:
			// Soonest weekly reset first, most headroom breaking ties.
			entry.tier = 2
		default:
			entry.tier = 3
		}
		qualifying = append(qualifying, entry)
	}

	if len(qualifying) == 0 {
		qualifying = fallback
	}

	slices.SortFunc(qualifying, func(a, b scored) int {
		if a.tier != b.tier {
			return a.tier - b.tier
		}
		switch a.tier {
		case 0:
			// Soonest recovery, then most headroom.
			if c := compareFloat(a.recovery, b.recovery); c != 0 {
				return c
			}
			return compareFloat(b.headroom, a.headroom)
		case 1:
			// Most headroom, with the recovery time breaking ties.
			if c := compareFloat(b.headroom, a.headroom); c != 0 {
				return c
			}
			return compareFloat(a.recovery, b.recovery)
		case 2:
			if c := compareFloat(a.reset, b.reset); c != 0 {
				return c
			}
			return compareFloat(b.headroom, a.headroom)
		default:
			if c := compareFloat(b.headroom, a.headroom); c != 0 {
				return c
			}
		}
		// Slot order breaks every remaining tie, so the choice is reproducible.
		return a.order - b.order
	})

	ordered = make([]string, len(qualifying))
	for i, entry := range qualifying {
		ordered[i] = entry.num
	}
	return ordered, anyKnown
}

// everyAccountAboveThreshold reports whether the active account and every
// measurable candidate are all at or over the threshold.
func everyAccountAboveThreshold(in rankInput) bool {
	if in.ActiveHeadroom == nil || 100.0-*in.ActiveHeadroom < in.Threshold {
		return false
	}
	anyKnown := false
	for _, num := range in.Candidates {
		value := in.Headroom[num]
		if value == nil {
			// Skipped rather than counted as spent: an unreadable row is no
			// evidence, and counting it either way would decide this on noise.
			continue
		}
		anyKnown = true
		if 100.0-*value < in.Threshold {
			return false
		}
	}
	return anyKnown
}

// noReturnAccount is the account this engine most recently left, while it is
// still barred, plus whether it has genuinely recovered.
//
// NEVER UNDO THE PREVIOUS MOVE. Each anti-flap margin is one-way on its own
// axis, but WHICH axis applies is a property of the pair's state, and burning
// changes that state — so a burning pair crosses the boundary repeatedly and
// each crossing re-opens a move. The bar closes that.
//
// Scoped like every sibling margin: at-limit and failover skip the anti-flap
// gates by design, and barring there would strand a two-account fleet on an
// exhausted active. Scoped, too, to the engine's OWN landing — once the user
// switches by hand the engine is no longer sitting where it put itself, that
// move is already undone, and the bar would merely withhold the fleet's best
// account.
func (e *Engine) noReturnAccount(state State, in rankInput) (barred string, recovered bool) {
	if in.Trigger != TriggerProactive && in.Trigger != TriggerConsumeFirst {
		return "", true
	}
	if state.LastSwitchFrom == "" {
		return "", true
	}
	// Only while still standing where that switch put us. A record written
	// before the landing was tracked cannot prove the engine moved away, so it
	// KEEPS the bar — the conservative reading, and the only one that does not
	// silently drop the bound for an upgrade cycle.
	if state.LastSwitchTo != "" && state.LastSwitchTo != in.Current {
		return "", true
	}

	barred = state.LastSwitchFrom
	recovered = e.leftAccountRecovered(state, in)
	if !recovered {
		// The ratio below comes true on its own as the active burns; see
		// leftAccountRecovered.
		return barred, false
	}

	// Released when the account left now beats the active by the same ratio the
	// anti-flap margin uses: that is not the flip this bars, it is a move the
	// outbound leg would have made on its own merits.
	leftHeadroom := in.Headroom[barred]
	if leftHeadroom != nil {
		if in.ActiveHeadroom != nil {
			if *leftHeadroom >= *in.ActiveHeadroom*HorizonHeadroomRatio {
				return "", true
			}
		} else if *leftHeadroom > 100.0-in.Threshold {
			// An unreadable active must not be silently scored as "the peer
			// does not beat it": the same landing-eligible fallback the
			// recovery check uses when it, too, has no active to compare with.
			return "", true
		}
	}
	return barred, true
}

// leftAccountRecovered asks the one question the ranking cannot: is the account
// we left a better proposition than when we LEFT it?
//
// This is the release the bar needs. A bar that leaves nothing is a stall, but
// "leaves nothing" is also what every flap looks like on two accounts, so
// lifting on emptiness alone lifts always. The distinction is not in the present
// state — it is between the present and the moment of departure, which is why
// the switch records that moment alongside where it came from.
//
// No recorded snapshot means RELEASE. Of the two failure modes, a permanent
// proactive lockout is the worse one: it is persisted, and survives a restart
// and a week of wall clock. Absence of evidence releases.
func (e *Engine) leftAccountRecovered(state State, in rankInput) bool {
	barred := state.LastSwitchFrom
	if barred == "" {
		return true
	}
	if state.LeftHeadroom == nil && state.LeftRecoveryAt == nil && state.LeftTrigger == "" {
		// A record from before the departure snapshot existed: genuinely no
		// evidence.
		return true
	}

	peerHeadroom := in.Headroom[barred]
	nowSeconds := float64(in.Now.UnixMilli()) / 1000

	// A failover departure recorded no severity — that is what "unmeasured"
	// means — so there is no baseline to diff against and the active's headroom
	// cannot be read into one. Two legs, both against CURRENT state.
	if state.LeftTrigger == TriggerFailover ||
		(state.LeftTrigger == "" && state.LeftHeadroom == nil && state.LeftRecoveryAt == nil) {
		// Would the ranking accept this peer as a landing spot right now? The
		// same test every candidate already faces, reused rather than inventing
		// a constant.
		if peerHeadroom != nil && *peerHeadroom > 100.0-in.Threshold {
			return true
		}
		// That floor is the exact complement of "every account above the
		// threshold", so it is unsatisfiable whenever the fleet is all-spent —
		// and that regime is precisely where the recovery axis is the one to
		// trust.
		peerRecovery := bindingRecoverySeconds(in.Snapshot.Entries[barred], in.Models, in.Now)
		activeRecovery := bindingRecoverySeconds(in.Snapshot.Entries[in.Current], in.Models, in.Now)
		// The active's recovery must be a REAL measurement, not merely larger:
		// an infinite value covers both "never resets" and "nobody knows", and
		// reading it as "never" would release onto a peer arbitrarily far out on
		// no evidence. But an active that reports a percentage with no reset is
		// plainly alive and burning, not unknown — so a peer inside the horizon
		// releases regardless.
		near := peerRecovery-nowSeconds <= RecoveryHorizon.Seconds()
		return (!math.IsInf(activeRecovery, 1) || near) &&
			peerRecovery < activeRecovery-RecoveryHysteresis.Seconds()
	}

	// Dominance over the ACTIVE. A peer moved away from for a reason other than
	// headroom — consume-first's reset ordering — can dominate from the moment
	// it was left, and self-improvement against its own baseline would never
	// fire for an account that had nothing to improve on.
	if peerHeadroom != nil {
		if in.ActiveHeadroom != nil {
			// The bare ratio is not enough: measured on a frozen peer against a
			// burning active, it flips true purely because the active kept
			// burning. The extra margin makes that take a genuine collapse.
			if *peerHeadroom > *in.ActiveHeadroom*HorizonHeadroomRatio+SpentHeadroom {
				return true
			}
		} else if *peerHeadroom > 100.0-in.Threshold {
			// Unreadable active: a different state from "readable, and does not
			// dominate", and it must not answer the same way.
			return true
		}
	}

	// Self-improvement against the departure baseline, with the margin the
	// headroom axis already uses.
	if state.LeftHeadroom != nil && peerHeadroom != nil &&
		*peerHeadroom >= min(*state.LeftHeadroom+SpentHeadroom, 100.0) {
		return true
	}

	// Or its binding reset pulled meaningfully nearer. Burn cannot fake this: a
	// reset moves nearer only when a nearer window starts binding.
	was := math.Inf(1)
	if state.LeftRecoveryAt != nil {
		was = *state.LeftRecoveryAt
	}
	return bindingRecoverySeconds(in.Snapshot.Entries[barred], in.Models, in.Now) <
		was-RecoveryHysteresis.Seconds()
}

// weeklyReset is when an account's seven-day window rolls over, as epoch
// seconds, or infinity when nobody knows.
func weeklyReset(entry usagestore.Entry, now time.Time) float64 {
	decision, known := entry.DecisionValue()
	if !known || decision.Usage == nil || decision.Usage.SevenDay == nil {
		return math.Inf(1)
	}
	reset, ok := decision.Usage.SevenDay.ResetTime()
	if !ok || !reset.After(now) {
		return math.Inf(1)
	}
	return float64(reset.UnixMilli()) / 1000
}

// bindingRecovery is when an account can work again: the LAST of its
// at-or-over-limit windows to reset.
//
// The last, not the first: an account is usable again only once every window
// that blocks it has rolled over.
func bindingRecovery(entry usagestore.Entry, models []string, now time.Time) (time.Time, bool) {
	decision, known := entry.DecisionValue()
	if !known || decision.Usage == nil {
		return time.Time{}, false
	}
	var latest time.Time
	found := false
	for _, window := range decision.Usage.Windows(models) {
		if window.Pct < 100.0 {
			continue
		}
		reset, ok := window.ResetTime()
		if !ok || !reset.After(now) {
			// An unknown or already-elapsed reset makes the whole answer
			// unknown: an account nobody can schedule around must not look like
			// one that recovers immediately.
			return time.Time{}, false
		}
		if !found || reset.After(latest) {
			latest, found = reset, true
		}
	}
	if !found {
		// Nothing is at its limit, so there is nothing to recover FROM. That is
		// not a recovery time.
		return time.Time{}, false
	}
	return latest, true
}

// bindingRecoverySeconds is bindingRecovery as epoch seconds, with infinity for
// "unknown or never" — an account nobody can schedule around.
func bindingRecoverySeconds(entry usagestore.Entry, models []string, now time.Time) float64 {
	reset, ok := bindingRecovery(entry, models, now)
	if !ok {
		return math.Inf(1)
	}
	return float64(reset.UnixMilli()) / 1000
}

func compareFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
