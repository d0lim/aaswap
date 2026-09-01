package tui

import (
	"fmt"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/realiti4/claude-swap/internal/swap"
)

// Update folds one message into the model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case collectedMsg:
		return m.handleCollected(msg)

	case switchedMsg:
		return m.handleSwitched(msg)

	case toggledMsg:
		return m.handleToggled(msg)

	case tickMsg:
		if !m.watch {
			return m, nil
		}
		// Chain the next tick from the tick itself, not from the collect: a
		// collect that hangs on a lock must not stop the clock, or watch mode
		// dies silently at the first contended pass.
		if m.busy != "" {
			return m, tickCmd()
		}
		m.busy = "collecting"
		return m, tea.Batch(collectCmd(m.switcher), tickCmd())

	case clearStatusMsg:
		m.status, m.statusErr = "", false
		return m, nil
	}
	return m, nil
}

// handleKey routes a keypress, giving whatever overlay is open first refusal.
func (m Model) handleKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case m.showHelp:
		// Any key closes help. It is a reference, not a mode to get stuck in.
		m.showHelp = false
		return m, nil
	case m.modal != nil:
		return m.handleModalKey(key)
	}

	switch key.String() {
	case "ctrl+c", "q", "esc":
		m.quitting = true
		return m, tea.Quit

	case "?":
		m.showHelp = true
		return m, nil

	case "up", "k":
		return m.moveCursor(-1), nil

	case "down", "j":
		return m.moveCursor(1), nil

	case "enter", "s":
		return m.askSwitch()

	case "d":
		return m.toggleSelected()

	case "r":
		if m.busy != "" {
			return m, nil
		}
		m.busy = "collecting"
		m.err = nil
		return m, collectCmd(m.switcher)

	case "w":
		m.watch = !m.watch
		if m.watch {
			return m, tickCmd()
		}
		return m, nil
	}
	return m, nil
}

// handleModalKey answers the open modal.
func (m Model) handleModalKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	md := m.modal
	if md.kind == modalNotice {
		m.modal = nil
		return m, nil
	}

	switch key.String() {
	case "y", "Y", "enter":
		cmd := md.run
		m.modal = nil
		m.busy = md.busyLabel
		return m, cmd
	case "n", "N", "esc", "q":
		m.modal = nil
		return m, nil
	}
	return m, nil
}

// moveCursor steps the selection, clamped rather than wrapped: a list of
// credentials is not a carousel, and wrapping past the end is how someone
// switches to the wrong account by holding a key down.
func (m Model) moveCursor(delta int) Model {
	if m.snapshot == nil || len(m.snapshot.Views) == 0 {
		return m
	}
	m.cursor = min(max(m.cursor+delta, 0), len(m.snapshot.Views)-1)
	return m
}

// askSwitch opens the confirmation for activating the selected slot.
func (m Model) askSwitch() (tea.Model, tea.Cmd) {
	view, ok := m.selected()
	if !ok || m.busy != "" {
		return m, nil
	}
	if view.IsActive {
		m.status, m.statusErr = "Account "+view.Number+" is already active", false
		return m, clearStatusCmd()
	}

	body := []string{
		"",
		m.styles.muted.Render("This replaces the live Claude Code credential."),
	}
	if m.hasManagedLiveLogin() {
		body = append(body,
			m.styles.muted.Render("The account you are on now is backed up first."))
	}
	m.modal = &modal{
		kind:      modalConfirm,
		title:     fmt.Sprintf("Switch to Account %s — %s?", view.Number, view.Account.Email),
		body:      body,
		busyLabel: "switching",
		run:       switchCmd(m.switcher, view.Number, view.Account.Email),
	}
	return m, nil
}

// hasManagedLiveLogin reports whether one of the managed slots is the live
// login.
//
// Read off the snapshot rather than by asking the switcher, which would open
// the live config — a disk read, on the UI loop, inside a key handler. The
// collect pass already answered this question; asking twice can only produce a
// second, differing answer.
func (m Model) hasManagedLiveLogin() bool {
	if m.snapshot == nil {
		return false
	}
	return slices.ContainsFunc(m.snapshot.Views, func(v swap.AccountView) bool {
		return v.IsActive
	})
}

// toggleSelected flips the selected slot's rotation membership.
//
// No confirmation: it changes no credential and is undone by pressing the same
// key again. Reserving the modal for operations that touch a credential keeps
// the prompt meaningful.
func (m Model) toggleSelected() (tea.Model, tea.Cmd) {
	view, ok := m.selected()
	if !ok || m.busy != "" {
		return m, nil
	}
	m.busy = "updating"
	return m, toggleCmd(m.switcher, view.Number, !view.Account.Disabled)
}

// --- result handling --------------------------------------------------------

func (m Model) handleCollected(msg collectedMsg) (tea.Model, tea.Cmd) {
	m.busy = ""
	if msg.err != nil {
		m.err = msg.err
		return m, nil
	}
	m.err = nil
	m.snapshot = msg.snapshot
	m.order = slotNumbers(msg.snapshot)

	// Keep the cursor inside a list that may have shrunk under it — a removal
	// from another process is entirely possible between passes.
	m.cursor = min(m.cursor, max(len(msg.snapshot.Views)-1, 0))
	return m, nil
}

func (m Model) handleSwitched(msg switchedMsg) (tea.Model, tea.Cmd) {
	m.busy = ""
	if msg.err != nil {
		m.modal = &modal{
			kind:   modalNotice,
			danger: true,
			title:  "Switch failed",
			body:   []string{"", m.styles.red.Render(msg.err.Error())},
		}
		return m, nil
	}
	m.status = fmt.Sprintf("Activated Account %s (%s)", msg.to, msg.email)
	m.statusErr = false
	// Re-collect: the active marker, and every sentinel derived from the live
	// credential, are now stale.
	m.busy = "collecting"
	return m, tea.Batch(collectCmd(m.switcher), clearStatusCmd())
}

func (m Model) handleToggled(msg toggledMsg) (tea.Model, tea.Cmd) {
	m.busy = ""
	if msg.err != nil {
		m.status, m.statusErr = msg.err.Error(), true
		return m, clearStatusCmd()
	}
	switch {
	case !msg.changed:
		m.status = fmt.Sprintf("Account %s was already %s", msg.num, rotationWord(msg.disabled))
	default:
		m.status = fmt.Sprintf("Account %s (%s) is now %s", msg.num, msg.email, rotationWord(msg.disabled))
	}
	m.statusErr = false
	m.busy = "collecting"
	return m, tea.Batch(collectCmd(m.switcher), clearStatusCmd())
}

func rotationWord(disabled bool) string {
	if disabled {
		return "out of rotation"
	}
	return "in rotation"
}

func slotNumbers(snapshot *swap.Snapshot) []string {
	if snapshot == nil {
		return nil
	}
	out := make([]string, 0, len(snapshot.Views))
	for _, view := range snapshot.Views {
		out = append(out, view.Number)
	}
	return out
}
