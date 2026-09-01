package swap

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/pollpolicy"
	"github.com/d0lim/aaswap/internal/settings"
	"github.com/d0lim/aaswap/internal/usage"
	"github.com/d0lim/aaswap/internal/usagestore"
)

// Sentinel states: derived conditions that answer for an account instead of a
// measurement.
//
// Every one is re-derived on each collect pass and NEVER persisted, so a
// sentinel cannot outlive the condition that produced it. The strings are a
// display and JSON contract shared with the Python implementation.
const (
	// SentinelNoCredentials means the slot has nothing stored to fetch with.
	SentinelNoCredentials = "no credentials"
	// SentinelTokenExpired means the access token is expired and this pass
	// could not refresh it — a live session owns the profile, a lock is held,
	// or the refresh gate deferred.
	SentinelTokenExpired = "token expired"
	// SentinelAPIKey means a managed API-key slot, which has no subscription
	// quota to report.
	SentinelAPIKey = "api key"
	// SentinelKeychainUnavailable means the credential exists but cannot be
	// read right now.
	SentinelKeychainUnavailable = "keychain unavailable"
	// SentinelReloginRequired means the refresh lineage is dead: only a
	// re-login helps, and the slot is quarantined from further fetches.
	SentinelReloginRequired = "re-login needed"
	// SentinelForeignCredential means the live credential belongs to another
	// account, so its quota is not this slot's.
	SentinelForeignCredential = "foreign credential"
)

// DefaultFetchStagger spaces request starts so N accounts never hit the
// endpoint in the same instant. The budget is a rolling window, and a
// simultaneous burst is the shape that saturates it.
const DefaultFetchStagger = 250 * time.Millisecond

// AccountView is what a collect pass needs about one slot: its record, its
// stored credential, and whether it is the live one.
type AccountView struct {
	Number   string
	Account  *Account
	IsActive bool
	// Credentials is the live credential for the active slot and the stored
	// backup for every other. Empty when there is none.
	Credentials string
	// Unreadable marks a credential that EXISTS but could not be read, which is
	// not the same as having none.
	Unreadable bool
}

// Identity narrows the view to the composite that keys the usage table.
func (v AccountView) Identity() usagestore.Identity {
	return usagestore.Identity{
		Email:            v.Account.Email,
		OrganizationUUID: v.Account.OrganizationUUID,
	}
}

// AccountViews reads every managed slot's credential and marks the live one.
//
// One pass over the store, so the whole collect works from a consistent picture
// rather than re-reading per account.
func (s *Switcher) AccountViews(roster *Roster) []AccountView {
	active := s.Creds.ReadActive()
	liveSlot := ""
	if live, ok := s.LiveIdentity(); ok {
		if num, managed := roster.FindSlot(live.Identity()); managed {
			liveSlot = num
		}
	}

	var views []AccountView
	for _, num := range roster.Numbers() {
		account := roster.Accounts[num]
		view := AccountView{Number: num, Account: account, IsActive: num == liveSlot}
		if view.IsActive {
			view.Credentials = active.Value
			view.Unreadable = active.FileReadFailed || active.KeychainUnavailable
		} else {
			view.Credentials, view.Unreadable = s.Creds.ReadAccount(num, account.Email)
		}
		views = append(views, view)
	}
	return views
}

// CollectRequest selects which accounts a collect pass may fetch.
type CollectRequest struct {
	// Fetch names the slots the caller wants measured. Nil makes every account
	// a candidate but RESPECTS the persisted poll plans — the on-demand mode,
	// for a list, a status, or a dashboard that repaints often.
	//
	// A non-nil set is the auto engine's deliberate schedule: its members may
	// beat the serve TTL when their plan says so.
	Fetch map[string]bool
	// Scheduled marks the engine's non-escalating mode, where a valid future
	// plan is respected.
	Scheduled bool
}

