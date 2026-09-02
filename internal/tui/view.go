package tui

import (
	"fmt"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/render"
	"github.com/d0lim/aaswap/internal/swap"
	"github.com/d0lim/aaswap/internal/usagestore"
)

// Layout constants.
//
// chromeCols is every fixed column a usage row spends outside the bar itself:
// the indent, the "5h" label, the percentage, and the widest reset note
// ("resets Jul 5 22:12"). The bar takes what is left, so the note stays inside
// the frame on a narrow terminal instead of wrapping and shearing the row.
//
// maxBarWidth caps it well short of a wide terminal's full width. The bar is
// there to be compared against the bar above it, and past a couple of dozen
// cells extra length adds no resolution a reader can use.
const (
	minBarWidth = 8
	maxBarWidth = 24
	chromeCols  = 38
	// noteIndent is how far a usage or sentinel line sits under its account.
	noteIndent = 5
)

// View renders one frame.
func (m Model) View() tea.View {
	view := tea.NewView("")
	view.AltScreen = true
	if m.quitting {
		// Leave the alt screen empty on the way out: the shell should get its
		// scrollback back, not a copy of the dashboard.
		return view
	}

	body := m.dashboard()
	switch {
	case m.showHelp:
		view.Content = m.overlay(body, m.renderHelp())
	case m.modal != nil:
		view.Content = m.overlay(body, m.renderModal(m.modal))
	default:
		view.Content = body
	}
	return view
}

// dashboard is the account list with its header and footer.
func (m Model) dashboard() string {
	sections := []string{m.header()}

	switch {
	case !m.pointed():
		sections = append(sections, m.styles.muted.Render("  choosing a tool…"))
	case m.err != nil:
		sections = append(sections, m.styles.red.Render("  "+m.err.Error()))
	case m.snapshot == nil:
		sections = append(sections, m.styles.muted.Render("  collecting…"))
	case len(m.snapshot.Views) == 0:
		sections = append(sections,
			m.styles.muted.Render("  No accounts are managed yet."),
			m.styles.muted.Render("  Press ")+m.styles.accent.Render("a")+
				m.styles.muted.Render(" to add the account you are logged in as, or ")+
				m.styles.accent.Render("n")+m.styles.muted.Render(" to wait for a /login."))
	default:
		sections = append(sections, m.accountList())
	}

	sections = append(sections, m.footer())
	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

// header carries the identity of the screen and the two facts that are true of
// the whole store rather than of any one account.
func (m Model) header() string {
	st := m.styles
	left := st.title.Render(" aaswap")
	if m.pointed() {
		left += st.muted.Render(" · ") + st.accent.Render(m.spec.DisplayName())
	}

	var right []string
	if m.watch {
		right = append(right, st.green.Render("watch on"))
	}
	if m.busy != "" {
		right = append(right, st.accent.Render(m.busy+"…"))
	}
	if len(right) == 0 && m.snapshot != nil {
		right = append(right, st.muted.Render(fmt.Sprintf("%d managed", len(m.snapshot.Views))))
	}

	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(strings.Join(right, "  "))-1, 1)
	return left + strings.Repeat(" ", gap) + strings.Join(right, "  ") + "\n"
}

// accountList is one block per slot.
func (m Model) accountList() string {
	var blocks []string
	for i, view := range m.snapshot.Views {
		blocks = append(blocks, m.accountBlock(i, view))
	}
	// A blank line between blocks. Each account is three or four lines that
	// mean nothing to each other across the boundary, and run together they
	// read as one long table where a 7d row could belong to the account above.
	return strings.Join(blocks, "\n\n")
}

// accountBlock renders one account: its identity line, then either its usage
// windows or the single reason it has none.
func (m Model) accountBlock(index int, view swap.AccountView) string {
	st := m.styles
	entry := m.snapshot.Entries[view.Name]

	cursor := "  "
	if index == m.cursor {
		cursor = st.accent.Render("▸ ")
	}

	marker := st.muted.Render("○")
	switch {
	case view.IsActive:
		marker = st.accent.Render("●")
	case view.Account.Disabled:
		marker = st.muted.Render("⊘")
	}

	email := st.email.Render(view.Account.Email)
	if view.IsActive {
		email = st.emailOn.Render(view.Account.Email)
	}

	head := fmt.Sprintf("%s%s %s %s", cursor, marker, st.slot.Render(view.Name), email)
	if view.Account.Disabled {
		head += st.muted.Render("  out of rotation")
	}
	if tag := view.Account.DisplayTag(); tag != "" {
		head = m.padTo(head, tag)
	}

	lines := []string{head}
	for _, line := range m.usageLines(entry) {
		lines = append(lines, strings.Repeat(" ", noteIndent)+line)
	}
	return strings.Join(lines, "\n")
}

// padTo right-aligns a trailing tag on the same row as the head.
func (m Model) padTo(head, tag string) string {
	styled := m.styles.tag.Render(tag)
	gap := m.width - lipgloss.Width(head) - lipgloss.Width(styled) - 1
	if gap < 1 {
		return head + " " + styled
	}
	return head + strings.Repeat(" ", gap) + styled
}

