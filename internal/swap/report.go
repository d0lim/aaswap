package swap

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/jsonout"
	"github.com/d0lim/aaswap/internal/settings"
	"github.com/d0lim/aaswap/internal/usage"
	"github.com/d0lim/aaswap/internal/usagestore"
)

// Snapshot is one collect pass's result: the roster, each slot's view, and the
// usage entry that goes with it.
//
// Assembled once and shared by every surface, so a listing, a status and a
// switch decision made in the same breath cannot disagree about what an account
// is doing.
type Snapshot struct {
	Roster  *Roster
	Views   []AccountView
	Entries map[string]usagestore.Entry
}

// TakeSnapshot reads the roster, collects usage, and returns everything a
// surface needs.
func (s *Switcher) TakeSnapshot(ctx context.Context, req CollectRequest) (*Snapshot, error) {
	roster, err := s.RosterOrEmpty()
	if err != nil {
		return nil, err
	}
	views := s.AccountViews(roster)
	return &Snapshot{
		Roster:  roster,
		Views:   views,
		Entries: s.Collect(ctx, roster, views, req),
	}, nil
}

// decision projects a stored entry to what a decision may act on.
func decisionOf(entry usagestore.Entry) jsonout.Decision {
	value, known := entry.DecisionValue()
	if !known {
		return jsonout.Decision{}
	}
	return jsonout.Decision{Usage: value.Usage, Sentinel: value.Sentinel}
}

// ListPayload builds the machine-readable listing.
func (s *Switcher) ListPayload(snapshot *Snapshot) jsonout.ListPayload {
	now := s.now()
	payload := jsonout.ListPayload{SchemaVersion: jsonout.SchemaVersion}

	for _, view := range snapshot.Views {
		entry := snapshot.Entries[view.Name]
		if view.IsActive {
			payload.ActiveAccount = view.Name
		}

		status, projected := jsonout.UsageFields(decisionOf(entry), entry.FetchedAt, now)
		row := jsonout.AccountRow{
			Email:            view.Account.Email,
			OrganizationName: view.Account.OrganizationName,
			OrganizationUUID: view.Account.OrganizationUUID,
			IsOrganization:   view.Account.OrganizationUUID != "",
			Active:           view.IsActive,
			UsageStatus:      status,
			Usage:            projected,
			Name:             view.Name,
			Disabled:         view.Account.Disabled,
		}
		row.SetFreshness(entry.LastGood, entry.FetchedAt, entry.Age, now)
		payload.Accounts = append(payload.Accounts, row)
	}

	payload.DuplicateAccountWarnings = DuplicateAccountWarnings(snapshot.Views)
	payload.LockstepUsageWarnings = LockstepUsageWarnings(snapshot.Views, snapshot.Entries)
	if entries, verdict := s.Creds.ListUnclaimed(); verdict == "ok" && len(entries) > 0 {
		payload.UnclaimedCredentials = sortedKeys(entries)
	}
	return payload
}

// StatusPayload describes the live login.
func (s *Switcher) StatusPayload(snapshot *Snapshot) jsonout.StatusPayload {
	payload := jsonout.StatusPayload{SchemaVersion: jsonout.SchemaVersion}

	live, ok := s.LiveIdentity()
	if !ok {
		// Null: there is no live login at all, which is distinct from one aaswap
		// does not manage.
		return payload
	}

	slot, managed := snapshot.Roster.FindName(live.Identity())
	if !managed {
		payload.Active = &jsonout.ActiveStatus{Email: live.Email, Managed: false}
		return payload
	}

	account := snapshot.Roster.Accounts[slot]
	entry := snapshot.Entries[slot]
	now := s.now()
	status, projected := jsonout.UsageFields(decisionOf(entry), entry.FetchedAt, now)

	active := &jsonout.ActiveStatus{
		Email:            live.Email,
		OrganizationName: account.OrganizationName,
		OrganizationUUID: account.OrganizationUUID,
		IsOrganization:   account.OrganizationUUID != "",
		Managed:          true,
		Name:             slot,
		UsageStatus:      status,
		Usage:            projected,
	}
	row := jsonout.AccountRow{Usage: projected}
	row.SetFreshness(entry.LastGood, entry.FetchedAt, entry.Age, now)
	active.UsageFetchedAt, active.UsageAgeSeconds = row.UsageFetchedAt, row.UsageAgeSeconds
	active.LastGoodUsage = row.LastGoodUsage
	active.LastGoodFetchedAt, active.LastGoodAgeSeconds = row.LastGoodFetchedAt, row.LastGoodAgeSeconds

	total := len(snapshot.Roster.Accounts)
	payload.Active = active
	payload.TotalManagedAccounts = &total
	return payload
}

