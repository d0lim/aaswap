package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/realiti4/claude-swap/internal/render"
	"github.com/realiti4/claude-swap/internal/swap"
)

// WatchInterval is how often watch mode re-collects.
//
// Deliberately slower than it looks like it could be: a collect pass may spend
// a real request per account, and the store's own poll plan decides what is
// actually due. This is the rate at which the dashboard ASKS, not the rate at
// which anything is fetched.
const WatchInterval = 30 * time.Second

// statusLinger is how long a one-line result stays before clearing itself.
const statusLinger = 6 * time.Second

// Model is the dashboard's whole state.
//
// The UI loop never performs blocking work: every lock, Keychain call and
// network fetch happens inside a [tea.Cmd] and comes back as a message. That
// is the same discipline the Textual implementation held, and it matters more
// here than in an ordinary TUI — a collect pass can sit on a file lock for
// seconds, and a frozen dashboard during a credential operation reads as a
// crash at exactly the moment the user needs to see what happened.
type Model struct {
	switcher *swap.Switcher
	styles   styles
	clock    func() time.Time

	width, height int

	// snapshot is the last completed collect pass, and order the slot numbers
	// it holds, in display order. Kept as a slice because the cursor indexes
	// positions, not map keys.
	snapshot *swap.Snapshot
	order    []string
	cursor   int

	// busy names the operation in flight, empty when idle. One at a time: two
	// concurrent switches would race for the store lock and the second would
	// act on a roster the first already changed.
	busy string

	err       error
	status    string
	statusErr bool

	modal    *modal
	showHelp bool
	watch    bool
	quitting bool
}

// NewModel builds the dashboard over a switcher.
func NewModel(s *swap.Switcher, theme render.Theme) Model {
	return Model{
		switcher: s,
		styles:   newStyles(PaletteFor(theme)),
		clock:    s.Now,
		width:    80,
		height:   24,
	}
}

func (m Model) now() time.Time {
	if m.clock == nil {
		return time.Now()
	}
	return m.clock()
}

// selected is the slot the cursor is on, if any.
func (m Model) selected() (swap.AccountView, bool) {
	if m.snapshot == nil || m.cursor < 0 || m.cursor >= len(m.snapshot.Views) {
		return swap.AccountView{}, false
	}
	return m.snapshot.Views[m.cursor], true
}

// Init starts the first collect.
func (m Model) Init() tea.Cmd {
	return collectCmd(m.switcher)
}

// --- messages ---------------------------------------------------------------

// collectedMsg carries a finished collect pass.
type collectedMsg struct {
	snapshot *swap.Snapshot
	err      error
}

// switchedMsg carries a finished activation.
type switchedMsg struct {
	to    string
	email string
	err   error
}

// toggledMsg carries a finished enable/disable.
type toggledMsg struct {
	num      string
	email    string
	disabled bool
	changed  bool
	err      error
}

// tickMsg drives watch mode.
type tickMsg time.Time

// clearStatusMsg retires a transient status line.
type clearStatusMsg struct{}

// --- commands ---------------------------------------------------------------
//
// Each wraps one blocking call. They take the switcher rather than a Model so
// nothing can accidentally close over UI state and mutate it off the loop.

func collectCmd(s *swap.Switcher) tea.Cmd {
	return func() tea.Msg {
		snapshot, err := s.TakeSnapshot(context.Background(), swap.CollectRequest{})
		return collectedMsg{snapshot: snapshot, err: err}
	}
}

func switchCmd(s *swap.Switcher, target, email string) tea.Cmd {
	return func() tea.Msg {
		outcome, err := s.Switch(context.Background(), swap.SwitchRequest{Target: target})
		if err != nil {
			return switchedMsg{to: target, email: email, err: err}
		}
		return switchedMsg{to: outcome.To.Number, email: outcome.To.Email}
	}
}

func toggleCmd(s *swap.Switcher, target string, disabled bool) tea.Cmd {
	return func() tea.Msg {
		num, email, changed, err := s.SetDisabled(target, disabled)
		return toggledMsg{num: num, email: email, disabled: disabled, changed: changed, err: err}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(WatchInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func clearStatusCmd() tea.Cmd {
	return tea.Tick(statusLinger, func(time.Time) tea.Msg { return clearStatusMsg{} })
}
