package autoswitch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strconv"
	"time"

	"github.com/realiti4/claude-swap/internal/claudeapi"
	"github.com/realiti4/claude-swap/internal/jsonout"
	"github.com/realiti4/claude-swap/internal/pollpolicy"
	"github.com/realiti4/claude-swap/internal/settings"
	"github.com/realiti4/claude-swap/internal/swap"
	"github.com/realiti4/claude-swap/internal/usage"
	"github.com/realiti4/claude-swap/internal/usagestore"
)

// Outcome is what one tick decided. The values double as exit codes for a
// single-tick run, so a cron wrapper can branch on them.
type Outcome int

const (
	// Switched: the live login moved.
	Switched Outcome = 0
	// Failed: the tick could not complete. Distinct from every decision below,
	// because a wrapper retrying on failure must not retry on "nothing to do".
	Failed Outcome = 1
	// NoAction: nothing needed doing, or nothing could be shown to help.
	NoAction Outcome = 2
	// Blocked: the engine WANTED to switch and had nowhere to go. A wrapper can
	// alert on this without alerting on an idle machine.
	Blocked Outcome = 3
)

// String names an outcome.
func (o Outcome) String() string {
	switch o {
	case Switched:
		return "switched"
	case Failed:
		return "error"
	case NoAction:
		return "no-action"
	case Blocked:
		return "blocked"
	}
	return "unknown"
}

// Triggers name why a tick wanted to move.
const (
	// TriggerAtLimit: the active account has no headroom left.
	TriggerAtLimit = "at-limit"
	// TriggerProactive: it is above the threshold but not yet spent.
	TriggerProactive = "proactive"
	// TriggerFailover: its usage cannot be read at all, for long enough to act
	// on.
	TriggerFailover = "failover"
	// TriggerConsumeFirst: it is healthy, but another account's quota is more
	// perishable.
	TriggerConsumeFirst = "consume-first"
)

// Strategies.
const (
	StrategyBest         = "best"
	StrategyConsumeFirst = "consume-first"
)

// Engine evaluates and acts.
type Engine struct {
	Switcher *swap.Switcher
	State    *Store
	Events   Emitter

	// Settings are the policy knobs.
	Settings settings.AutoSwitch

	// DryRun decides but never acts, and never writes state.
	DryRun bool

	// Now is the clock.
	Now func() time.Time
	// Rand supplies the sleep jitter, so several machines do not synchronize.
	Rand func() float64

	// unhealthyTicks counts consecutive ticks whose active usage was unknown.
	unhealthyTicks int
	// idleHoldSince is when an owned-and-expired token started being tolerated.
	idleHoldSince time.Time
	// sleepUntil is a reset the next wait should not overshoot.
	sleepUntil time.Time
	// blockedWaitLong marks a block that cannot resolve on its own.
	blockedWaitLong bool
	// idleHoldSlow marks a tick that should crawl.
	idleHoldSlow bool
	// modelCheckDone marks the one-time inert-model warning as spent.
	modelCheckDone bool
}

func (e *Engine) now() time.Time {
	if e.Now == nil {
		return time.Now()
	}
	return e.Now()
}

func (e *Engine) emit(event Event) {
	if e.Events != nil {
		e.Events.Emit(event)
	}
}

func (e *Engine) event(kind string) Event { return newEvent(kind, e.now()) }

// Tick evaluates once and acts if it should. It never fails outward: a failure
// becomes an event and an outcome, because a loop that dies on one bad tick is
// worse than one that reports it.
func (e *Engine) Tick(ctx context.Context) Outcome {
	outcome, err := e.tick(ctx)
	if err != nil {
		transient := true
		event := e.event(KindError)
		event.Message = err.Error()
		event.Transient = &transient
		e.emit(event)
		return Failed
	}
	return outcome
}

