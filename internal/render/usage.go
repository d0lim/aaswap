package render

import (
	"fmt"
	"strings"
	"time"

	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/pace"
	"github.com/d0lim/aaswap/internal/usage"
	"github.com/d0lim/aaswap/internal/usagestore"
)

// sentinelNotes explain a derived state in the terms a user can act on.
//
// Shared wording, so every surface describes a state identically. An
// owned-and-expired token means Claude Code will refresh it, NOT that the user
// must log in again — and a note that gets that backwards sends people to
// re-authenticate for nothing.
var sentinelNotes = map[string]string{
	"token expired":        "token expired — the refresh was deferred this pass; it retries automatically",
	"foreign credential":   "the live credential belongs to another account — a switch repairs it",
	"api key":              "API key (no quota)",
	"keychain unavailable": "keychain unavailable — locked or in use; try again",
	"re-login needed":      "re-login needed — the refresh token is dead; log in with Claude Code, then run: aaswap add",
}

// SentinelNote explains a sentinel, falling back to the sentinel itself so an
// unrecognized one still says something.
func SentinelNote(sentinel string) string {
	if note, ok := sentinelNotes[sentinel]; ok {
		return note
	}
	return sentinel
}

// UsageLines renders a measurement as aligned rows.
//
// Labels are padded to the widest one, so a per-model name does not shift the
// columns of the account-wide lines it sits beside.
func UsageLines(result *usage.Result, fetchedAt, now time.Time) []string {
	if result == nil {
		return nil
	}
	type row struct{ label, body string }
	var rows []row

	if spend := result.Spend; spend != nil {
		if _, clock, ok := usage.FormatReset(spend.ResetsAt, now); ok {
			rows = append(rows, row{"$$", fmt.Sprintf("%3.0f%%   resets %-12s  %s / %s",
				spend.Pct, clock, money(spend.Used, spend.Currency), money(spend.Limit, spend.Currency))})
		} else {
			rows = append(rows, row{"$$", fmt.Sprintf("%3.0f%%   %s / %s",
				spend.Pct, money(spend.Used, spend.Currency), money(spend.Limit, spend.Currency))})
		}
	}

	if w := result.FiveHour; w != nil {
		// Pace applies to the WEEKLY window only: a five-hour window has no
		// weekly cycle to be ahead of.
		rows = append(rows, row{"5h", windowBody(w.Pct, w.ResetsAt, "", now)})
	}
	if w := result.SevenDay; w != nil {
		rows = append(rows, row{"7d", windowBody(w.Pct, w.ResetsAt, paceMarker(w.Pct, w.ResetsAt, fetchedAt), now)})
	}
	for _, scoped := range result.Scoped {
		// A model at its limit is the usual reason to switch, so it is flagged
		// outright rather than left to the percentage.
		marker := paceMarker(scoped.Pct, scoped.ResetsAt, fetchedAt)
		if scoped.Pct >= 100 {
			marker = "  (!)"
		}
		rows = append(rows, row{scoped.Name, windowBody(scoped.Pct, scoped.ResetsAt, marker, now)})
	}

	width := 0
	for _, r := range rows {
		width = max(width, len(r.label))
	}
	width++ // the colon

	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = fmt.Sprintf("%-*s %s", width, r.label+":", r.body)
	}
	return out
}

func windowBody(pct float64, resetsAt, marker string, now time.Time) string {
	countdown, clock, ok := usage.FormatReset(resetsAt, now)
	if !ok {
		return fmt.Sprintf("%3.0f%%%s", pct, marker)
	}
	return fmt.Sprintf("%3.0f%%   resets %-12s  in %s%s", pct, clock, countdown, marker)
}

// paceMarker flags a weekly window that is meaningfully ahead of pace.
//
// Meaningfully: the threshold is what keeps the marker off an already-dense row
// for variance that means nothing.
func paceMarker(pct float64, resetsAt string, fetchedAt time.Time) string {
	if fetchedAt.IsZero() || resetsAt == "" {
		return ""
	}
	reset, ok := usage.ParseReset(resetsAt)
	if !ok {
		return ""
	}
	result, ok := pace.Compute(pace.Window{Pct: pct, ResetsAt: reset, Valid: true}, fetchedAt, pace.Options{})
	if !ok || !result.Ahead {
		return ""
	}
	return "  (ahead of pace)"
}

// money renders a spend figure with thousands separators.
func money(amount float64, currency string) string {
	symbol := "$"
	if currency != "" && currency != "USD" {
		symbol = currency + " "
	}
	return symbol + group(fmt.Sprintf("%.2f", amount))
}

// group inserts thousands separators into a fixed-point decimal string.
func group(text string) string {
	whole, frac, _ := strings.Cut(text, ".")
	sign := ""
	if rest, cut := strings.CutPrefix(whole, "-"); cut {
		sign, whole = "-", rest
	}
	var parts []string
	for len(whole) > 3 {
		parts = append([]string{whole[len(whole)-3:]}, parts...)
		whole = whole[:len(whole)-3]
	}
	parts = append([]string{whole}, parts...)
	return sign + strings.Join(parts, ",") + "." + frac
}

// EntryLines renders one account's usage state, sentinel or measurement.
//
// A sentinel renders its note first, with a supplementary "last seen" line when
// an older measurement exists — the state explains why there is no number, and
// the older number is still worth showing. An account with no measurement at
// all says so plus the last error, so a failing endpoint is visible instead of
// a silent blank.
func EntryLines(entry usagestore.Entry, now time.Time, ageNote time.Duration) []string {
	if entry.Sentinel != "" {
		out := []string{SentinelNote(entry.Sentinel)}
		// An API-key account has no quota to have last seen.
		if note, ok := LastSeenNote(entry, now); ok && entry.Sentinel != "api key" {
			out = append(out, note)
		}
		return out
	}

	if entry.LastGood != nil {
		lines := UsageLines(entry.LastGood, entry.FetchedAt, now)
		if len(lines) > 0 && entry.Age > ageNote && !entry.FetchedAt.IsZero() {
			lines[len(lines)-1] += " · " + Age(entry.Age)
		}
		return lines
	}

	detail := "usage unavailable"
	if entry.LastError != "" {
		detail += " (" + claudeapi.Note(entry.LastError) + ")"
	}
	return []string{detail}
}

// LastSeenNote summarizes an entry's last measurement in one line.
func LastSeenNote(entry usagestore.Entry, now time.Time) (string, bool) {
	if entry.LastGood == nil || entry.FetchedAt.IsZero() {
		return "", false
	}
	headroom, ok := entry.LastGood.Headroom(nil)
	if !ok {
		return "", false
	}
	return fmt.Sprintf("last seen %.0f%% used · %s", 100-headroom, Age(entry.Age)), true
}

// Age renders how long ago something happened, coarsening as it recedes.
func Age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
