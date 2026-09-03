package tui

import (
	"fmt"
	"strconv"
	"strings"

	tea "charm.land/bubbletea/v2"
)

// pickOption is one row of a pick modal.
type pickOption struct {
	label string
	note  string
}

// askLogin starts a login, asking which tool it is for first.
//
// Asked rather than read off the cursor, for the same reason `aaswap login`
// asks at a terminal whatever the store holds: nothing on screen says which
// tool the NEXT account is for. The cursor is on some tool's section, but the
// person adding their first Codex account has it wherever it was left — on a
// Claude Code account, usually — and a login that opened Claude Code's on
// them would be the failure the question exists to prevent. The cursor's
// tool is the row marked, so the usual answer is one keypress.
func (m Model) askLogin() (tea.Model, tea.Cmd) {
	if m.busy != "" {
		return m, nil
	}
	if len(m.panes) == 1 {
		return m.startLogin(0)
	}
	options := make([]pickOption, 0, len(m.panes))
	for _, p := range m.panes {
		n := 0
		if p.snapshot != nil {
			n = len(p.snapshot.Views)
		}
		options = append(options, pickOption{label: p.spec.DisplayName(), note: accountsWord(n)})
	}
	m.modal = &modal{
		kind:      modalPick,
		title:     "Which tool is this login for?",
		options:   options,
		pick:      m.paneIndex(),
		busyLabel: "opening a login",
		onPick: func(index int) tea.Cmd {
			return beginLoginCmd(m.panes[index].switcher, index)
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
	m.busy = m.modal.busyLabel
	m.modal = nil
	return m, cmd
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