// Collect returns one usage entry per managed account, fetching what is due.
//
// Final eligibility — freshness, backoff, leases, plans — is decided atomically
// by the store's reserve, so concurrent collectors can never double-fetch a
// slot. After each successful fetch the adapted cadence is persisted in the
// same transaction as the measurement, making every surface inherit one plan.
//
// A failed fetch updates only the error and backoff fields, so the last-good
// measurement keeps being served.
func (s *Switcher) Collect(ctx context.Context, roster *Roster, views []AccountView, req CollectRequest) map[string]usagestore.Entry {
	identities := make(map[string]usagestore.Identity, len(views))
	byNum := make(map[string]AccountView, len(views))
	for _, view := range views {
		identities[view.Number] = view.Identity()
		byNum[view.Number] = view
	}

	threshold, models := s.pollPolicyInputs()
	sentinels := map[string]string{}
	for _, view := range views {
		if sentinel := s.staticSentinel(view); sentinel != "" {
			sentinels[view.Number] = sentinel
		}
	}

	entries := s.Usage.Entries(identities, models)

	// A dead refresh lineage is a quarantine. Surfacing it here both drives the
	// "re-login needed" display and — by excluding the slot from the fetch set
	// below — stops the endless loop that would otherwise draw a 401 forever.
	for _, view := range views {
		num := view.Number
		if sentinels[num] != "" {
			continue
		}
		entry := entries[num]
		switch {
		case s.entryTokenDead(entry, view):
			sentinels[num] = SentinelReloginRequired
		case entry.AuthDeadStrikes > 0 && entry.TokenDead(""):
			// Struck, but no stored source still matches the condemned
			// generation — the fingerprint healed the verdict. Clear the stale
			// row too: display and fetch eligibility must agree, or the slot
			// silently freezes at its last measurement.
			if err := s.Usage.ClearDeadToken([]string{num},
				map[string]usagestore.Identity{num: identities[num]}); err != nil {
				slog.Warn("could not clear a healed dead-token strike", "account", num, "error", err)
			}
			entries = s.Usage.Entries(identities, models)
		}
	}

	var requested []string
	for _, view := range views {
		num := view.Number
		if sentinels[num] != "" {
			continue
		}
		if req.Fetch != nil && !req.Fetch[num] {
			continue
		}
		requested = append(requested, num)
	}

	mode := usagestore.Mode{RespectPlans: req.Fetch == nil, RepairOverslept: req.Scheduled}
	if req.Fetch == nil {
		// Repair reset-parked plans written by releases that stopped polling an
		// exhausted account until its advertised reset. The store recognizes
		// that impossible deadline under the same lock that installs the lease,
		// so a concurrent valid replan is never bypassed.
		mode.RepairOverslept = true
	}
	claims, err := s.Usage.Reserve(requested, identities, mode)
	if err != nil {
		slog.Warn("could not reserve accounts for a usage fetch", "error", err)
		claims = map[string]string{}
	}

	// An expired ACTIVE credential that cannot reach the fetch path this tick —
	// backoff, another collector's lease, a plan gate — must still surface the
	// expired state, so the auto engine idle-holds instead of counting the gap
	// toward a spurious failover. When the gate lifts, the fetch path refreshes
	// the token and the sentinel clears itself.
	for _, view := range views {
		num := view.Number
		if sentinels[num] != "" || !view.IsActive {
			continue
		}
		if _, claimed := claims[num]; claimed {
			continue
		}
		if payload, ok := claudeapi.OAuthPayload(view.Credentials); ok && claudeapi.Expired(payload, s.now()) {
			sentinels[num] = SentinelTokenExpired
		}
	}

	if len(claims) > 0 {
		pre := entries
		var toFetch []AccountView
		for num := range claims {
			toFetch = append(toFetch, byNum[num])
		}
		records := s.runFetches(ctx, toFetch)
		plans := s.plansAfterFetch(records, pre, byNum, threshold, models)

		accepted, err := s.Usage.Record(records, identities, claims, plans)
		if err != nil {
			slog.Warn("could not record usage outcomes", "error", err)
			accepted = map[string]bool{}
		}
		for num := range accepted {
			if records[num].Sentinel != "" {
				sentinels[num] = records[num].Sentinel
			}
		}
		entries = s.Usage.Entries(identities, models)

		// A fetch that just returned a permanent verdict advances the strike to
		// the dead threshold. The pre-fetch scan could not see it, so surface
		// "re-login needed" in THIS pass rather than leaving the slot looking
		// merely refresh-failed until the next one notices.
		for num := range accepted {
			if s.entryTokenDead(entries[num], byNum[num]) {
				sentinels[num] = SentinelReloginRequired
			}
		}
	}

	out := make(map[string]usagestore.Entry, len(views))
	for _, view := range views {
		out[view.Number] = usagestore.WithSentinel(entries[view.Number], sentinels[view.Number])
	}
	return out
}

