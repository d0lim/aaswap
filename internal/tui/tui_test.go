package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/render"
	"github.com/d0lim/aaswap/internal/swap"
	"github.com/d0lim/aaswap/internal/usage"
	"github.com/d0lim/aaswap/internal/usagestore"
)

var testNow = time.Date(2026, 7, 4, 14, 12, 0, 0, time.UTC)

// fixture builds a dashboard over a hand-made snapshot.
//
// No switcher: View and the key handling are pure, and the commands that need
// one are covered by the packages they call into. Wiring a real store here
// would test swap, not the dashboard.
func fixture(t *testing.T, views []swap.AccountView, entries map[string]usagestore.Entry) Model {
	t.Helper()
	m := Model{
		styles:   newStyles(PaletteFor(render.Dark)),
		clock:    func() time.Time { return testNow },
		stat:     os.Stat,
		execTool: execProcess,
		width:    76,
		height:   24,
		panes: []pane{{
			// Claude's declaration, which is what these tests were written
			// against. Leaving it zero would disable every key the
			// declaration gates and the tests would pass by not reaching them.
			spec:     provider.MustLookup(provider.Claude),
			snapshot: &swap.Snapshot{Views: views, Entries: entries},
		}},
	}
	m.rows = flatten(m.panes)
	return m
}

func window(pct float64, resetsIn time.Duration) *usage.Window {
	return &usage.Window{Pct: pct, ResetsAt: testNow.Add(resetsIn).Format(time.RFC3339)}
}

func twoAccounts(t *testing.T) Model {
	t.Helper()
	return fixture(t,
		[]swap.AccountView{
			{Name: "1", IsActive: true, Account: &swap.Account{
				Email: "work@example.com"}},
			{Name: "2", Account: &swap.Account{Email: "spare@example.com"}},
		},
		map[string]usagestore.Entry{
			"1": {FetchedAt: testNow, LastGood: &usage.Result{
				FiveHour: window(62, 6*time.Hour+27*time.Minute),
				SevenDay: window(31, 32*time.Hour),
			}},
			"2": {FetchedAt: testNow, LastGood: &usage.Result{
				FiveHour: window(11, 3*time.Hour+50*time.Minute),
				SevenDay: window(19, 70*time.Hour),
			}},
		})
}

// A bar's length has to track its percentage, and both ends have to be
// distinguishable: a fully spent window and an untouched one rendering alike
// is the one failure that would make the dashboard actively misleading.
func TestBarLength(t *testing.T) {
	tests := []struct {
		name  string
		pct   float64
		width int
		want  string
	}{
		{"empty", 0, 8, "░░░░░░░░"},
		{"full", 100, 8, "████████"},
		{"half", 50, 8, "████░░░░"},
		{"over 100 is clamped to the track", 140, 8, "████████"},
		{"negative is clamped to empty", -20, 8, "░░░░░░░░"},
		// A sliver must remain visible rather than rounding away: "a little
		// quota left" and "none" are different answers.
		{"a sliver still shows", 3, 8, "▏░░░░░░░"},
		{"zero width renders nothing", 50, 0, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := renderBar(tt.pct, tt.width); got != tt.want {
				t.Errorf("renderBar(%v, %d) = %q, want %q", tt.pct, tt.width, got, tt.want)
			}
		})
	}
}

// Every bar is the same number of cells wide whatever it holds, or the columns
// beneath it shear and the list stops being scannable.
func TestBarsAreAllTheSameWidth(t *testing.T) {
	for _, pct := range []float64{0, 3, 12.5, 50, 99.9, 100, 250} {
		if got := len([]rune(renderBar(pct, 16))); got != 16 {
			t.Errorf("renderBar(%v, 16) is %d cells, want 16", pct, got)
		}
	}
}

func TestResetNote(t *testing.T) {
	tests := []struct {
		name  string
		reset time.Time
		want  string
	}{
		{"later today shows the clock", testNow.Add(6 * time.Hour), "resets 20:12"},
		{"another day shows the date", testNow.Add(50 * time.Hour), "resets Jul 6 16:12"},
		// The API leaves a spent window's reset in the past until it rolls it
		// over; yesterday's clock time would read as broken data.
		{"already past reads as due", testNow.Add(-time.Hour), "resets due"},
		{"exactly now reads as due", testNow, "resets due"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resetNote(tt.reset, testNow); got != tt.want {
				t.Errorf("resetNote = %q, want %q", got, tt.want)
			}
		})
	}
}

// The frame has to name every account and mark exactly one as live.
func TestTheDashboardShowsEveryAccount(t *testing.T) {
	frame := twoAccounts(t).View().Content
	for _, want := range []string{"work@example.com", "spare@example.com", "62%", "11%", "aaswap"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the frame is missing %q:\n%s", want, frame)
		}
	}
	if got := strings.Count(frame, "●"); got != 1 {
		t.Errorf("%d active markers, want exactly one", got)
	}
}