func (e *Engine) tick(ctx context.Context) (Outcome, error) {
	e.sleepUntil = time.Time{}
	e.blockedWaitLong = false
	e.idleHoldSlow = false

	state := e.State.Read()
	if !e.DryRun {
		// A dry run must write nothing, so recovered quarantines are only
		// released — a state mutation — on a real tick.
		released, err := e.releaseRecovered(state)
		if err != nil {
			return Failed, err
		}
		state = released
	}
	quarantined := state.Quarantined()

	roster, err := e.Switcher.RosterOrEmpty()
	if err != nil {
		return Failed, err
	}
	current, managed := e.Switcher.CurrentNumber(roster)
	if !managed {
		e.emit(e.event(KindPoll))
		reason, detail := "no-active-account", "log in and run `ccswap add` first"
		if e.Switcher.HasLiveLogin() {
			// A live login ccswap does not manage: NEVER act. A switch would
			// overwrite it with no backup anywhere.
			reason = "unmanaged-active-account"
			detail = "run `ccswap add` to include it in rotation"
		}
		event := e.event(KindNoSwitch)
		event.Reason, event.Detail = reason, detail
		e.emit(event)
		return NoAction, nil
	}

	activeAccount := roster.Accounts[current]
	snapshot, headroom, err := e.collect(ctx, roster, current, quarantined)
	if err != nil {
		return Failed, err
	}
	e.emitPoll(current, activeAccount, snapshot, headroom)

	if !e.modelCheckDone {
		e.checkModelNames(snapshot)
		e.modelCheckDone = true
	}

	if activeAccount.AuthKind() == swap.KindAPIKey && !e.Settings.IncludeAPIKeyAccounts {
		event := e.event(KindNoSwitch)
		event.Reason = "active-api-key"
		event.Detail = "API-key accounts have no quota to watch"
		e.emit(event)
		return NoAction, nil
	}

	trigger, outcome, decided := e.classify(current, snapshot, headroom)
	if !decided {
		return outcome, nil
	}

	if (trigger == TriggerProactive || trigger == TriggerConsumeFirst) &&
		state.InCooldown(e.now(), time.Duration(e.Settings.CooldownSeconds)*time.Second) {
		event := e.event(KindNoSwitch)
		event.Reason = "cooldown"
		e.emit(event)
		return NoAction, nil
	}

	return e.choose(ctx, roster, state, snapshot, headroom, current, trigger)
}

// classify decides whether this tick wants to move, and why.
//
// The three positive answers are escalating: proactive is an optimization,
// at-limit is a rescue, and failover is a guess made because nothing can be
// measured. The last one is deliberately slow to reach.
func (e *Engine) classify(current string, snapshot *swap.Snapshot, headroom map[string]*float64) (trigger string, outcome Outcome, wantsToMove bool) {
	activeHeadroom := headroom[current]

	if activeHeadroom != nil {
		// Measurable: the counters that guard against a flapping endpoint reset.
		e.unhealthyTicks = 0
		e.idleHoldSince = time.Time{}

		utilization := 100.0 - *activeHeadroom
		if utilization < e.Settings.Threshold {
			if e.Settings.Strategy != StrategyConsumeFirst {
				event := e.event(KindNoSwitch)
				event.Reason = "below-threshold"
				// Both sides through the same formatter: rounding one and not
				// the other could display an impossible "100% < 99.9%".
				event.Detail = fmt.Sprintf("%s%% < %s%%",
					pctLabel(utilization), pctLabel(e.Settings.Threshold))
				e.emit(event)
				return "", NoAction, false
			}
			// Below the threshold, consume-first still moves — toward whichever
			// account's weekly window resets soonest, to spend the most
			// perishable quota first. Candidate selection decides whether such
			// an account actually exists.
			return TriggerConsumeFirst, 0, true
		}
		if *activeHeadroom <= 0 {
			return TriggerAtLimit, 0, true
		}
		return TriggerProactive, 0, true
	}

	// Unmeasurable. Before counting toward failover, check whether this is
	// simply an idle editor.
	entry := snapshot.Entries[current]
	if entry.Sentinel == swap.SentinelTokenExpired {
		// Expired, and the refresh could not complete this pass. The refresh
		// path retries on later passes; there is no quota being burned and
		// nothing to switch for yet, so crawl instead of spending failover
		// ticks.
		now := e.now()
		if e.idleHoldSince.IsZero() {
			e.idleHoldSince = now
		}
		if now.Sub(e.idleHoldSince) <= IdleHoldMax {
			e.unhealthyTicks = 0
			e.idleHoldSlow = true
			event := e.event(KindNoSwitch)
			event.Reason = "active-idle"
			event.Detail = "the token expired while Claude Code is idle; it resumes on next use"
			e.emit(event)
			return "", NoAction, false
		}
		// Held far longer than any idle nap needs. This looks like a dead
		// refresh token with an ACTIVE user, so resume normal counting and let
		// failover happen.
		slog.Warn("the active token has been expired and owned for a long time; resuming "+
			"unhealthy counting (a dead refresh token?)", "held", now.Sub(e.idleHoldSince))
	} else {
		e.idleHoldSince = time.Time{}
	}

	e.unhealthyTicks++
	if e.unhealthyTicks < e.Settings.UnhealthyTicks {
		event := e.event(KindNoSwitch)
		event.Reason = "active-usage-unknown"
		event.Detail = fmt.Sprintf("%d/%d before failover", e.unhealthyTicks, e.Settings.UnhealthyTicks)
		e.emit(event)
		return "", NoAction, false
	}
	return TriggerFailover, 0, true
}

