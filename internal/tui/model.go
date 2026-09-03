package tui

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/swap"
)

// ScanInterval is how often the dashboard looks for a reason to re-collect.
//
// A look is a stat per watched file — the roster, the usage table, the live
// login files — and nothing else, so it can afford to be frequent. It is what
// makes a `/login` or an `aaswap switch` in another terminal show up on the
// dashboard within about a second, without a Keychain read or a store lock per
// second to pay for it.
const ScanInterval = time.Second

// RefreshInterval is how often the dashboard re-collects with no change seen.
//
// The scan notices what OTHER processes write; this notices what only a
// collect would produce: a usage fetch that has come due. Deliberately slower
// than it looks like it could be — a collect pass may spend a real request per
// account, and the store's own poll plan decides what is actually due. This is
// the rate at which the dashboard ASKS, not the rate at which anything is
// fetched.
const RefreshInterval = 15 * time.Second

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
	// panes is one section per tool, in registry order. Every tool the build
	// manages is on screen at once: the question the dashboard used to open
	// on — which tool? — has no good answer on a machine with both, and the
	// answer to "which account am I on" is only complete across all of them.
	panes  []pane
	styles styles
	clock  func() time.Time

	width, height int

	// rows is the flat list the cursor walks: every account of every pane, and
	// one placeholder row for a pane with none, so that a tool with nothing
	// stored is still somewhere the keys can be pointed. Rebuilt from the
	// panes after every collect.
	rows   []row
	cursor int

	// busy names the exclusive operation in flight, empty when idle. One at a
	// time: two concurrent switches would race for the store lock and the
	// second would act on a roster the first already changed. Collecting is
	// not exclusive — it changes no credential — and is tracked per pane.
	busy string

	// scanning is true while a file scan is out, so a slow disk does not
	// stack them.
	scanning bool
	// lastCollect is when the last full collect was started, for the refresh
	// floor. Zero until the first one.
	lastCollect time.Time
	// gen numbers collect passes. A pass carries the number it was started
	// with, and a result older than the pane's last applied one is dropped:
	// with collects overlapping — one started by a switch, one by the clock
	// — the slower must not overwrite the fresher.
	gen int

	status    string
	statusErr bool

	modal    *modal
	showHelp bool
	quitting bool

	// execTool hands the terminal to a process and takes it back when it
	// exits: the tool's own login, run into a sandbox. Bubble Tea's own in
	// production; a test's stand-in otherwise, since there is no terminal.
	execTool func(*exec.Cmd, func(error) tea.Msg) tea.Cmd
	// stat fingerprints a watched file. The OS's own in production; a test
	// substitutes one to drive a change without touching a disk.
	stat func(string) (os.FileInfo, error)
}

// pane is one tool's section of the dashboard.
type pane struct {
	switcher *swap.Switcher
	// spec is the declaration for the tool. The dashboard's keys are a menu,
	// and a menu offering an action the provider has no declaration for is
	// worse here than at the command line: a flag at least names itself,
	// while a key is simply listed on the help screen with nothing to say it
	// belongs to another tool.
	spec provider.Spec

	// snapshot is the last completed collect pass, nil before the first.
	snapshot *swap.Snapshot
	err      error

	// collecting is true while a pass is out for this pane.
	collecting bool
	// gen is the generation of the last applied pass.
	gen int
	// signature fingerprints the watched files as of the last pass. The scan
	// compares against it, and the pass that follows a change replaces it —
	// so a collect's own writes to the usage table never read as a change
	// that calls for another collect.
	signature string
}

// row is one cursor position: an account, or a pane's placeholder when it
// has none (view is -1).
type row struct {
	pane int
	view int
	// name is the account's, kept on the row so a relayout can find the same
	// account again after the snapshot it was read from is replaced.
	name string
}

// NewModel builds the dashboard over one switcher per tool.
func NewModel(opts Options) Model {
	m := Model{
		styles:   newStyles(PaletteFor(opts.Theme)),
		execTool: execProcess,
		stat:     os.Stat,
		width:    80,
		height:   24,
	}
	for _, s := range opts.Switchers {
		m.panes = append(m.panes, pane{switcher: s, spec: s.Spec()})
	}
	if len(m.panes) > 0 {
		m.clock = m.panes[0].switcher.Now
	}
	m.rows = flatten(m.panes)
	return m
}

func (m Model) now() time.Time {
	if m.clock == nil {
		return time.Now()
	}
	return m.clock()
}

// paneIndex is the pane the cursor is in. There is always one: every pane has
// at least its placeholder row, and the dashboard refuses to open with none.
func (m Model) paneIndex() int {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return 0
	}
	return m.rows[m.cursor].pane
}

