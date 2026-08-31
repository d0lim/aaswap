// Package jsonout is the --json contract.
//
// Every structured payload is defined here so the list, status and switch
// surfaces cannot drift apart on a field name, and so the one thing scripts
// depend on — the shape — has a single place to change.
//
// # The rules the shape follows
//
// camelCase throughout, matching the export envelope. [SchemaVersion] is bumped
// only on a BREAKING change, so additive fields appear without warning and a
// consumer must ignore what it does not recognize. A field that is absent and a
// field that is null mean different things and both occur.
//
// The `usage` field is DECISION-GRADE: a measurement appears there only while
// it is fresh enough to act on. An older one is still reported, under
// `lastGoodUsage`, because showing it is a display affordance — but a script
// keying on `usageStatus == "ok"` must never act on arbitrarily old data.
package jsonout

import (
	"math"
	"time"

	"github.com/realiti4/claude-swap/internal/pace"
	"github.com/realiti4/claude-swap/internal/usage"
)

// SchemaVersion is the payload contract's version. Bumped only on a breaking
// change to any shape; scripts key off it.
const SchemaVersion = 1

// Usage status values. Each names a distinct reason a measurement is or is not
// available, because the remedies differ: an expired token retries itself, a
// dead lineage needs a re-login, and an unreadable Keychain needs a GUI
// session.
const (
	StatusOK                  = "ok"
	StatusTokenExpired        = "token_expired"
	StatusAPIKey              = "api_key"
	StatusKeychainUnavailable = "keychain_unavailable"
	StatusReloginRequired     = "relogin_required"
	StatusForeignCredential   = "foreign_credential"
	StatusNoCredentials       = "no_credentials"
	// StatusUnavailable is a fetch that failed, or a measurement too old to
	// decide on.
	StatusUnavailable = "unavailable"
)

// Window is one rate-limit window's projection.
//
// Countdown and Clock are RECOMPUTED at serialization time rather than carried
// from the fetch: the store may serve a measurement hours after it was taken,
// and a cached countdown would understate the remaining wait by exactly that
// gap.
type Window struct {
	Pct       float64 `json:"pct"`
	ResetsAt  string  `json:"resetsAt,omitzero"`
	Countdown string  `json:"countdown,omitzero"`
	Clock     string  `json:"clock,omitzero"`

	// Pace fields, on weekly windows only. A five-hour window has no weekly
	// cycle to be ahead of.
	ExpectedPct *float64 `json:"expectedPct,omitzero"`
	AheadOfPace *bool    `json:"aheadOfPace,omitzero"`
	// ProjectedExhaustionAt is a LINEAR extrapolation, with wide error bars
	// against real bursty usage. It appears here and on no human-facing
	// surface, deliberately.
	ProjectedExhaustionAt string `json:"projectedExhaustionAt,omitzero"`
	WillLastToReset       *bool  `json:"willLastToReset,omitzero"`

	// Name carries a scoped window's model.
	Name string `json:"name,omitzero"`
}

// Spend is the pay-as-you-go axis.
type Spend struct {
	Used      float64 `json:"used"`
	Limit     float64 `json:"limit"`
	Pct       float64 `json:"pct"`
	Currency  string  `json:"currency"`
	ResetsAt  string  `json:"resetsAt,omitzero"`
	Countdown string  `json:"countdown,omitzero"`
	Clock     string  `json:"clock,omitzero"`
}

// Usage is a measurement's projection. Sub-objects appear only when the
// endpoint reported them.
type Usage struct {
	FiveHour *Window  `json:"fiveHour,omitzero"`
	SevenDay *Window  `json:"sevenDay,omitzero"`
	Spend    *Spend   `json:"spend,omitzero"`
	Scoped   []Window `json:"scoped,omitzero"`
}

