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

// dashboard is every tool's account list with its header and footer.
func (m Model) dashboard() string {
	sections := []string{m.header()}
	for i := range m.panes {
		sections = append(sections, m.paneSection(i))
	}
	sections = append(sections, m.footer())
	return lipgloss.JoinVertical(lipgloss.Left, sections...)
}

// header carries the identity of the screen and the facts that are true of
// the whole store rather than of any one account.
func (m Model) header() string {
	st := m.styles
	left := st.title.Render(" aaswap")

	var right []string
	switch {
	case m.busy != "":
		right = append(right, st.accent.Render(m.busy+"…"))
	case m.anyCollecting():
		right = append(right, st.accent.Render("collecting…"))
	default:
		right = append(right, st.muted.Render(fmt.Sprintf("%d managed", m.managed())))
	}
	// Said on every frame: a dashboard that refreshes on its own and a
	// dashboard that does not look the same until something changes, and the
	// difference decides whether a person waits or presses r.
	right = append(right, st.green.Render("live"))

	gap := max(m.width-lipgloss.Width(left)-lipgloss.Width(strings.Join(right, "  "))-1, 1)
	return left + strings.Repeat(" ", gap) + strings.Join(right, "  ") + "\n"
}

// paneSection is one tool: its name, then its accounts or the one reason
// there are none.
func (m Model) paneSection(index int) string {
	st := m.styles
	p := m.panes[index]
	title := " " + st.accent.Render(p.spec.DisplayName())
	if p.snapshot != nil {
		title += st.muted.Render("  " + accountsWord(len(p.snapshot.Views)))
	}
	lines := []string{title, ""}

	switch {
	case p.err != nil:
		lines = append(lines, st.red.Render("  "+p.err.Error()))
	case p.snapshot == nil:
		lines = append(lines, st.muted.Render("  collecting…"))
	case len(p.snapshot.Views) == 0:
		lines = append(lines, m.placeholderRow(index))
	default:
		lines = append(lines, m.accountList(index))
	}
	// A blank line after the section, so the next tool's name does not read
	// as another account of this one.
	lines = append(lines, "")
	return strings.Join(lines, "\n")
}

// placeholderRow is where the cursor lands in a tool with nothing stored: the
// keys still need somewhere to point, and the row says which ones apply.
func (m Model) placeholderRow(index int) string {
	st := m.styles
	cursor := "  "
	if m.cursor < len(m.rows) && m.rows[m.cursor].pane == index && m.rows[m.cursor].view < 0 {
		cursor = st.accent.Render("▸ ")
	}
	return cursor + st.muted.Render("No accounts yet. Press ") + st.accent.Render("n") +
		st.muted.Render(" to log in, or ") + st.accent.Render("a") +
		st.muted.Render(" to add the account you are logged in as.")
}

// accountList is one block per slot.
func (m Model) accountList(index int) string {
	var blocks []string
	for i, view := range m.panes[index].snapshot.Views {
		blocks = append(blocks, m.accountBlock(index, i, view))
	}
	// A blank line between blocks. Each account is three or four lines that
	// mean nothing to each other across the boundary, and run together they
	// read as one long table where a 7d row could belong to the account above.
	return strings.Join(blocks, "\n\n")
}

// accountBlock renders one account: its identity line, then either its usage
// windows or the single reason it has none.
func (m Model) accountBlock(paneIndex, index int, view swap.AccountView) string {
	st := m.styles
	entry := m.panes[paneIndex].snapshot.Entries[view.Name]

	cursor := "  "
	if m.cursor < len(m.rows) && m.rows[m.cursor] == (row{pane: paneIndex, view: index, name: view.Name}) {
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
	{"↑↓", "move"}, {"enter", "switch"}, {"n", "log in"}, {"a", "add"},
	{"t", "token"}, {"d", "disable"}, {"r", "refresh"},
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
	bar := render(hints)
	for shown := len(hints); shown > 0 && lipgloss.Width(bar) > m.width-1; shown-- {
		bar = render(hints[:shown-1])
	}
	return bar
}

// renderHelp is the full key reference, which the footer only samples.
func (m Model) renderHelp() string {
	st := m.styles
	rows := [][2]string{
		{"↑ / k", "move up"},
		{"↓ / j", "move down"},
		{"enter / s", "switch to the selected account"},
		{"n", "log in to add another account (asks which tool)"},
		{"a", "add the account the selected tool is logged in as"},
	}
	// Listed only where it works. A key on the reference that reports
	// "unsupported" when pressed is worse than an absent one: the reference is
	// where someone looks to find out what the dashboard can do.
	if slices.ContainsFunc(m.panes, func(p pane) bool { return p.spec.Can(provider.CapToken) }) {
		rows = append(rows, [2]string{"t", "add a setup token or managed API key"})
	}
	rows = append(rows,
		[2]string{"d", "disable or enable the selected account"},
		[2]string{"r", "collect now"},
		[2]string{"?", "close this help"},
		[2]string{"q / esc", "quit"},
	)
	var b strings.Builder
	b.WriteString(st.title.Render("Keys"))
	for _, row := range rows {
		b.WriteString("\n" + st.helpKey.Render(fmt.Sprintf("%-10s", row[0])) + st.help.Render(row[1]))
	}
	b.WriteString("\n\n" + st.muted.Render(fmt.Sprintf(
		"Every tool's accounts are on this screen, and it refreshes on its own:\n"+
			"a login or a switch made anywhere shows within a second, and usage is\n"+
			"re-collected every %d seconds. Usage itself is fetched on the store's\n"+
			"own schedule, so an age note is not a stuck screen.\n\n"+
			"Switching writes a live credential. It takes the store lock, so it may\n"+
			"pause while another aaswap or a running tool holds it.\n\n"+
			"`n` runs the tool's own login into a sandbox and stores what lands\n"+
			"there. The login you have now is not touched.", int(RefreshInterval.Seconds()))))
	return st.modal.Render(b.String())
}