// A sentinel replaces the bars rather than joining them: an empty bar beside
// "api key" reads as zero usage, which is a different and wrong claim.
func TestASentinelReplacesTheBars(t *testing.T) {
	m := fixture(t,
		[]swap.AccountView{{Name: "1", Account: &swap.Account{Email: "key@example.com"}}},
		map[string]usagestore.Entry{"1": {Sentinel: swap.SentinelAPIKey}})

	frame := m.View().Content
	if !strings.Contains(frame, render.SentinelNote(swap.SentinelAPIKey)) {
		t.Errorf("the sentinel is not shown:\n%s", frame)
	}
	if strings.ContainsRune(frame, barEmpty) || strings.ContainsRune(frame, barFull) {
		t.Errorf("a sentinel account still drew a usage bar:\n%s", frame)
	}
}

// The cursor clamps instead of wrapping. A credential list is not a carousel:
// wrapping past the end is how someone holding a key down lands on — and then
// switches to — an account they never meant to select.
func TestTheCursorClampsAtBothEnds(t *testing.T) {
	m := twoAccounts(t)

	for range 5 {
		m = m.moveCursor(-1)
	}
	if m.cursor != 0 {
		t.Errorf("cursor = %d after moving up past the start, want 0", m.cursor)
	}
	for range 5 {
		m = m.moveCursor(1)
	}
	if m.cursor != 1 {
		t.Errorf("cursor = %d after moving down past the end, want 1", m.cursor)
	}
}

// A collect pass that comes back shorter than the last one must not leave the
// cursor pointing past the end — another process removing an account between
// passes is ordinary, and a stale index would switch to the wrong slot.
func TestTheCursorSurvivesAShrinkingList(t *testing.T) {
	m := twoAccounts(t)
	m.cursor = 1

	next, _ := m.handleCollected(collectedMsg{snapshot: &swap.Snapshot{
		Views:   []swap.AccountView{{Name: "1", Account: &swap.Account{Email: "work@example.com"}}},
		Entries: map[string]usagestore.Entry{},
	}})

	if got := next.(Model).cursor; got != 0 {
		t.Errorf("cursor = %d after the list shrank to one, want 0", got)
	}
}

// The cursor follows its account, not its position. With the screen
// refreshing on its own, an account added above the cursor by another
// process would otherwise slide the neighbour under a finger already on
// enter.
func TestTheCursorFollowsItsAccountAcrossARefresh(t *testing.T) {
	m := twoAccounts(t)
	m.cursor = 1 // spare@example.com

	next, _ := m.handleCollected(collectedMsg{snapshot: &swap.Snapshot{
		Views: []swap.AccountView{
			{Name: "0", Account: &swap.Account{Email: "first@example.com"}},
			{Name: "1", Account: &swap.Account{Email: "work@example.com"}},
			{Name: "2", Account: &swap.Account{Email: "spare@example.com"}},
		},
		Entries: map[string]usagestore.Entry{},
	}})

	_, view, ok := next.(Model).selected()
	if !ok || view.Name != "2" {
		t.Errorf("the cursor is on %q after an account was added above it, want it still on 2", view.Name)
	}
}

// Switching replaces a live credential, so it asks first — and the modal it
// opens must carry the command for the account the prompt actually names.
func TestSwitchingAsksBeforeItActs(t *testing.T) {
	m := twoAccounts(t)
	m.cursor = 1

	next, cmd := m.askSwitch()
	model := next.(Model)
	if model.modal == nil {
		t.Fatal("selecting another account switched without asking")
	}
	if !strings.Contains(model.modal.title, "spare@example.com") {
		t.Errorf("the prompt names %q, not the selected account", model.modal.title)
	}
	if model.modal.run == nil {
		t.Error("the confirm modal carries no command, so answering yes does nothing")
	}
	if cmd != nil {
		t.Error("asking a question ran a command")
	}
}

// The already-active account is the one case where the prompt would be a lie:
// answering yes would do nothing at all.
func TestSwitchingToTheActiveAccountJustSaysSo(t *testing.T) {
	m := twoAccounts(t) // slot 1 is active, and the cursor starts there
	next, _ := m.askSwitch()
	model := next.(Model)
	if model.modal != nil {
		t.Error("the active account still opened a switch prompt")
	}
	if !strings.Contains(model.status, "already active") {
		t.Errorf("status = %q, want it to say the account is already active", model.status)
	}
}

// One blocking operation at a time. Two switches would race for the store lock
// and the second would act on a roster the first had already changed.
func TestABusyDashboardStartsNothingNew(t *testing.T) {
	m := twoAccounts(t)
	m.busy = "switching"
	m.cursor = 1

	next, cmd := m.askSwitch()
	if next.(Model).modal != nil || cmd != nil {
		t.Error("a second operation started while one was in flight")
	}

	next, cmd = m.toggleSelected()
	if cmd != nil || next.(Model).busy != "switching" {
		t.Error("a disable started while a switch was in flight")
	}
}