// choose ranks the candidates and acts on the best one that can be freshened.
func (e *Engine) choose(ctx context.Context, roster *swap.Roster, state State, snapshot *swap.Snapshot, headroom map[string]*float64, current, trigger string) (Outcome, error) {
	quarantined := state.Quarantined()

	var oauthCandidates, apiKeyCandidates []string
	for _, num := range e.Switcher.SwitchableNumbers(roster) {
		if num == current || quarantined[num] {
			continue
		}
		if roster.Accounts[num].AuthKind() == swap.KindAPIKey {
			if e.Settings.IncludeAPIKeyAccounts {
				apiKeyCandidates = append(apiKeyCandidates, num)
			}
			continue
		}
		oauthCandidates = append(oauthCandidates, num)
	}

	activeHeadroom := headroom[current]
	if trigger == TriggerConsumeFirst && len(oauthCandidates) == 0 && activeHeadroom != nil {
		// A healthy below-threshold account with no OAuth peer to compare
		// against — the same state the default strategy reports as
		// below-threshold before candidate selection is even reached. Reporting
		// it identically keeps the exit-code contract the same across
		// strategies, so a cron wrapper keying on Blocked does not see a false
		// block from the flag alone.
		event := e.event(KindNoSwitch)
		event.Reason = "below-threshold"
		event.Detail = fmt.Sprintf("%s%% < %s%%",
			pctLabel(100.0-*activeHeadroom), pctLabel(e.Settings.Threshold))
		e.emit(event)
		return NoAction, nil
	}
	if len(oauthCandidates) == 0 && len(apiKeyCandidates) == 0 {
		// This will not change until the user adds or recovers an account, so
		// there is no point re-polling at full cadence.
		e.blockedWaitLong = true
		event := e.event(KindNoSwitch)
		event.Reason = "no-candidates"
		e.emit(event)
		return Blocked, nil
	}

	ranking := rankInput{
		Trigger:        trigger,
		ConsumeFirst:   e.Settings.Strategy == StrategyConsumeFirst,
		Candidates:     oauthCandidates,
		Snapshot:       snapshot,
		Headroom:       headroom,
		Current:        current,
		ActiveHeadroom: activeHeadroom,
		Threshold:      e.Settings.Threshold,
		HysteresisPct:  e.Settings.HysteresisPct,
		Models:         settings.ParseModelNames(e.Settings.Model),
		Now:            e.now(),
	}
	ranking.NoReturn, ranking.Recovered = e.noReturnAccount(state, ranking)
	ordered, anyKnown := rankCandidates(ranking)
	if ranking.NoReturn != "" && len(ordered) == 0 && ranking.Recovered {
		// Barring the account we just left emptied the list, AND that account
		// is genuinely a better proposition than when we left it. Ask again
		// without the bar: emptiness alone cannot be the release, because with
		// two accounts barring the only candidate ALWAYS empties the list — the
		// bar would then be inert exactly at the fleet size the flapping was
		// reported on.
		unbarred := ranking
		unbarred.NoReturn = ""
		if retry, _ := rankCandidates(unbarred); len(retry) > 0 {
			ordered = retry
		}
	}

	// API-key accounts have no quota to compare, so they are a last resort:
	// somewhere to land when nothing measurable is left.
	if len(ordered) == 0 && trigger != TriggerConsumeFirst {
		ordered = apiKeyCandidates
	}

	if len(ordered) == 0 {
		return e.reportBlocked(snapshot, headroom, current, anyKnown, trigger)
	}

	return e.activateFirstUsable(ctx, roster, snapshot, headroom, current, trigger, ordered)
}

