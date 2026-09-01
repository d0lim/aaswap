package usage

import (
	"fmt"
	"time"
)

// FormatReset renders a window's reset timestamp as a countdown and a
// wall-clock string, reporting false when the timestamp is absent or
// unparseable.
//
// Both strings are derived from now rather than cached at fetch time. That is
// the whole point: a countdown frozen two hours ago overstates the remaining
// wait by those two hours, and a same-day "15:30" silently starts meaning
// yesterday. See the package doc for why neither is persisted.
//
// The countdown coarsens as the wait grows — "2d 6h", "6h 15m", "45m" — because
// nobody schedules around the seconds of a reset three hours out, and a reset
// already in the past reads as "0m" rather than going negative.
//
// The clock is rendered in now's location, on the assumption that the caller
// passes a local now: reset times are only ever read against the reader's own
// day. It shows the time alone when the reset lands today and prefixes the date
// otherwise, so "08:59" is never silently tomorrow's.
func FormatReset(resetsAt string, now time.Time) (countdown, clock string, ok bool) {
	reset, ok := ParseReset(resetsAt)
	if !ok {
		return "", "", false
	}
	return Countdown(reset, now), ResetClock(reset, now), true
}

// Countdown renders how long until reset, floored at zero.
func Countdown(reset, now time.Time) string {
	remaining := max(reset.Sub(now), 0)
	days := int(remaining / (24 * time.Hour))
	hours := int(remaining/time.Hour) % 24
	minutes := int(remaining/time.Minute) % 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

// ResetClock renders the absolute reset time in now's location: "20:39" when it
// falls on the same day, "Jul 5 08:59" otherwise.
func ResetClock(reset, now time.Time) string {
	local := reset.In(now.Location())
	ly, lm, ld := local.Date()
	ny, nm, nd := now.Date()
	if ly == ny && lm == nm && ld == nd {
		return local.Format("15:04")
	}
	// "Jan 2" and not "Jan 02": the day is unpadded, matching how a person
	// would say it.
	return local.Format("Jan 2 15:04")
}