// Quitting must leave the alt screen empty, so the shell gets its scrollback
// back instead of a frozen copy of the dashboard.
func TestQuittingClearsTheScreen(t *testing.T) {
	m := twoAccounts(t)
	next, cmd := m.handleKey(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if cmd == nil {
		t.Fatal("q did not quit")
	}
	if content := next.(Model).View().Content; content != "" {
		t.Errorf("the final frame still holds content:\n%s", content)
	}
}

// The refresh clock is chained from the tick, not from the scan or collect
// it starts. A pass stuck on a contended lock must not silently end the
// refresh — and while a credential is being written, no scan starts, since
// the operation's own collect is about to redraw everything.
func TestTheRefreshClockKeepsTickingThroughABusyOperation(t *testing.T) {
	m := twoAccounts(t)
	m.busy = "switching"

	next, cmd := m.Update(scanTickMsg(testNow))
	if cmd == nil {
		t.Error("a tick that arrived mid-switch scheduled no successor, so the refresh stops")
	}
	if next.(Model).scanning {
		t.Error("a scan started while a credential was being written")
	}

	m.busy = ""
	next, cmd = m.Update(scanTickMsg(testNow))
	if cmd == nil || !next.(Model).scanning {
		t.Error("an idle tick started no scan")
	}
}

// A change to one tool's files re-collects that tool and only that tool: the
// other's pass would be a Keychain read and a store lock for nothing.
func TestAChangedFileRecollectsOnlyThatTool(t *testing.T) {
	m := bothTools(t)
	m.lastCollect = testNow
	m.panes[0].signature, m.panes[1].signature = "claude-v1", "codex-v1"

	next, cmd := m.handleScanned(scannedMsg{signatures: []string{"claude-v1", "codex-v2"}})
	model := next.(Model)
	if model.panes[0].collecting {
		t.Error("the unchanged tool was re-collected")
	}
	if !model.panes[1].collecting || cmd == nil {
		t.Error("the changed tool was not re-collected")
	}
}

// With nothing changed, the floor still re-collects everything on its
// interval: a usage fetch that has come due is produced by a collect, and no
// file changes until one runs.
func TestTheRefreshFloorRecollectsEverything(t *testing.T) {
	m := bothTools(t)
	m.lastCollect = testNow.Add(-RefreshInterval)
	m.panes[0].signature, m.panes[1].signature = "same", "same"

	next, _ := m.handleScanned(scannedMsg{signatures: []string{"same", "same"}})
	for i, p := range next.(Model).panes {
		if !p.collecting {
			t.Errorf("pane %d was not re-collected at the floor", i)
		}
	}

	m.lastCollect = testNow
	next, _ = m.handleScanned(scannedMsg{signatures: []string{"same", "same"}})
	for i, p := range next.(Model).panes {
		if p.collecting {
			t.Errorf("pane %d was re-collected with nothing changed and the floor not due", i)
		}
	}
}

// A collect writes to the usage table itself. The fingerprint it reports
// becomes the pane's baseline, so its own writes never read as a change that
// calls for another collect — which would be a collect a second, forever.
func TestACollectsOwnWritesAreNotAChange(t *testing.T) {
	m := twoAccounts(t)
	m.lastCollect = testNow
	next, _ := m.handleCollected(collectedMsg{gen: 1, signature: "after-pass",
		snapshot: m.panes[0].snapshot})
	m = next.(Model)
	if m.panes[0].signature != "after-pass" {
		t.Fatalf("signature = %q after the pass, want the pass's own", m.panes[0].signature)
	}
	next, _ = m.handleScanned(scannedMsg{signatures: []string{"after-pass"}})
	if next.(Model).panes[0].collecting {
		t.Error("the pass's own writes triggered another pass")
	}
}

// Passes overlap — one started by a switch, one by the clock — and the slower
// must not overwrite the fresher picture.
func TestAStalePassDoesNotOverwriteAFresherOne(t *testing.T) {
	m := twoAccounts(t)
	m.panes[0].gen = 5

	next, _ := m.handleCollected(collectedMsg{gen: 3, snapshot: &swap.Snapshot{
		Views: []swap.AccountView{{Name: "9", Account: &swap.Account{Email: "old@example.com"}}},
	}})
	if got := len(next.(Model).panes[0].snapshot.Views); got != 2 {
		t.Errorf("an older pass replaced the snapshot: %d accounts, want the fresher 2", got)
	}
}

// The fingerprint tracks what a stat can see, and a file that is not there
// is its own state: a login file appearing is the change worth noticing.
func TestTheSignatureFollowsTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	absent := signatureOf([]string{path}, os.Stat)

	if err := os.WriteFile(path, []byte(`{"a":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	present := signatureOf([]string{path}, os.Stat)
	if present == absent {
		t.Error("a file appearing did not change the signature")
	}
	if err := os.WriteFile(path, []byte(`{"a":1,"b":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if grown := signatureOf([]string{path}, os.Stat); grown == present {
		t.Error("a file growing did not change the signature")
	}
}

// The header says the screen is live. A dashboard that refreshes on its own
// and one that does not look the same until something changes, and the
// difference decides whether a person waits or presses r.
func TestTheHeaderSaysTheScreenIsLive(t *testing.T) {
	if frame := twoAccounts(t).View().Content; !strings.Contains(frame, "live") {
		t.Errorf("the header does not say the screen refreshes on its own:\n%s", frame)
	}
}