// ProjectUsage converts a measurement to its JSON shape.
//
// fetchedAt adds the pace fields to the WEEKLY windows only — the seven-day one
// and each scoped one. now drives the recomputed countdown and clock.
func ProjectUsage(result *usage.Result, fetchedAt, now time.Time) *Usage {
	if result == nil {
		return nil
	}
	out := &Usage{}
	if result.FiveHour != nil {
		w := projectWindow(result.FiveHour.Pct, result.FiveHour.ResetsAt, now)
		out.FiveHour = &w
	}
	if result.SevenDay != nil {
		w := projectWindow(result.SevenDay.Pct, result.SevenDay.ResetsAt, now)
		addPace(&w, fetchedAt)
		out.SevenDay = &w
	}
	if result.Spend != nil {
		spend := Spend{
			Used: result.Spend.Used, Limit: result.Spend.Limit,
			Pct: result.Spend.Pct, Currency: result.Spend.Currency,
			ResetsAt: result.Spend.ResetsAt,
		}
		if countdown, clock, ok := usage.FormatReset(result.Spend.ResetsAt, now); ok {
			spend.Countdown, spend.Clock = countdown, clock
		}
		out.Spend = &spend
	}
	for _, scoped := range result.Scoped {
		w := projectWindow(scoped.Pct, scoped.ResetsAt, now)
		addPace(&w, fetchedAt)
		w.Name = scoped.Name
		out.Scoped = append(out.Scoped, w)
	}
	return out
}

func projectWindow(pct float64, resetsAt string, now time.Time) Window {
	w := Window{Pct: pct, ResetsAt: resetsAt}
	if countdown, clock, ok := usage.FormatReset(resetsAt, now); ok {
		w.Countdown, w.Clock = countdown, clock
	}
	return w
}

// addPace layers the weekly pace fields onto a window, when they are computable
// and not suppressed.
func addPace(w *Window, fetchedAt time.Time) {
	if fetchedAt.IsZero() || w.ResetsAt == "" {
		return
	}
	reset, ok := usage.ParseReset(w.ResetsAt)
	if !ok {
		return
	}
	result, ok := pace.Compute(pace.Window{Pct: w.Pct, ResetsAt: reset, Valid: true}, fetchedAt, pace.Options{})
	if !ok {
		return
	}
	expected := round1(result.ExpectedPct)
	ahead := result.Ahead
	w.ExpectedPct = &expected
	w.AheadOfPace = &ahead
	if eta, ok := pace.ProjectedExhaustion(result, fetchedAt); ok {
		w.ProjectedExhaustionAt = Timestamp(eta)
	}
	if lasts, ok := pace.WillLastToReset(result); ok {
		w.WillLastToReset = &lasts
	}
}

// Decision is what a payload reports about one account's usage: either a
// measurement, or a sentinel naming why there is none.
type Decision struct {
	Usage    *usage.Result
	Sentinel string
}

// UsageFields maps a collected decision to its status and projection.
func UsageFields(decision Decision, fetchedAt, now time.Time) (string, *Usage) {
	if decision.Usage != nil {
		return StatusOK, ProjectUsage(decision.Usage, fetchedAt, now)
	}
	switch decision.Sentinel {
	case "":
		return StatusUnavailable, nil
	case "token expired":
		return StatusTokenExpired, nil
	case "api key":
		return StatusAPIKey, nil
	case "keychain unavailable":
		return StatusKeychainUnavailable, nil
	case "re-login needed":
		return StatusReloginRequired, nil
	case "foreign credential":
		return StatusForeignCredential, nil
	}
	// An unrecognized sentinel is still a stated reason, not a fetch failure.
	return StatusNoCredentials, nil
}

// AccountRef is a minimal reference, used for a switch's two sides.
//
// Number is a pointer because null is meaningful: the live login was not one
// cswap manages, so there is no slot to name.
type AccountRef struct {
	Number *int   `json:"number"`
	Email  string `json:"email"`
}

// AccountRow is one account in a listing.
type AccountRow struct {
	Number           int    `json:"number"`
	Email            string `json:"email"`
	OrganizationName string `json:"organizationName"`
	OrganizationUUID string `json:"organizationUuid"`
	IsOrganization   bool   `json:"isOrganization"`
	Active           bool   `json:"active"`
	UsageStatus      string `json:"usageStatus"`
	Usage            *Usage `json:"usage"`

	Alias string `json:"alias,omitzero"`
	// Disabled appears only when the slot is held out of rotation, so a
	// consumer keying on the base shape is unaffected.
	Disabled bool `json:"disabled,omitzero"`

	// Freshness for a served measurement.
	UsageFetchedAt  string   `json:"usageFetchedAt,omitzero"`
	UsageAgeSeconds *float64 `json:"usageAgeSeconds,omitzero"`

	// Display-grade last-known-good, for a row whose measurement is too old to
	// decide on. Kept separate from Usage precisely so the two cannot be
	// confused.
	LastGoodUsage      *Usage   `json:"lastGoodUsage,omitzero"`
	LastGoodFetchedAt  string   `json:"lastGoodFetchedAt,omitzero"`
	LastGoodAgeSeconds *float64 `json:"lastGoodAgeSeconds,omitzero"`
}