// staticSentinel is the state derivable with no network call at all.
func (s *Switcher) staticSentinel(view AccountView) string {
	if credstore.LooksLikeAPIKey(view.Credentials) {
		// A managed API-key account has no subscription quota to fetch.
		return SentinelAPIKey
	}
	if view.Credentials != "" && claudeapi.AccessToken(view.Credentials) != "" {
		// An expired ACTIVE token is deliberately NOT static: the fetch path
		// refreshes it under Claude Code's own lock protocol, so the collect
		// pass must reach it rather than short-circuit here.
		return ""
	}
	if view.Unreadable {
		// THIS slot's own read, not a process-wide flag — one slot's clean read
		// must not erase the verdict for every other slot. "No credentials"
		// sends the user to re-add a slot that has one.
		return SentinelKeychainUnavailable
	}
	return SentinelNoCredentials
}

// entryTokenDead answers the fingerprint-bound dead verdict against EVERY
// stored source.
//
// For an idle slot the backup is the only source a strike can bind to. The
// ACTIVE slot has two — the live credential and the backup — because the
// recovery path legitimately spends the BACKUP's grant. Comparing the strike
// only against the live bytes mis-heals it on every pass whenever the two
// lineages differ, which is the strike-heal-respend loop that keeps a dead
// backup out of quarantine forever.
func (s *Switcher) entryTokenDead(entry usagestore.Entry, view AccountView) bool {
	if entry.TokenDead(claudeapi.Fingerprint(view.Credentials)) {
		return true
	}
	if !view.IsActive {
		return false
	}
	backup, unreadable := s.Creds.ReadAccount(view.Number, view.Account.Email)
	if unreadable {
		// The second source cannot be seen, so "no stored source matches the
		// condemned generation" is UNPROVEN — and the caller spends that answer
		// on clearing the strike, which zeroes the quarantine in the persisted
		// store. One momentary lock would un-quarantine a genuinely dead
		// account permanently. Holding the strike costs one pass of "re-login
		// needed"; erasing it costs the quarantine itself.
		//
		// Only a strike that EXISTS can be held. The empty fingerprint asks
		// the count alone, because an unreadable source cannot be
		// fingerprint-matched; the check above already proved the live
		// credential does not carry the condemned generation, so a row that
		// was never struck has nothing here to preserve, and answering true
		// for it would manufacture "re-login needed" for a healthy account
		// out of one momentary lock.
		return entry.TokenDead("")
	}
	return backup != "" && entry.TokenDead(claudeapi.Fingerprint(backup))
}

// runFetches measures the given accounts in parallel, staggering request starts.
func (s *Switcher) runFetches(ctx context.Context, views []AccountView) map[string]usagestore.FetchRecord {
	records := make(map[string]usagestore.FetchRecord, len(views))
	stagger := s.FetchStagger
	if stagger == 0 {
		stagger = DefaultFetchStagger
	}
	var mu sync.Mutex
	var wg sync.WaitGroup

	for i, view := range views {
		wg.Go(func() {
			if i > 0 && stagger > 0 {
				select {
				case <-time.After(time.Duration(i) * stagger):
				case <-ctx.Done():
					return
				}
			}
			record := s.fetchAccountUsage(ctx, view)
			mu.Lock()
			defer mu.Unlock()
			records[view.Number] = record
		})
	}
	wg.Wait()
	return records
}

