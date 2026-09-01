package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/d0lim/aaswap/internal/usage"
)

// Bar-fill thresholds. A bar is read at a glance, so the color has to carry
// the verdict before the number is read: green while there is room, yellow
// once the account is worth watching, red once it is nearly spent.
const (
	warnPct   = 60.0
	dangerPct = 85.0
)

// Bar glyphs. Eighth-blocks give a bar sub-cell resolution, so a 3% window is
// visibly non-empty on a narrow terminal instead of rounding away to nothing —
// "some quota" and "no quota" are different states and must not render alike.
const (
	barFull  = '█'
	barEmpty = '░'
)

var barPartials = []rune{' ', '▏', '▎', '▍', '▌', '▋', '▊', '▉'}

// renderBar draws a proportional bar of the given cell width.
//
// pct is clamped to 0..100: the API has been seen to report over 100 on a
// window that is fully spent, and a bar longer than its own track would break
// the column alignment of every row beneath it.
func renderBar(pct float64, width int) string {
	if width <= 0 {
		return ""
	}
	pct = min(max(pct, 0), 100)

	eighths := int(pct / 100 * float64(width) * 8)
	full := eighths / 8
	remainder := eighths % 8

	var b strings.Builder
	b.Grow(width * 4)
	for range full {
		b.WriteRune(barFull)
	}
	empty := width - full
	if remainder > 0 && empty > 0 {
		b.WriteRune(barPartials[remainder])
		empty--
	}
	for range empty {
		b.WriteRune(barEmpty)
	}
	return b.String()
}

// barStyle picks the color a fill level calls for.
func (s styles) barStyle(pct float64) lipglossStyle {
	switch {
	case pct >= dangerPct:
		return s.red
	case pct >= warnPct:
		return s.yellow
	default:
		return s.green
	}
}

// windowRow is one rendered rate-limit window: "5h ███░░░ 62%  resets 20:39".
//
// The label is padded to a fixed width so the bars of the 5-hour and 7-day
// rows start in the same column — an eye scanning a list compares bar lengths,
// which only works if they share an origin.
func (m Model) windowRow(label string, window *usage.Window, width int) string {
	st := m.styles
	if window == nil {
		return st.muted.Render(fmt.Sprintf("%-3s %s", label, "—"))
	}
	bar := renderBar(window.Pct, width)
	row := fmt.Sprintf("%-3s %s %3.0f%%",
		label, st.barStyle(window.Pct).Render(bar), window.Pct)

	if reset, ok := window.ResetTime(); ok {
		if note := resetNote(reset, m.now()); note != "" {
			row += st.muted.Render("   " + note)
		}
	}
	return row
}

// resetNote is the human form of a window's reset instant: the wall clock when
// it lands today, the date when it does not.
//
// A reset already in the past reads as "due" rather than a stale timestamp —
// the API has simply not rolled the window over yet, and showing yesterday's
// clock time invites the reader to think the data is broken.
func resetNote(reset, now time.Time) string {
	if !reset.After(now) {
		return "resets due"
	}
	reset = reset.In(now.Location())
	if reset.YearDay() == now.YearDay() && reset.Year() == now.Year() {
		return "resets " + reset.Format("15:04")
	}
	return "resets " + reset.Format("Jan 2 15:04")
}