// SetFreshness fills in whichever freshness fields match the row's status.
func (r *AccountRow) SetFreshness(lastGood *usage.Result, fetchedAt time.Time, age time.Duration, now time.Time) {
	if fetchedAt.IsZero() {
		return
	}
	seconds := round1(age.Seconds())
	if r.Usage != nil {
		r.UsageFetchedAt = Timestamp(fetchedAt)
		r.UsageAgeSeconds = &seconds
		return
	}
	if lastGood == nil {
		return
	}
	r.LastGoodUsage = ProjectUsage(lastGood, fetchedAt, now)
	r.LastGoodFetchedAt = Timestamp(fetchedAt)
	r.LastGoodAgeSeconds = &seconds
}

// ListPayload is `cswap list --json`.
type ListPayload struct {
	SchemaVersion       int          `json:"schemaVersion"`
	ActiveAccountNumber *int         `json:"activeAccountNumber"`
	Accounts            []AccountRow `json:"accounts"`

	// Additive, and absent when clean: the JSON contract keeps stdout a single
	// machine-readable object, so warnings ride here rather than being printed.
	DuplicateAccountWarnings []string `json:"duplicateAccountWarnings,omitzero"`
	LockstepUsageWarnings    []string `json:"lockstepUsageWarnings,omitzero"`
	UnclaimedCredentials     []string `json:"unclaimedCredentials,omitzero"`
}

// ActiveStatus describes the live login.
type ActiveStatus struct {
	Number           *int   `json:"number,omitzero"`
	Email            string `json:"email"`
	OrganizationName string `json:"organizationName,omitzero"`
	OrganizationUUID string `json:"organizationUuid,omitzero"`
	IsOrganization   bool   `json:"isOrganization,omitzero"`
	Managed          bool   `json:"managed"`
	Alias            string `json:"alias,omitzero"`

	UsageStatus string `json:"usageStatus,omitzero"`
	Usage       *Usage `json:"usage,omitzero"`

	UsageFetchedAt  string   `json:"usageFetchedAt,omitzero"`
	UsageAgeSeconds *float64 `json:"usageAgeSeconds,omitzero"`

	LastGoodUsage      *Usage   `json:"lastGoodUsage,omitzero"`
	LastGoodFetchedAt  string   `json:"lastGoodFetchedAt,omitzero"`
	LastGoodAgeSeconds *float64 `json:"lastGoodAgeSeconds,omitzero"`
}

// StatusPayload is `cswap status --json`.
//
// Active is a pointer because null is the answer on a machine with no live
// login at all — distinct from an unmanaged one, which reports an address with
// managed false.
type StatusPayload struct {
	SchemaVersion        int           `json:"schemaVersion"`
	Active               *ActiveStatus `json:"active"`
	TotalManagedAccounts *int          `json:"totalManagedAccounts,omitzero"`
}

// SwitchPayload is `cswap switch --json`.
type SwitchPayload struct {
	SchemaVersion int         `json:"schemaVersion"`
	Switched      bool        `json:"switched"`
	From          *AccountRef `json:"from"`
	To            AccountRef  `json:"to"`
	Warnings      []string    `json:"warnings,omitzero"`
}

// ErrorEnvelope is what a handled failure emits.
//
// A single object on stdout either way, so a consumer parses one shape and
// branches on the presence of `error` rather than on the exit code alone.
type ErrorEnvelope struct {
	SchemaVersion int   `json:"schemaVersion"`
	Error         Error `json:"error"`
}

// Error names a failure.
type Error struct {
	// Type is a stable kind, not a Go type name: the taxonomy is the contract,
	// and renaming an internal type must not break a consumer.
	Type    string `json:"type"`
	Message string `json:"message"`
}

// Timestamp renders an instant the way every payload field does: UTC, second
// resolution, Z-suffixed.
func Timestamp(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05Z")
}

// round1 rounds to one decimal, which is the resolution every percentage and
// age in this contract is reported at.
func round1(f float64) float64 {
	return math.Round(f*10) / 10
}