// usageLines is an account's usage, or the one reason it has none.
//
// A sentinel replaces the bars entirely rather than sitting beside them: "this
// slot has no quota to report" and "this slot's quota is X" are different
// claims, and showing an empty bar next to "api key" invites reading it as
// zero usage.
func (m Model) usageLines(entry usagestore.Entry) []string {
	st := m.styles
	if entry.Sentinel != "" {
		style := st.muted
		if entry.Sentinel == swap.SentinelReloginRequired {
			style = st.red
		}
		// Wrapped, not truncated: a sentinel is the whole explanation of why
		// this slot shows no usage, and the half that gets cut is the half
		// that says what to do about it.
		return strings.Split(
			style.Width(max(m.width-noteIndent-1, 20)).Render(render.SentinelNote(entry.Sentinel)),
			"\n")
	}
	if entry.LastGood == nil {
		return []string{st.muted.Render("no measurement yet")}
	}

	width := min(max(m.width-chromeCols, minBarWidth), maxBarWidth)
	lines := []string{
		m.windowRow("5h", entry.LastGood.FiveHour, width),
		m.windowRow("7d", entry.LastGood.SevenDay, width),
	}
	// The age note only when the reading is old enough to caveat. On fresh
	// data it restates the percentage the bar just drew, and a line that says
	// nothing new trains the eye to skip the line that will.
	if entry.Age > usagestore.StaleOK {
		if note, ok := render.LastSeenNote(entry, m.now()); ok {
			lines = append(lines, st.muted.Render(note))
		}
	}
	return lines
}

// footer is the status line and the key hints.
func (m Model) footer() string {
	st := m.styles
	var lines []string

	if m.status != "" {
		style := st.green
		if m.statusErr {
			style = st.red
		}
		lines = append(lines, "\n "+style.Render(m.status))
	} else {
		lines = append(lines, "")
	}

	return strings.Join(append(lines, " "+m.hintBar()), "\n")
}

// footerHints are the keys the bar gives up when it has to, in the order it
// gives them up: the last is the first to go.
//
// The bar is one line, and there are more keys than fit an eighty-column
// terminal. Rather than wrapping — which pushes an account row off a short
// screen — it drops hints until what is left fits.
var footerHints = [][2]string{
	{"↑↓", "move"}, {"enter", "switch"}, {"a", "add"},
	{"t", "token"}, {"d", "disable"}, {"r", "refresh"}, {"w", "watch"},
}

// pinnedHints survive any width.
//
// Quit, because a dashboard whose exit is not written down is one people kill
// the terminal to escape; help, because it is where every key shed above can
// still be read.
var pinnedHints = [][2]string{{"q", "quit"}, {"?", "help"}}

// hintBar renders as many hints as fit, keeping the pinned ones whatever
// happens.
func (m Model) hintBar() string {
	st := m.styles
	separator := st.help.Render("  ·  ")
	render := func(hints [][2]string) string {
		parts := make([]string, 0, len(hints)+len(pinnedHints))
		for _, hint := range slices.Concat(hints, pinnedHints) {
			parts = append(parts, st.helpKey.Render(hint[0])+st.help.Render(" "+hint[1]))
		}
		return strings.Join(parts, separator)
	}

	hints := footerHints
	if m.canPickProvider() {
		// Ahead of the rest: which tool the screen is about comes before
		// anything done on it.
		hints = slices.Concat([][2]string{{"p", "tool"}}, hints)
	}
	bar := render(hints)
	for shown := len(hints); shown > 0 && lipgloss.Width(bar) > m.width-1; shown-- {
		bar = render(hints[:shown-1])
	}
	return bar
}

// renderHelp is the full key reference, which the footer only samples.
func (m Model) renderHelp() string {
	st := m.styles
	tool := m.spec.DisplayName()
	rows := [][2]string{
		{"↑ / k", "move up"},
		{"↓ / j", "move down"},
		{"enter / s", "switch to the selected account"},
		{"a", "add the account you are logged in as"},
		{"n", "wait for a login, then add that account"},
	}
	// Listed only where it works. A key on the reference that reports
	// "unsupported" when pressed is worse than an absent one: the reference is
	// where someone looks to find out what the dashboard can do.
	if m.spec.Can(provider.CapToken) {
		rows = append(rows, [2]string{"t", "add a setup token or managed API key"})
	}
	if m.canPickProvider() {
		rows = append(rows, [2]string{"p", "show another tool's accounts"})
	}
	rows = append(rows,
		[2]string{"d", "disable or enable the selected account"},
		[2]string{"r", "collect now"},
		[2]string{"w", "toggle watch mode (re-collect every 30s)"},
		[2]string{"?", "close this help"},
		[2]string{"q / esc", "quit"},
	)
	var b strings.Builder
	b.WriteString(st.title.Render("Keys"))
	for _, row := range rows {
		b.WriteString("\n" + st.helpKey.Render(fmt.Sprintf("%-10s", row[0])) + st.help.Render(row[1]))
	}
	b.WriteString("\n\n" + st.muted.Render(fmt.Sprintf(
		"Switching writes a live credential. It takes the store lock, so it may\n"+
			"pause while another aaswap or a running %s holds it.\n\n"+
			"aaswap cannot log you in — %s owns that flow. `n` waits for you to\n"+
			"log in elsewhere and captures the account when it lands.", tool, tool)))
	return st.modal.Render(b.String())
}
