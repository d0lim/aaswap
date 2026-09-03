package tui

import (
	"fmt"
	"slices"

	tea "charm.land/bubbletea/v2"

	"github.com/d0lim/aaswap/internal/swap"
)

// Update folds one message into the model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case startMsg:
		next, cmd := m.collectAll()
		return next, tea.Batch(cmd, next.scanTickCmd())

	case collectedMsg:
		return m.handleCollected(msg)

	case switchedMsg:
		return m.handleSwitched(msg)

	case toggledMsg:
		return m.handleToggled(msg)

	case liveProbedMsg:
		return m.handleLiveProbed(msg)

	case addedMsg:
		return m.handleAdded(msg)

	case loginBegunMsg:
		return m.handleLoginBegun(msg)

	case loginRanMsg:
		return m.handleLoginRan(msg)

	case scanTickMsg:
		return m.handleScanTick()

	case scannedMsg:
		return m.handleScanned(msg)

	case clearStatusMsg:
		m.status, m.statusErr = "", false
		return m, nil
	}
	return m, nil
}

// handleScanTick is one beat of the live refresh.
//
// The next beat is chained from this one, never from the scan or the collect
// it starts: a pass stuck on a contended lock must not stop the clock, or the
// refresh dies silently at the first contended pass. A tick also redraws,
// which is what keeps "6m ago" and "resets 20:12" honest between collects.
func (m Model) handleScanTick() (tea.Model, tea.Cmd) {
	next := m.scanTickCmd()
	// Not while the terminal belongs to a tool's login, or while a credential
	// is being written: the picture is about to change by our own hand, and
	// the collect that follows the operation will draw it.
	if m.busy != "" || m.scanning {
		return m, next
	}
	m.scanning = true
	switchers := make([]*swap.Switcher, len(m.panes))
	for i, p := range m.panes {
		switchers[i] = p.switcher
	}
	return m, tea.Batch(scanCmd(switchers, m.stat), next)
}

// handleScanned re-collects what changed, or everything when the floor is due.
func (m Model) handleScanned(msg scannedMsg) (tea.Model, tea.Cmd) {
	m.scanning = false
	if m.busy != "" {
		return m, nil
	}
	if m.now().Sub(m.lastCollect) >= RefreshInterval {
		return m.collectAll()
	}
	var cmds []tea.Cmd
	for i := range m.panes {
		if i >= len(msg.signatures) || m.panes[i].collecting {
			continue
		}
		if msg.signatures[i] != m.panes[i].signature {
			var cmd tea.Cmd
			m, cmd = m.collectPane(i)
			cmds = append(cmds, cmd)
		}
	}
	return m, tea.Batch(cmds...)
}

// collectAll starts a pass for every pane that is not already in one.
func (m Model) collectAll() (Model, tea.Cmd) {
	m.lastCollect = m.now()
	var cmds []tea.Cmd
	for i := range m.panes {
		if m.panes[i].collecting {
			continue
		}
		var cmd tea.Cmd
		m, cmd = m.collectPane(i)
		cmds = append(cmds, cmd)
	}
	return m, tea.Batch(cmds...)
}

// collectPane starts one pane's pass.
//
// The panes are cloned before the mark is set. The model is a value, and a
// slice shared between two copies of it would let a mark made on one show
// through the other — a test's fixture, or a model Bubble Tea has already
// replaced — as if it had been made on both.
func (m Model) collectPane(index int) (Model, tea.Cmd) {
	m.panes = slices.Clone(m.panes)
	m.gen++
	p := &m.panes[index]
	p.collecting = true
	return m, collectCmd(p.switcher, index, m.gen, m.stat)
}

// handleKey routes a keypress, giving whatever overlay is open first refusal.
func (m Model) handleKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	// Ahead of every overlay, because an overlay that can swallow ctrl+c is a
	// dashboard someone cannot get out of.
	if key.String() == "ctrl+c" {
		return m.quit()
	}

	switch {
	case m.showHelp:
		// Any key closes help. It is a reference, not a mode to get stuck in.
		m.showHelp = false
		return m, nil
	case m.modal != nil:
		return m.handleModalKey(key)
	}

	switch key.String() {
	case "q", "esc":
		return m.quit()

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

	case "a":
		return m.askAdd()

	case "n":
		return m.askLogin()

	case "t":
		return m.askAddToken()

	case "r":
		if m.busy != "" {
			return m, nil
		}
		return m.collectAll()
	}
	return m, nil
}

// quit leaves the dashboard.
func (m Model) quit() (tea.Model, tea.Cmd) {
	m.quitting = true
	return m, tea.Quit
}

