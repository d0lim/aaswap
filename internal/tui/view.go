package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/d0lim/ccswap/internal/render"
	"github.com/d0lim/ccswap/internal/swap"
	"github.com/d0lim/ccswap/internal/usagestore"
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
	case m.err != nil:
		sections = append(sections, m.styles.red.Render("  "+m.err.Error()))
	case m.snapshot == nil:
		sections = append(sections, m.styles.muted.Render("  collecting…"))
	case len(m.snapshot.Views) == 0:
		sections = append(sections,
			m.styles.muted.Render("  No accounts are managed yet."),
			m.styles.muted.Render("  Log in with Claude Code, then quit and run: ")+
				m.styles.accent.Render("ccswap add"))
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
	left := st.title.Render(" ccswap")

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
	entry := m.snapshot.Entries[view.Number]

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

	head := fmt.Sprintf("%s%s %s %s", cursor, marker, st.slot.Render(view.Number), email)
	if alias := view.Account.Alias; alias != "" {
		head += st.muted.Render("  (" + alias + ")")
	}
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

	hints := [][2]string{
		{"↑↓", "move"}, {"enter", "switch"}, {"d", "disable"},
		{"r", "refresh"}, {"w", "watch"}, {"?", "help"}, {"q", "quit"},
	}
	var parts []string
	for _, h := range hints {
		parts = append(parts, st.helpKey.Render(h[0])+st.help.Render(" "+h[1]))
	}
	return strings.Join(append(lines, " "+strings.Join(parts, st.help.Render("  ·  "))), "\n")
}

// renderHelp is the full key reference, which the footer only samples.
func (m Model) renderHelp() string {
	st := m.styles
	rows := [][2]string{
		{"↑ / k", "move up"},
		{"↓ / j", "move down"},
		{"enter / s", "switch to the selected account"},
		{"d", "disable or enable the selected account"},
		{"r", "collect now"},
		{"w", "toggle watch mode (re-collect every 30s)"},
		{"?", "close this help"},
		{"q / esc", "quit"},
	}
	var b strings.Builder
	b.WriteString(st.title.Render("Keys"))
	for _, row := range rows {
		b.WriteString("\n" + st.helpKey.Render(fmt.Sprintf("%-10s", row[0])) + st.help.Render(row[1]))
	}
	b.WriteString("\n\n" + st.muted.Render(
		"Switching writes a live credential. It takes the store lock, so it may\n"+
			"pause while another ccswap or a running Claude Code holds it."))
	return st.modal.Render(b.String())
}