// activateFirstUsable freshens candidates in order and switches to the first
// one that can actually be activated.
func (e *Engine) activateFirstUsable(ctx context.Context, roster *swap.Roster, snapshot *swap.Snapshot, headroom map[string]*float64, current, trigger string, ordered []string) (Outcome, error) {
	systemic := map[claudeapi.ErrorKind]bool{}
	skippedLive := false

	for _, num := range ordered {
		account := roster.Accounts[num]
		verdict, kind := e.freshen(ctx, num, account)
		switch verdict {
		case freshenOK:
			return e.perform(roster, snapshot, headroom, current, num, account, trigger)
		case freshenDead:
			if err := e.quarantine(num, account.Email, "invalid_grant"); err != nil {
				return Failed, err
			}
		case freshenIdentityConflict:
			if err := e.quarantine(num, account.Email, "identity-conflict"); err != nil {
				return Failed, err
			}
		case freshenSkipLiveSession:
			// A live session owns this account's token in its own profile.
			// Activating it as the default login too would put one rotating
			// refresh token in two config directories, with nobody reading the
			// warning — and its quota is already being consumed there.
			skippedLive = true
		case freshenSystemic:
			systemic[kind] = true
		}
	}

	e.blockedWaitLong = true
	event := e.event(KindNoSwitch)
	switch {
	case len(systemic) > 0:
		kind, message, _ := mostActionable(systemic)
		event.Reason = string(kind)
		event.Detail = message
	case skippedLive:
		event.Reason = "candidates-in-session"
		event.Detail = "every candidate is running as a `ccswap run` session"
	default:
		event.Reason = "no-viable-candidate"
		event.Detail = "no candidate's stored token could be refreshed (network?)"
	}
	e.emit(event)
	return Blocked, nil
}

// reportBlocked explains why a tick that wanted to move had nowhere to go.
func (e *Engine) reportBlocked(snapshot *swap.Snapshot, headroom map[string]*float64, current string, anyKnown bool, trigger string) (Outcome, error) {
	models := settings.ParseModelNames(e.Settings.Model)

	// Everything measurable is at its limit — including where the user already
	// is. Say so, with when the soonest quota returns, and wait for it.
	if anyKnown && everyKnownExhausted(headroom) {
		reset, known := e.earliestReset(snapshot, models)
		event := e.event(KindAllExhausted)
		if known {
			event.EarliestResetAt = jsonout.Timestamp(reset)
			e.sleepUntil = reset.Add(pollpolicy.ResetSlack)
		} else {
			e.blockedWaitLong = true
		}
		e.emit(event)
		return Blocked, nil
	}

	event := e.event(KindNoSwitch)
	if !anyKnown {
		// Nothing readable anywhere. Not a block that waiting fixes, but not
		// something to act on either.
		event.Reason = "candidates-usage-unknown"
		event.Detail = "no candidate's usage could be measured"
		e.emit(event)
		return Blocked, nil
	}
	// Something is measurable and has headroom, but not enough to clear the
	// anti-flap margin. That can resolve on any tick, so the cadence stays
	// normal.
	event.Reason = "no-better-account"
	event.Detail = fmt.Sprintf("no candidate beats the active account by %s points",
		pctLabel(e.Settings.HysteresisPct))
	e.emit(event)
	return Blocked, nil
}