// SelectionNote explains why a strategy did not name a target.
type SelectionNote string

const (
	// NoteNone means no other switchable account exists.
	NoteNone SelectionNote = "none"
	// NoteCurrentUnavailable means the current account's usage is unknown, so
	// no comparison is possible. Stay rather than risk moving onto something
	// worse.
	NoteCurrentUnavailable SelectionNote = "current-unavailable"
	// NoteNoComparison means no other account has known usage.
	NoteNoComparison SelectionNote = "no-comparison"
	// NoteIncompleteComparison means the current account is best among those
	// that can be measured, but some candidate is unknown — so neither "it is
	// the best" nor "everything is exhausted" can be claimed.
	NoteIncompleteComparison SelectionNote = "incomplete-comparison"
	// NoteStay means the current account provably has the most headroom.
	NoteStay SelectionNote = "stay"
	// NoteExhausted means the current account is best and every account is at
	// its limit, so switching would not help.
	NoteExhausted SelectionNote = "exhausted"
)

// SelectBestSwitchable names the account with strictly more headroom than the
// current one, or explains why it will not.
//
// It only ever recommends a switch it can PROVE lands on more headroom — never
// onto an account that is worse than, or merely unverifiable against, where the
// user already is. Ties resolve in favor of staying put, and a bare rotation
// remains the way to force a move.
//
// Never fails: an unmeasurable account is a reason to stay, not an error.
func (s *Switcher) SelectBestSwitchable(snapshot *Snapshot, currentNum string) (string, SelectionNote) {
	modelNames := settings.ParseModelNames(s.Settings.AutoSwitch.Model)

	var others []string
	for _, num := range s.SwitchableNumbers(snapshot.Roster) {
		if num != currentNum {
			others = append(others, num)
		}
	}
	if len(others) == 0 {
		return "", NoteNone
	}

	currentHeadroom, currentKnown := headroomOf(snapshot.Entries[currentNum], modelNames)
	if !currentKnown {
		return "", NoteCurrentUnavailable
	}

	bestNum, bestHeadroom := "", 0.0
	anyUnknown := false
	for _, num := range others {
		headroom, known := headroomOf(snapshot.Entries[num], modelNames)
		if !known {
			anyUnknown = true
			continue
		}
		// Strictly greater keeps the FIRST maximal element, and others
		// preserves rotation order, so ties resolve to the earliest slot.
		if bestNum == "" || headroom > bestHeadroom {
			bestNum, bestHeadroom = num, headroom
		}
	}
	if bestNum == "" {
		return "", NoteNoComparison
	}
	if bestHeadroom > currentHeadroom {
		return bestNum, ""
	}

	// The current account is at least as good as everything measurable. Stay —
	// but only claim "all exhausted" when every candidate's usage is known.
	if anyUnknown {
		return "", NoteIncompleteComparison
	}
	if currentHeadroom <= 0 {
		return "", NoteExhausted
	}
	return "", NoteStay
}

// headroomOf is an entry's decision-grade headroom.
//
// A sentinel is not a measurement, so it reports unknown — which callers must
// treat as "do not skip this account", never as exhausted.
func headroomOf(entry usagestore.Entry, models []string) (float64, bool) {
	decision, known := entry.DecisionValue()
	if !known || decision.Usage == nil {
		return 0, false
	}
	return decision.Usage.Headroom(models)
}