// handleModalKey answers the open modal.
func (m Model) handleModalKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	md := m.modal
	switch md.kind {
	case modalNotice:
		m.modal = nil
		return m, nil
	case modalInput:
		return m.handleInputKey(key)
	case modalPick:
		return m.handlePickKey(key)
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
	if len(m.rows) == 0 {
		return m
	}
	m.cursor = min(max(m.cursor+delta, 0), len(m.rows)-1)
	return m
}

// askSwitch opens the confirmation for activating the selected slot.
func (m Model) askSwitch() (tea.Model, tea.Cmd) {
	p, view, ok := m.selected()
	if !ok || m.busy != "" {
		return m, nil
	}
	if view.IsActive {
		m.status, m.statusErr = "Account "+view.Name+" is already active", false
		return m, clearStatusCmd()
	}

	body := []string{
		"",
		m.styles.muted.Render(fmt.Sprintf(
			"This replaces the live %s credential.", p.spec.DisplayName())),
	}
	if hasManagedLiveLogin(p.snapshot) {
		body = append(body,
			m.styles.muted.Render("The account you are on now is backed up first."))
	}
	m.modal = &modal{
		kind:      modalConfirm,
		title:     fmt.Sprintf("Switch to Account %s — %s?", view.Name, view.Account.Email),
		body:      body,
		busyLabel: "switching",
		run:       switchCmd(p.switcher, view.Name, view.Account.Email),
	}
	return m, nil
}

// hasManagedLiveLogin reports whether one of a pane's managed slots is the
// live login.
//
// Read off the snapshot rather than by asking the switcher, which would open
// the live config — a disk read, on the UI loop, inside a key handler. The
// collect pass already answered this question; asking twice can only produce a
// second, differing answer.
func hasManagedLiveLogin(snapshot *swap.Snapshot) bool {
	if snapshot == nil {
		return false
	}
	return slices.ContainsFunc(snapshot.Views, func(v swap.AccountView) bool {
		return v.IsActive
	})
}

// toggleSelected flips the selected slot's rotation membership.
//
// No confirmation: it changes no credential and is undone by pressing the same
// key again. Reserving the modal for operations that touch a credential keeps
// the prompt meaningful.
func (m Model) toggleSelected() (tea.Model, tea.Cmd) {
	p, view, ok := m.selected()
	if !ok || m.busy != "" {
		return m, nil
	}
	m.busy = "updating"
	return m, toggleCmd(p.switcher, view.Name, !view.Account.Disabled)
}

// --- result handling --------------------------------------------------------

func (m Model) handleCollected(msg collectedMsg) (tea.Model, tea.Cmd) {
	if msg.pane < 0 || msg.pane >= len(m.panes) {
		return m, nil
	}
	m.panes = slices.Clone(m.panes)
	p := &m.panes[msg.pane]
	if msg.gen < p.gen {
		// An older pass finishing after a newer one. Its picture is already
		// superseded; a pane still marked collecting is waiting on the newer.
		return m, nil
	}
	p.gen = msg.gen
	p.collecting = false
	p.signature = msg.signature
	if msg.err != nil {
		p.err = msg.err
		return m, nil
	}
	p.err = nil
	p.snapshot = msg.snapshot
	return m.relayout(), nil
}

// relayout rebuilds the cursor rows after a pane changed, keeping the cursor
// on the account it was on when that account is still there.
//
// By name rather than by position: a removal from another process is entirely
// possible between passes, and a cursor that kept its index would then sit on
// the neighbour — and switch to it on the enter that was meant for the account
// that vanished. An account that is gone leaves the cursor clamped inside the
// list, on whatever now holds its position.
func (m Model) relayout() Model {
	var was *row
	if m.cursor >= 0 && m.cursor < len(m.rows) {
		was = new(m.rows[m.cursor])
	}
	m.rows = flatten(m.panes)
	m.cursor = min(m.cursor, max(len(m.rows)-1, 0))
	if was == nil || was.view < 0 {
		return m
	}
	// The account's name, from the rows as they were laid out. The pane's
	// snapshot has been replaced by now, so the old index cannot be resolved
	// through it — the name was captured onto the row for this purpose.
	if i := slices.IndexFunc(m.rows, func(r row) bool {
		return r.pane == was.pane && r.view >= 0 && r.name == was.name
	}); i >= 0 {
		m.cursor = i
	}
	return m
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
	next, cmd := m.collectAll()
	return next, tea.Batch(cmd, clearStatusCmd())
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
	next, cmd := m.collectAll()
	return next, tea.Batch(cmd, clearStatusCmd())
}

func rotationWord(disabled bool) string {
	if disabled {
		return "out of rotation"
	}
	return "in rotation"
}