// everyKnownExhausted reports whether every measurable account, the active one
// included, is at or over its limit.
//
// Unreadable rows are skipped rather than counted as spent: one of them counted
// either way would decide this on noise.
func everyKnownExhausted(headroom map[string]*float64) bool {
	anyKnown := false
	for _, value := range headroom {
		if value == nil {
			continue
		}
		anyKnown = true
		if *value > 0 {
			return false
		}
	}
	return anyKnown
}

// earliestReset is when the fleet's soonest relevant window rolls over.
func (e *Engine) earliestReset(snapshot *swap.Snapshot, models []string) (time.Time, bool) {
	var earliest time.Time
	found := false
	now := e.now()
	for _, entry := range snapshot.Entries {
		decision, known := entry.DecisionValue()
		if !known || decision.Usage == nil {
			continue
		}
		for _, window := range decision.Usage.Windows(models) {
			reset, ok := window.ResetTime()
			if !ok || !reset.After(now) {
				continue
			}
			if !found || reset.Before(earliest) {
				earliest, found = reset, true
			}
		}
	}
	return earliest, found
}

// perform switches, and records what was left behind.
func (e *Engine) perform(roster *swap.Roster, snapshot *swap.Snapshot, headroom map[string]*float64, current, target string, account *swap.Account, trigger string) (Outcome, error) {
	if e.DryRun {
		event := e.event(KindSwitch)
		event.Trigger, event.DryRun = trigger, true
		event.From = accountRef(current, roster.Accounts[current].Email)
		event.To = accountRef(target, account.Email)
		e.emit(event)
		return Switched, nil
	}

	models := settings.ParseModelNames(e.Settings.Model)
	leftHeadroom := headroom[current]
	leftRecovery, _ := bindingRecovery(snapshot.Entries[current], models, e.now())

	var outcome swap.SwitchOutcome
	// The whole recheck-switch-record sequence runs under the state lock, so
	// two concurrent engines make one serialized decision: the loser re-reads
	// the winner's timestamp and backs off instead of double-switching. No
	// deadlock cycle — the switch path takes the store lock and Claude Code's,
	// never this one.
	err := e.State.WithLock(func() error {
		state := e.State.Read()
		if (trigger == TriggerProactive || trigger == TriggerConsumeFirst) &&
			state.InCooldown(e.now(), time.Duration(e.Settings.CooldownSeconds)*time.Second) {
			return errCooldown
		}

		var switchErr error
		outcome, switchErr = e.Switcher.Switch(context.Background(), swap.SwitchRequest{Target: target})
		if switchErr != nil {
			return switchErr
		}

		now := e.now()
		state.LastSwitchAt = epochOf(now)
		state.LastSwitchTo = target
		state.LastSwitchFrom = current
		state.LeftHeadroom = leftHeadroom
		if !leftRecovery.IsZero() {
			state.LeftRecoveryAt = epochOf(leftRecovery)
		} else {
			state.LeftRecoveryAt = nil
		}
		state.LeftTrigger = trigger
		return e.State.Write(state)
	})
	switch {
	case errors.Is(err, errCooldown):
		event := e.event(KindNoSwitch)
		event.Reason = "cooldown"
		e.emit(event)
		return NoAction, nil
	case err != nil:
		return Failed, err
	}

	event := e.event(KindSwitch)
	event.Trigger = trigger
	if outcome.From != nil {
		event.From = accountRef(outcome.From.Number, outcome.From.Email)
	}
	event.To = accountRef(outcome.To.Number, outcome.To.Email)
	e.emit(event)
	return Switched, nil
}

// errCooldown carries a cooldown refusal out of the locked transaction, where
// it must not look like a failure.
var errCooldown = errors.New("a switch happened too recently")

