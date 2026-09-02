package tui

import (
	"context"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/swap"
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
	// spec is the declaration for the provider being shown. The dashboard's
	// keys are a menu, and a menu offering an action the provider has no
	// declaration for is worse here than at the command line: a flag at least
	// names itself, while a key is simply listed on the help screen with
	// nothing to say it belongs to another tool.
	spec   provider.Spec
	styles styles
	clock  func() time.Time

	// providers is every tool the dashboard can be pointed at, and open
	// builds the switcher for one. With more than one, `p` re-points it;
	// with no switcher at all, the first thing on screen is the question.
	providers []ProviderChoice
	open      func(name string) (*swap.Switcher, error)

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

	// awaitCancel stops a login wait, and is non-nil exactly while one is
	// running. The wait outlives the keypress that started it — it can run for
	// as long as it takes a person to log in elsewhere — so the only way to end
	// it early is to hold its cancel here.
	awaitCancel context.CancelFunc
	// awaitFrame advances the waiting modal's marker.
	awaitFrame int
}

// NewModel builds the dashboard. Without a switcher it opens on the provider
// picker, which needs Providers and Open.
func NewModel(opts Options) Model {
	m := Model{
		styles:    newStyles(PaletteFor(opts.Theme)),
		providers: opts.Providers,
		open:      opts.Open,
		width:     80,
		height:    24,
	}
	if s := opts.Switcher; s != nil {
		m.switcher, m.spec, m.clock = s, s.Spec(), s.Now
	}
	return m
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

// Init starts the first collect — or, with no tool chosen yet, asks.
func (m Model) Init() tea.Cmd {
	if m.switcher == nil {
		return func() tea.Msg { return askProviderMsg{} }
	}
	return collectCmd(m.switcher)
}

// askProviderMsg opens the picker from Init, which returns a command rather
// than a model and so cannot set the modal itself.
type askProviderMsg struct{}

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
		return switchedMsg{to: outcome.To.Name, email: outcome.To.Email}
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
