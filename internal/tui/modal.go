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
	// modalInput collects one line of text — a token — and submits it.
	modalInput
	// modalWaiting reports work that ends on its own, and is cancelled rather
	// than answered.
	modalWaiting
	// modalPick chooses one row of a short list and runs onPick with it.
	modalPick
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

	// input is what a modalInput has collected so far, and submit turns it
	// into the command to run. Unused by every other kind.
	input  string
	submit func(string) tea.Cmd
	// placeholder shows in the field while it is empty.
	placeholder string
	// hint describes what has been typed so far — for a token, which kind of
	// account it would become. Rendered under the field, and skipped when it
	// returns nothing.
	hint func(string) string

	// options are a modalPick's rows, pick the marked one, and onPick turns
	// the accepted index into the command to run. cancelQuits makes esc leave
	// the program rather than the modal, for a question the dashboard cannot
	// show anything without an answer to.
	options     []pickOption
	pick        int
	onPick      func(int) tea.Cmd
	cancelQuits bool
}

// visibleInput is what the field shows for what has been typed.
//
// A token is a live credential, so it is masked — but never entirely. The first
// characters are the kind marker (sk-ant-oat01- against sk-ant-api03-), they
// decide which sort of account this becomes, and a field that hides them leaves
// no way to notice the wrong thing was pasted.
func (md *modal) visibleInput() string {
	const revealed = 13
	runes := []rune(md.input)
	if len(runes) <= revealed {
		return md.input
	}
	return string(runes[:revealed]) + strings.Repeat("•", len(runes)-revealed)
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
	if md.kind == modalInput {
		field := md.visibleInput()
		if field == "" {
			field = st.muted.Render(md.placeholder)
		}
		b.WriteString("\n\n  " + st.accent.Render("▏") + field + st.accent.Render("▁"))
		if md.hint != nil {
			if note := md.hint(md.input); note != "" {
				b.WriteString("\n  " + st.muted.Render("  "+note))
			}
		}
	}
	if md.kind == modalPick {
		b.WriteString(m.renderPick(md))
	}
	b.WriteString("\n\n")
	switch md.kind {
	case modalPick:
		leave := " quit"
		if !md.cancelQuits {
			leave = " cancel"
		}
		b.WriteString(st.helpKey.Render("↑↓") + st.help.Render(" move    ") +
			st.helpKey.Render("enter") + st.help.Render(" choose    ") +
			st.helpKey.Render("esc") + st.help.Render(leave))
	case modalConfirm:
		b.WriteString(st.helpKey.Render("y") + st.help.Render(" confirm    ") +
			st.helpKey.Render("n/esc") + st.help.Render(" cancel"))
	case modalNotice:
		b.WriteString(st.help.Render("any key to dismiss"))
	case modalInput:
		b.WriteString(st.helpKey.Render("enter") + st.help.Render(" submit    ") +
			st.helpKey.Render("ctrl+u") + st.help.Render(" clear    ") +
			st.helpKey.Render("esc") + st.help.Render(" cancel"))
	case modalWaiting:
		b.WriteString(st.helpKey.Render("esc") + st.help.Render(" stop waiting"))
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