// selected is the account the cursor is on, if it is on one.
func (m Model) selected() (*pane, swap.AccountView, bool) {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return nil, swap.AccountView{}, false
	}
	r := m.rows[m.cursor]
	p := &m.panes[r.pane]
	if r.view < 0 || p.snapshot == nil || r.view >= len(p.snapshot.Views) {
		return p, swap.AccountView{}, false
	}
	return p, p.snapshot.Views[r.view], true
}

// flatten lays the panes' accounts out as cursor rows.
func flatten(panes []pane) []row {
	var rows []row
	for i, p := range panes {
		if p.snapshot == nil || len(p.snapshot.Views) == 0 {
			rows = append(rows, row{pane: i, view: -1})
			continue
		}
		for j, view := range p.snapshot.Views {
			rows = append(rows, row{pane: i, view: j, name: view.Name})
		}
	}
	return rows
}

// managed is how many accounts are stored across every tool.
func (m Model) managed() int {
	n := 0
	for _, p := range m.panes {
		if p.snapshot != nil {
			n += len(p.snapshot.Views)
		}
	}
	return n
}

// anyCollecting reports whether a pass is out for any pane.
func (m Model) anyCollecting() bool {
	for _, p := range m.panes {
		if p.collecting {
			return true
		}
	}
	return false
}

// Init starts the dashboard. Init returns a command rather than a model, and
// the first collect marks every pane as collecting, so the work is done by
// Update on a message sent from here.
func (m Model) Init() tea.Cmd {
	return func() tea.Msg { return startMsg{} }
}

// startMsg is Init's request to begin: the first collect and the clock behind
// the live refresh.
type startMsg struct{}

// --- messages ---------------------------------------------------------------

// collectedMsg carries a finished collect pass for one pane.
type collectedMsg struct {
	pane      int
	gen       int
	snapshot  *swap.Snapshot
	signature string
	err       error
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

// scanTickMsg is the clock behind the live refresh.
type scanTickMsg time.Time

// scannedMsg carries one fingerprint per pane.
type scannedMsg struct {
	signatures []string
}

// clearStatusMsg retires a transient status line.
type clearStatusMsg struct{}

// --- commands ---------------------------------------------------------------
//
// Each wraps one blocking call. They take the switcher rather than a Model so
// nothing can accidentally close over UI state and mutate it off the loop.

func collectCmd(s *swap.Switcher, index, gen int, stat func(string) (os.FileInfo, error)) tea.Cmd {
	return func() tea.Msg {
		snapshot, err := s.TakeSnapshot(context.Background(), swap.CollectRequest{})
		// Fingerprinted AFTER the pass, so the pane's baseline includes
		// whatever the pass itself wrote to the usage table.
		return collectedMsg{pane: index, gen: gen, snapshot: snapshot, err: err,
			signature: signatureOf(s.WatchedPaths(), stat)}
	}
}

func scanCmd(switchers []*swap.Switcher, stat func(string) (os.FileInfo, error)) tea.Cmd {
	return func() tea.Msg {
		signatures := make([]string, len(switchers))
		for i, s := range switchers {
			signatures[i] = signatureOf(s.WatchedPaths(), stat)
		}
		return scannedMsg{signatures: signatures}
	}
}

// signatureOf fingerprints a set of files by what a stat can see.
//
// Modification time and size, not content: content would mean reading a
// credential file once a second, and the point of the scan is to be the one
// thing the dashboard does that often. A missing file fingerprints as its own
// state rather than as an error — a login file appearing is a change worth
// noticing, and so is one disappearing.
func signatureOf(paths []string, stat func(string) (os.FileInfo, error)) string {
	var b strings.Builder
	for _, path := range paths {
		b.WriteString(path)
		if info, err := stat(path); err == nil {
			b.WriteString("=" + info.ModTime().UTC().Format(time.RFC3339Nano) + "/" + strconv.FormatInt(info.Size(), 10))
		} else {
			b.WriteString("=-")
		}
		b.WriteByte('\n')
	}
	return b.String()
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

func (m Model) scanTickCmd() tea.Cmd {
	return tea.Tick(ScanInterval, func(t time.Time) tea.Msg { return scanTickMsg(t) })
}

func clearStatusCmd() tea.Cmd {
	return tea.Tick(statusLinger, func(time.Time) tea.Msg { return clearStatusMsg{} })
}

// execProcess is Bubble Tea's terminal handover, in the shape the model holds.
func execProcess(c *exec.Cmd, done func(error) tea.Msg) tea.Cmd {
	return tea.ExecProcess(c, func(err error) tea.Msg { return done(err) })
}