// collect polls the accounts that are due and returns their headroom.
func (e *Engine) collect(ctx context.Context, roster *swap.Roster, current string, quarantined map[string]bool) (*swap.Snapshot, map[string]*float64, error) {
	// The active account plus one candidate per tick: the endpoint's budget is
	// shared across every surface, and polling the whole fleet every tick would
	// spend it on accounts whose plans say they are not due.
	fetch := map[string]bool{current: true}
	entries := e.Switcher.Collect(ctx, roster, e.Switcher.AccountViews(roster), swap.CollectRequest{
		Fetch: map[string]bool{}, Scheduled: true,
	})

	var candidates []string
	for _, num := range e.Switcher.SwitchableNumbers(roster) {
		if num != current && !quarantined[num] {
			candidates = append(candidates, num)
		}
	}
	if due, ok := usagestore.DueCandidate(candidates, entries, e.now()); ok {
		fetch[due] = true
	}

	snapshot, err := e.Switcher.TakeSnapshot(ctx, swap.CollectRequest{Fetch: fetch, Scheduled: true})
	if err != nil {
		return nil, nil, err
	}

	models := settings.ParseModelNames(e.Settings.Model)
	headroom := make(map[string]*float64, len(snapshot.Entries))
	for num, entry := range snapshot.Entries {
		decision, known := entry.DecisionValue()
		if !known || decision.Usage == nil {
			headroom[num] = nil
			continue
		}
		value, measurable := decision.Usage.Headroom(models)
		if !measurable {
			headroom[num] = nil
			continue
		}
		headroom[num] = &value
	}
	return snapshot, headroom, nil
}

// emitPoll reports what this tick found.
func (e *Engine) emitPoll(current string, account *swap.Account, snapshot *swap.Snapshot, headroom map[string]*float64) {
	models := settings.ParseModelNames(e.Settings.Model)
	threshold := e.Settings.Threshold

	event := e.event(KindPoll)
	event.Active = accountRef(current, account.Email)
	event.HeadroomPct = headroom
	event.Threshold = &threshold
	event.FetchErrors = map[string]string{}
	event.WindowsPct = map[string]map[string]float64{}

	for num, entry := range snapshot.Entries {
		if headroom[num] == nil {
			switch {
			case entry.Sentinel != "":
				event.FetchErrors[num] = entry.Sentinel
			case entry.LastError != "":
				event.FetchErrors[num] = string(entry.LastError)
			}
			continue
		}
		decision, _ := entry.DecisionValue()
		windows := map[string]float64{}
		for _, window := range decision.Usage.Windows(models) {
			windows[window.Label] = window.Pct
		}
		if len(windows) > 0 {
			event.WindowsPct[num] = windows
		}
	}
	if len(event.FetchErrors) == 0 {
		event.FetchErrors = nil
	}
	if len(event.WindowsPct) == 0 {
		event.WindowsPct = nil
	}
	e.emit(event)
}

// checkModelNames warns once about a configured model no account reports.
//
// Not an error: the engine keeps running on the axes that do exist. But a
// silently inert setting is worse than a warning — the user configured it
// expecting it to matter.
func (e *Engine) checkModelNames(snapshot *swap.Snapshot) {
	configured := settings.ParseModelNames(e.Settings.Model)
	if len(configured) == 0 || slices.Contains(configured, usage.AllModels) {
		return
	}

	reported := map[string]bool{}
	for _, entry := range snapshot.Entries {
		decision, known := entry.DecisionValue()
		if !known || decision.Usage == nil {
			continue
		}
		for _, scoped := range decision.Usage.Scoped {
			reported[lowerASCII(scoped.Name)] = true
		}
	}
	for _, name := range configured {
		if !reported[lowerASCII(name)] {
			event := e.event(KindConfigWarning)
			event.Message = fmt.Sprintf("no account reports a per-model limit named %q, so "+
				"the autoswitch.model setting has no effect for it", name)
			e.emit(event)
		}
	}
}

func accountRef(num, email string) *jsonout.AccountRef {
	ref := &jsonout.AccountRef{Email: email}
	if number, err := strconv.Atoi(num); err == nil {
		ref.Number = &number
	}
	return ref
}

func lowerASCII(s string) string {
	out := []byte(s)
	for i, b := range out {
		if b >= 'A' && b <= 'Z' {
			out[i] = b + ('a' - 'A')
		}
	}
	return string(out)
}

// jitter is the ±10% spread applied to a sleep, so several machines do not
// synchronize their requests.
func (e *Engine) jitter() float64 {
	if e.Rand == nil {
		return rand.Float64()
	}
	return e.Rand()
}