// fetchAccountUsage measures one account. It never fails outward: every problem
// becomes a record the store can pace.
func (s *Switcher) fetchAccountUsage(ctx context.Context, view AccountView) usagestore.FetchRecord {
	if s.Fetcher == nil {
		return usagestore.FetchRecord{Error: claudeapi.KindTransient}
	}

	req := claudeapi.FetchRequest{
		AccountNum:  view.Number,
		Email:       view.Account.Email,
		Credentials: view.Credentials,
		Now:         s.now(),
	}

	if view.IsActive {
		// The active account owns the live credential: Claude Code refreshes it
		// lazily on its next API call, and rotating it here would invalidate
		// the token that process is holding. So the fetch is strictly
		// read-only, and an expired token earns a sentinel rather than a grant.
		if payload, ok := claudeapi.OAuthPayload(view.Credentials); ok &&
			claudeapi.Expired(payload, s.now()) {
			return usagestore.FetchRecord{Sentinel: SentinelTokenExpired}
		}
		req.IsActive = true
		return toFetchRecord(s.Fetcher.FetchUsageForAccount(ctx, req))
	}

	// An idle slot's expired token is refreshed through the consume gate, which
	// re-reads the freshest copy under the store lock, spends it once, and
	// persists or stashes the successor.
	req.Refresher = consumeGate{s}
	return toFetchRecord(s.Fetcher.FetchUsageForAccount(ctx, req))
}

// consumeGate adapts the gate to the fetcher's refresher interface.
type consumeGate struct{ s *Switcher }

func (g consumeGate) Refresh(ctx context.Context, accountNum, email, credentials string) claudeapi.RefreshOutcome {
	return g.s.ConsumeBackupGrant(ctx, accountNum, email, credentials)
}

func toFetchRecord(outcome claudeapi.UsageOutcome) usagestore.FetchRecord {
	return usagestore.FetchRecord{
		Usage:      outcome.Usage,
		Error:      outcome.Error,
		RetryAfter: outcome.RetryAfter,
		StruckFP:   outcome.StruckFP,
	}
}

// plansAfterFetch builds the cadence updates that commit with their
// measurements.
//
// Only successes get a plan. A failure is paced by the store's backoff and
// keeps its past-due plan for when that backoff lifts.
func (s *Switcher) plansAfterFetch(
	records map[string]usagestore.FetchRecord,
	pre map[string]usagestore.Entry,
	byNum map[string]AccountView,
	threshold float64,
	models []string,
) map[string]pollpolicy.Plan {
	now := s.now()
	plans := map[string]pollpolicy.Plan{}
	for num, record := range records {
		if record.Sentinel != "" || record.Error != "" {
			continue
		}
		before := pre[num]
		var prevUsage *usage.Result
		if before.LastGood != nil {
			prevUsage = before.LastGood
		}
		plans[num] = pollpolicy.AfterFetch(pollpolicy.Input{
			PrevInterval: before.PollInterval,
			PrevUsage:    prevUsage,
			NewUsage:     record.Usage,
			IsActive:     byNum[num].IsActive,
			Threshold:    threshold,
			Models:       models,
			Recent429:    before.Recent429(now),
			Now:          now,
		})
	}
	return plans
}

// pollPolicyInputs are the decision threshold and scoped models the planner and
// the trust bound share.
//
// One source, so the cadence and the 429-stale trust bound cannot disagree
// about which windows gate an account.
func (s *Switcher) pollPolicyInputs() (threshold float64, models []string) {
	auto := s.Settings.AutoSwitch
	threshold = auto.Threshold
	if threshold == 0 {
		threshold = settings.Defaults().AutoSwitch.Threshold
	}
	return threshold, settings.ParseModelNames(auto.Model)
}