// DuplicateAccountWarnings names slots that provably authenticate as the same
// account.
//
// Impossible by construction, so a collision means one slot's credential was
// overwritten with another's, or the same account was registered twice. Two
// offline signals: an identical credential lineage across two slots, or the
// same recorded account uuid and organization.
//
// It cannot see two different GENERATIONS of one account — the poisoned end
// state — because those carry different fingerprints and untouched roster
// identities. [LockstepUsageWarnings] covers that case heuristically.
func DuplicateAccountWarnings(views []AccountView) []string {
	// The account uuid scoped by organization — not the roster's Identity,
	// which keys on the address. The address is exactly what a poisoned slot
	// can be lying about.
	type accountKey struct{ uuid, org string }

	byFingerprint := map[string]string{}
	byIdentity := map[accountKey]string{}
	var out []string

	for _, view := range views {
		if view.Credentials != "" {
			if fp := claudeapi.Fingerprint(view.Credentials); fp != "" {
				if other, seen := byFingerprint[fp]; seen {
					out = append(out, fmt.Sprintf(
						"Accounts %s and %s hold the same credential (%s) — one slot's backup "+
							"was overwritten. Log in with the missing account and re-add it: "+
							"aaswap add --slot N", other, view.Name, view.Account.Email))
				} else {
					byFingerprint[fp] = view.Name
				}
			}
		}
		// An empty uuid is an add-token placeholder and must never match
		// another empty one.
		if view.Account.UUID == "" {
			continue
		}
		key := accountKey{uuid: view.Account.UUID, org: view.Account.OrganizationUUID}
		if other, seen := byIdentity[key]; seen && other != view.Name {
			out = append(out, fmt.Sprintf(
				"Accounts %s and %s both authenticate as %s — remove or re-login one of them.",
				other, view.Name, view.Account.Email))
		} else if !seen {
			byIdentity[key] = view.Name
		}
	}
	return out
}

// LockstepUsageWarnings names slots whose usage moves in perfect lockstep.
//
// Two different generations of one account carry different fingerprints and
// untouched roster identities, so the offline check cannot see them — but both
// tokens report the SAME account's usage. Identical five-hour and seven-day
// percentages with identical reset timestamps is that signature.
//
// Heuristic, not proof. It goes quiet once the older generation dies, and only
// rows where both windows carry a reset are compared: two idle accounts at 0%
// with nothing scheduled are indistinguishable, and are never flagged.
func LockstepUsageWarnings(views []AccountView, entries map[string]usagestore.Entry) []string {
	type signature struct {
		fivePct, sevenPct     float64
		fiveReset, sevenReset string
	}
	seen := map[signature]string{}
	var out []string

	for _, view := range views {
		decision, known := entries[view.Name].DecisionValue()
		if !known || decision.Usage == nil {
			continue
		}
		result := decision.Usage
		if result.FiveHour == nil || result.SevenDay == nil {
			continue
		}
		if result.FiveHour.ResetsAt == "" || result.SevenDay.ResetsAt == "" {
			continue
		}
		key := signature{
			fivePct: result.FiveHour.Pct, fiveReset: result.FiveHour.ResetsAt,
			sevenPct: result.SevenDay.Pct, sevenReset: result.SevenDay.ResetsAt,
		}
		if other, dup := seen[key]; dup {
			out = append(out, fmt.Sprintf(
				"Accounts %s and %s report identical usage and reset times — they may be "+
					"the same account. If it persists, log in with the missing account and "+
					"re-add it: aaswap add --slot N", other, view.Name))
		} else {
			seen[key] = view.Name
		}
	}
	return out
}

// UsageByAccount maps each slot to what a switch decision may act on.
func UsageByAccount(entries map[string]usagestore.Entry) map[string]*usage.Result {
	out := make(map[string]*usage.Result, len(entries))
	for num, entry := range entries {
		if decision, known := entry.DecisionValue(); known {
			out[num] = decision.Usage
		}
	}
	return out
}

func sortedKeys[T any](m map[string]T) []string {
	return slices.Sorted(maps.Keys(m))
}
