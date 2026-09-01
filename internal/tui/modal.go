package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// modalKind distinguishes what a modal is for, because the key that dismisses
// one is not the key that answers another.
type modalKind int

const (
	// modalConfirm asks a yes-or-no question and runs onYes.
	modalConfirm modalKind = iota
	// modalNotice reports a result and is dismissed by any key.
	modalNotice
)

// modal is the one overlay the dashboard can show at a time.
//
// A pointer field on the Model rather than a stack: two stacked modals would
// mean two pending answers, and every question here is about a credential
// operation that must be resolved before the next one starts.
type modal struct {
	kind   modalKind
	title  string
	body   []string
	danger bool

	// run is the command a confirm modal fires when accepted, and busyLabel
	// what the header shows while it is in flight. The command is built when
	// the question is asked, so the decision and the action cannot drift apart:
	// whatever the prompt named is exactly what runs.
	run       tea.Cmd
	busyLabel string
}

// render draws a modal centered on the dashboard.
func (m Model) renderModal(md *modal) string {
	st := m.styles
	box := st.modal
	if md.danger {
		box = st.modalWarn
	}

	var b strings.Builder
	b.WriteString(st.title.Render(md.title))
	for _, line := range md.body {
		b.WriteString("\n" + line)
	}
	b.WriteString("\n\n")
	switch md.kind {
	case modalConfirm:
		b.WriteString(st.helpKey.Render("y") + st.help.Render(" confirm    ") +
			st.helpKey.Render("n/esc") + st.help.Render(" cancel"))
	case modalNotice:
		b.WriteString(st.help.Render("any key to dismiss"))
	}

	// Cap the modal so a long error wraps inside the frame instead of running
	// off the terminal and taking the border with it.
	width := min(max(m.width-10, 30), 72)
	return box.Width(width).Render(b.String())
}

// overlay centers a modal over the dashboard.
//
// The background is rendered and then replaced rather than composited: a real
// composite needs cell-accurate width for every styled rune underneath, and
// getting that subtly wrong shears the frame. Replacing is honest about what
// it is, and a modal is meant to take the screen anyway.
func (m Model) overlay(background, box string) string {
	if m.width <= 0 || m.height <= 0 {
		return box
	}
	return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, box)
}
