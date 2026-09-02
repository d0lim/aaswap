package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/d0lim/aaswap/internal/swap"
)

// ProviderChoice is one tool the dashboard can be pointed at.
type ProviderChoice struct {
	Name  string
	Label string
	// Accounts is how many are stored for it — the fact that tells the
	// choices apart on a machine where several tools are managed.
	Accounts int
}

// pickOption is one row of a pick modal.
type pickOption struct {
	label string
	note  string
}

// providerOpenedMsg carries a switcher built for the chosen provider.
type providerOpenedMsg struct {
	name string
	s    *swap.Switcher
	err  error
}

func openProviderCmd(open func(string) (*swap.Switcher, error), name string) tea.Cmd {
	return func() tea.Msg {
		s, err := open(name)
		return providerOpenedMsg{name: name, s: s, err: err}
	}
}

// pointed reports whether the dashboard is showing some tool yet. Before the
// first pick is answered it is not, and there is nothing to draw.
func (m Model) pointed() bool { return m.spec.Name != "" }

// canPickProvider reports whether there is a choice to make at all.
func (m Model) canPickProvider() bool {
	return m.open != nil && len(m.providers) > 1
}

// askProvider opens the picker.
//
// This is how the dashboard is pointed at a tool: nothing is the default, so
// where the store cannot say which one is meant, the person is asked here
// rather than at a prompt before the screen even opens. The same picker
// serves `p`, which turns one dashboard into every tool's.
func (m Model) askProvider() (tea.Model, tea.Cmd) {
	if m.busy != "" || !m.canPickProvider() {
		return m, nil
	}
	options := make([]pickOption, 0, len(m.providers))
	pick := 0
	for i, choice := range m.providers {
		note := accountsWord(choice.Accounts)
		if choice.Name == m.spec.Name {
			note += ", shown now"
			pick = i
		}
		options = append(options, pickOption{label: choice.Label, note: note})
	}
	m.modal = &modal{
		kind:    modalPick,
		title:   "Which tool?",
		options: options,
		pick:    pick,
		// Before any tool is shown there is nothing to go back to, so the
		// way out of the question is the way out of the program.
		cancelQuits: !m.pointed(),
		onPick: func(index int) tea.Cmd {
			return openProviderCmd(m.open, m.providers[index].Name)
		},
	}
	return m, nil
}

// handlePickKey moves through, answers, or leaves a pick modal.
func (m Model) handlePickKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	md := m.modal
	switch key.String() {
	case "up", "k":
		md.pick = max(md.pick-1, 0)
		return m, nil
	case "down", "j":
		md.pick = min(md.pick+1, len(md.options)-1)
		return m, nil
	case "esc", "q":
		if md.cancelQuits {
			return m.quit()
		}
		m.modal = nil
		return m, nil
	case "enter":
		return m.acceptPick(md.pick)
	}
	// A digit is the row's number as the modal prints it, one-based.
	if n, err := strconv.Atoi(key.String()); err == nil && n >= 1 && n <= len(md.options) {
		return m.acceptPick(n - 1)
	}
	return m, nil
}

func (m Model) acceptPick(index int) (tea.Model, tea.Cmd) {
	cmd := m.modal.onPick(index)
	m.modal = nil
	m.busy = "opening"
	return m, cmd
}

// handleProviderOpened points the dashboard at the chosen tool.
func (m Model) handleProviderOpened(msg providerOpenedMsg) (tea.Model, tea.Cmd) {
	m.busy = ""
	if msg.err != nil {
		// Back to the question, with the reason on the status line: a
		// dashboard with no tool behind it has nothing else to show.
		m.status, m.statusErr = msg.err.Error(), true
		next, cmd := m.askProvider()
		return next, tea.Batch(cmd, clearStatusCmd())
	}
	m.switcher = msg.s
	m.spec = msg.s.Spec()
	m.clock = msg.s.Now
	// Everything on screen described the previous tool.
	m.snapshot, m.order, m.cursor, m.err = nil, nil, 0, nil
	m.busy = "collecting"
	return m, collectCmd(m.switcher)
}

// renderPick is the body of a pick modal: numbered rows, one marked.
func (m Model) renderPick(md *modal) string {
	st := m.styles
	var b strings.Builder
	for i, option := range md.options {
		cursor := "  "
		label := st.help.Render(option.label)
		if i == md.pick {
			cursor = st.accent.Render("▸ ")
			label = st.accent.Render(option.label)
		}
		fmt.Fprintf(&b, "\n%s%s %s  %s", cursor,
			st.helpKey.Render(strconv.Itoa(i+1)), label, st.muted.Render(option.note))
	}
	return b.String()
}

// accountsWord is the count as a person reads it.
func accountsWord(n int) string {
	switch n {
	case 0:
		return "no accounts stored"
	case 1:
		return "1 account stored"
	}
	return fmt.Sprintf("%d accounts stored", n)
}
