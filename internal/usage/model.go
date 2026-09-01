// Package usage holds the normalized shape of an account's rate-limit usage,
// and the one canonical answer to which windows actually gate that account.
//
// The types here are the on-disk shape too: the usage store persists a
// [Result] as a slot's last-known-good measurement, and both the Python
// implementation and this one must read each other's files during the
// migration. Field names and JSON spelling are therefore a contract, not a
// style choice.
//
// # What is not stored
//
// The API's reset timestamps are kept as the raw ISO strings they arrived as,
// and the human-facing countdown and clock strings are NOT persisted. Those are
// recomputed at render time, because strings cached at fetch time drift as the
// measurement ages: a countdown frozen two hours ago overstates the remaining
// wait by those two hours, and a same-day "15:30" clock silently starts meaning
// yesterday.
package usage

import (
	"slices"
	"strings"
	"time"
)

// Window is one rate-limit window: how much of it is used, and when it resets.
type Window struct {
	Pct float64 `json:"pct"`
	// ResetsAt is the ISO timestamp exactly as the API sent it, or empty when
	// it sent none. Kept verbatim so a value written by either implementation
	// round-trips unchanged.
	ResetsAt string `json:"resets_at,omitempty"`
}

// ResetTime parses ResetsAt, reporting false when it is absent or unparseable.
func (w Window) ResetTime() (time.Time, bool) { return ParseReset(w.ResetsAt) }

// Scoped is a per-model weekly window, named by the model's display name.
//
// These live in the newer limits array the API returns and are invisible to the
// legacy five-hour and seven-day keys, so each is surfaced separately.
type Scoped struct {
	Name     string  `json:"name"`
	Pct      float64 `json:"pct"`
	ResetsAt string  `json:"resets_at,omitempty"`
}

// ResetTime parses ResetsAt, reporting false when it is absent or unparseable.
func (s Scoped) ResetTime() (time.Time, bool) { return ParseReset(s.ResetsAt) }

// Spend is the pay-as-you-go extra-usage axis: credits consumed against a
// monthly limit. It is deliberately NOT a gating window — see [Result.Windows].
type Spend struct {
	Used     float64 `json:"used"`
	Limit    float64 `json:"limit"`
	Pct      float64 `json:"pct"`
	Currency string  `json:"currency,omitempty"`
	ResetsAt string  `json:"resets_at,omitempty"`
}

// Result is one account's normalized usage.
type Result struct {
	FiveHour *Window  `json:"five_hour,omitempty"`
	SevenDay *Window  `json:"seven_day,omitempty"`
	Spend    *Spend   `json:"spend,omitempty"`
	Scoped   []Scoped `json:"scoped,omitempty"`
}

// Empty reports whether the result carries no usable data at all.
func (r *Result) Empty() bool {
	return r == nil || (r.FiveHour == nil && r.SevenDay == nil && r.Spend == nil && len(r.Scoped) == 0)
}

// Relevant is one window that gates an account, flattened for decisions.
type Relevant struct {
	// Label is "5h", "7d", or a model's display name.
	Label    string
	Pct      float64
	ResetsAt string
}

// ResetTime parses ResetsAt, reporting false when it is absent or unparseable.
func (r Relevant) ResetTime() (time.Time, bool) { return ParseReset(r.ResetsAt) }

// AllModels is the sentinel that matches every scoped window an account
// reports, rather than a specific model display name.
const AllModels = "all"

// Windows returns every window that gates this account.
//
// Always the five-hour and seven-day windows. When models is non-empty, each
// named per-model weekly window is included too — matched case-insensitively on
// display name, with [AllModels] matching every scoped window the account
// reports.
//
// This is the single canonical window source for decisions, scheduling and
// reset math, so a window that binds a decision can never be invisible to the
// scheduler. Spend is deliberately excluded: pay-as-you-go credits are a
// separate axis and do not gate requests.
func (r *Result) Windows(models []string) []Relevant {
	if r == nil {
		return nil
	}
	var out []Relevant
	for _, w := range []struct {
		label  string
		window *Window
	}{{"5h", r.FiveHour}, {"7d", r.SevenDay}} {
		if w.window != nil {
			out = append(out, Relevant{Label: w.label, Pct: w.window.Pct, ResetsAt: w.window.ResetsAt})
		}
	}
	if len(models) == 0 {
		return out
	}

	wanted := make([]string, len(models))
	for i, m := range models {
		wanted[i] = strings.ToLower(m)
	}
	matchAll := slices.Contains(wanted, AllModels)
	for _, s := range r.Scoped {
		if s.Name == "" {
			continue
		}
		if matchAll || slices.Contains(wanted, strings.ToLower(s.Name)) {
			out = append(out, Relevant{Label: s.Name, Pct: s.Pct, ResetsAt: s.ResetsAt})
		}
	}
	return out
}

// Headroom returns the remaining percentage before this account hits a
// rate-limit window, and whether that is knowable at all.
//
// It is the headroom of the BINDING window — 100 minus the highest utilization
// — so a result at or below zero means the account is at or over a limit. When
// models is non-empty each named per-model window is folded in: a model maxed
// at 100% blocks that model's work even with five-hour and seven-day headroom,
// so for someone pinned to that model it binds just as hard.
//
// It reports false when usage is unavailable or carries no window data, which
// callers must treat as "unknown" and never as a reason to skip an account.
func (r *Result) Headroom(models []string) (float64, bool) {
	windows := r.Windows(models)
	if len(windows) == 0 {
		return 0, false
	}
	highest := windows[0].Pct
	for _, w := range windows[1:] {
		highest = max(highest, w.Pct)
	}
	return 100.0 - highest, true
}

// BindingPct returns the utilization of the binding — worst — relevant window.
func (r *Result) BindingPct(models []string) (float64, bool) {
	headroom, ok := r.Headroom(models)
	if !ok {
		return 0, false
	}
	return 100.0 - headroom, true
}

// resetLayouts are the timestamp spellings the usage API has been observed to
// send. RFC 3339 covers the normal case; the others accept a value that omits
// the zone or the seconds, which Python's lenient ISO parser accepted and a
// persisted file may therefore still carry.
var resetLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02",
}

// ParseReset parses an API reset timestamp, reporting false when it is absent
// or unparseable.
//
// A value with no zone is read as UTC: every timestamp this endpoint sends is
// UTC, and guessing the local zone would shift a reset by hours.
func ParseReset(resetsAt string) (time.Time, bool) {
	if resetsAt == "" {
		return time.Time{}, false
	}
	for _, layout := range resetLayouts {
		if t, err := time.Parse(layout, resetsAt); err == nil {
			return t, true
		}
	}
	// A trailing "+00:00" spelled as "Z" and vice versa are both covered above;
	// anything left is genuinely unparseable.
	return time.Time{}, false
}
