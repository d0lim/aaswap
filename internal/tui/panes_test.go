package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/swap"
	"github.com/d0lim/aaswap/internal/usage"
	"github.com/d0lim/aaswap/internal/usagestore"
)

// bothTools is a dashboard over two Claude Code accounts and one Codex
// account, the shape of a machine where both tools are managed.
func bothTools(t *testing.T) Model {
	t.Helper()
	m := twoAccounts(t)
	m.panes = append(m.panes, pane{
		spec: provider.MustLookup(provider.Codex),
		snapshot: &swap.Snapshot{
			Views: []swap.AccountView{
				{Name: "cx", IsActive: true, Account: &swap.Account{Email: "codex@example.com"}},
			},
			Entries: map[string]usagestore.Entry{
				"cx": {FetchedAt: testNow, LastGood: &usage.Result{
					FiveHour: window(40, 2*time.Hour),
					SevenDay: window(55, 90*time.Hour),
				}},
			},
		},
	})
	m.rows = flatten(m.panes)
	return m
}

// typed presses one key and hands back the model.
func typed(m Model, key string) (Model, tea.Cmd) {
	next, cmd := m.Update(press(key))
	return next.(Model), cmd
}

// Every tool is on the one screen, each under its own name, with its own
// live account marked. The question the dashboard used to open on — which
// tool? — has no good answer on a machine with both.
func TestTheDashboardShowsEveryTool(t *testing.T) {
	frame := bothTools(t).View().Content
	for _, want := range []string{"Claude Code", "Codex", "work@example.com", "spare@example.com", "codex@example.com", "40%"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the frame is missing %q:\n%s", want, frame)
		}
	}
	if got := strings.Count(frame, "●"); got != 2 {
		t.Errorf("%d active markers, want one per tool", got)
	}
}

// The cursor walks from one tool's accounts into the next's, so a Codex
// account can be switched to without leaving the screen.
func TestTheCursorCrossesIntoTheNextTool(t *testing.T) {
	m := bothTools(t)
	for range 2 {
		m = m.moveCursor(1)
	}
	p, view, ok := m.selected()
	if !ok || view.Name != "cx" || p.spec.Name != provider.Codex {
		t.Fatalf("selected = %q in %q, want the Codex account", view.Name, p.spec.Name)
	}
	m.cursor = 1 // spare, which is not live
	next, _ := m.askSwitch()
	if title := next.(Model).modal.title; !strings.Contains(title, "spare@example.com") {
		t.Errorf("the prompt names %q, not the selected account", title)
	}
	if body := strings.Join(next.(Model).modal.body, "\n"); !strings.Contains(body, "Claude Code") {
		t.Errorf("the prompt does not say which tool's credential it replaces:\n%s", body)
	}
}

// A tool with nothing stored still has a row: the keys need somewhere to
// point, and the row says which ones apply.
func TestAToolWithNothingStoredStillHasARow(t *testing.T) {
	m := bothTools(t)
	m.panes[1].snapshot = &swap.Snapshot{}
	m.rows = flatten(m.panes)

	frame := m.View().Content
	if !strings.Contains(frame, "No accounts yet") {
		t.Errorf("the empty tool has no row:\n%s", frame)
	}
	m.cursor = len(m.rows) - 1
	if _, _, ok := m.selected(); ok || m.paneIndex() != 1 {
		t.Fatal("the cursor cannot rest in the empty tool's section")
	}
	next, cmd := m.askSwitch()
	if next.(Model).modal != nil || cmd != nil {
		t.Error("enter on the placeholder opened a switch")
	}
	next, cmd = m.askAdd()
	if next.(Model).busy == "" || cmd == nil {
		t.Error("a on the placeholder did not probe that tool's login")
	}
}

// Every message about a tool says which one, and the prompt that follows
// names it: the cursor may have moved while the probe ran.
func TestAnAddPromptNamesTheToolItIsFor(t *testing.T) {
	m := bothTools(t)
	next, _ := m.handleLiveProbed(liveProbedMsg{pane: 1, state: swap.LiveState{
		LoggedIn: true, Identity: swap.LiveIdentity{Email: "new@example.com"}}})
	if title := next.(Model).modal.title; !strings.Contains(title, "Codex") {
		t.Errorf("the prompt does not name Codex:\n%s", title)
	}
}

// A token key pressed in a tool that declares no token format is refused by
// name, with the reason: the key was on the help screen, and a key that does
// nothing reads as a freeze.
func TestATokenIsRefusedWhereTheToolDeclaresNone(t *testing.T) {
	m := bothTools(t)
	m.cursor = 2 // the Codex account
	next, _ := m.askAddToken()
	model := next.(Model)
	if model.modal != nil {
		t.Fatal("a token field opened for a tool that cannot store one")
	}
	if !model.statusErr || !strings.Contains(model.status, "codex") {
		t.Errorf("status = %q, want the declared reason", model.status)
	}
}

// n asks which tool the login is for, with the cursor's tool marked — the
// same rule as `aaswap login`: nothing on screen says which tool the NEXT
// account is for, and the person adding their first Codex account has the
// cursor wherever it was left.
func TestNAsksWhichToolWithTheCursorsToolMarked(t *testing.T) {
	m := bothTools(t)
	m.cursor = 2 // in the Codex section
	m, cmd := typed(m, "n")
	if m.modal == nil || m.modal.kind != modalPick || cmd != nil {
		t.Fatal("n did not ask which tool")
	}
	if m.modal.pick != 1 {
		t.Errorf("the marked row is %d, want the Codex section the cursor is in", m.modal.pick)
	}
	frame := m.View().Content
	for _, want := range []string{"Which tool is this login for?", "Claude Code", "Codex", "2 accounts stored", "1 account stored"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the question is missing %q:\n%s", want, frame)
		}
	}
	closed, cmd := typed(m, "esc")
	if cmd != nil || closed.modal != nil {
		t.Error("esc did not simply close the question")
	}
}

// Answering — by enter or by the row's digit — opens that tool's login.
func TestAnsweringTheLoginQuestionOpensThatToolsLogin(t *testing.T) {
	for _, key := range []string{"enter", "1"} {
		m := bothTools(t)
		m, _ = typed(m, "n")
		m, cmd := typed(m, key)
		if m.modal != nil || m.busy != "opening a login" || cmd == nil {
			t.Errorf("%q: busy = %q with cmd %v, want a login being opened", key, m.busy, cmd)
		}
	}
}

// With one tool there is nothing to ask, and n goes straight to the login.
func TestNWithOneToolDoesNotAsk(t *testing.T) {
	m := twoAccounts(t)
	m, cmd := typed(m, "n")
	if m.modal != nil || m.busy != "opening a login" || cmd == nil {
		t.Errorf("busy = %q with modal %v, want a login being opened without a question", m.busy, m.modal)
	}
}

// Run refuses a configuration that could show nothing.
func TestRunNeedsATool(t *testing.T) {
	if err := Run(t.Context(), Options{}); err == nil {
		t.Error("Run accepted no tool at all")
	}
}
