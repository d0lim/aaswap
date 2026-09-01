package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

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
		styles: newStyles(PaletteFor(render.Dark)),
		clock:  func() time.Time { return testNow },
		width:  76,
		height: 24,
		snapshot: &swap.Snapshot{
			Views:   views,
			Entries: entries,
		},
	}
	m.order = slotNumbers(m.snapshot)
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

// Watch mode's clock is chained from the tick, not from the collect it starts.
// A pass stuck on a contended lock must not silently end watch mode.
func TestWatchKeepsTickingThroughABusyPass(t *testing.T) {
	m := twoAccounts(t)
	m.watch = true
	m.busy = "collecting"

	_, cmd := m.Update(tickMsg(testNow))
	if cmd == nil {
		t.Error("a tick that arrived mid-collect scheduled no successor, so watch mode stops")
	}
}
