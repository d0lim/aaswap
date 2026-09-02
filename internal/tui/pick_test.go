package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/render"
	"github.com/d0lim/aaswap/internal/swap"
)

var bothTools = []ProviderChoice{
	{Name: provider.Claude, Label: "Claude Code", Accounts: 2},
	{Name: provider.Codex, Label: "Codex", Accounts: 1},
}

// opening records which provider the picker asked for and hands back a
// switcher for it.
func opening(t *testing.T) (func(string) (*swap.Switcher, error), *[]string) {
	t.Helper()
	var opened []string
	return func(name string) (*swap.Switcher, error) {
		opened = append(opened, name)
		return &swap.Switcher{Provider: name}, nil
	}, &opened
}

// unpointed is a dashboard started with no tool chosen.
func unpointed(t *testing.T) Model {
	t.Helper()
	open, _ := opening(t)
	m := NewModel(Options{Theme: render.Dark, Providers: bothTools, Open: open})
	m.width, m.height = 76, 24
	return m
}

// typed presses one key and hands back the model.
func typed(m Model, key string) (Model, tea.Cmd) {
	next, cmd := m.Update(press(key))
	return next.(Model), cmd
}

// With no tool chosen the first frame is the question, not an empty Claude
// dashboard — which is what someone who only uses Codex used to get.
func TestTheDashboardOpensOnTheQuestionWhenNoToolIsChosen(t *testing.T) {
	m := unpointed(t)
	msg := m.Init()()
	next, _ := m.Update(msg)
	m = next.(Model)

	frame := m.View().Content
	for _, want := range []string{"Which tool?", "Claude Code", "Codex", "2 accounts stored", "1 account stored"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the opening frame is missing %q:\n%s", want, frame)
		}
	}
	if m.modal == nil || !m.modal.cancelQuits {
		t.Fatal("the opening question must be one esc leaves the program from: there is nothing behind it")
	}
	if _, cmd := typed(m, "esc"); cmd == nil {
		t.Error("esc on the opening question did not quit")
	}
}

// Answering points the dashboard at the chosen tool and collects for it.
func TestAnsweringTheQuestionOpensThatTool(t *testing.T) {
	open, opened := opening(t)
	m := NewModel(Options{Theme: render.Dark, Providers: bothTools, Open: open})
	next, _ := m.Update(askProviderMsg{})
	m = next.(Model)

	m, _ = typed(m, "j") // down to Codex
	m, cmd := typed(m, "enter")
	if cmd == nil {
		t.Fatal("enter produced no command")
	}
	next, cmd = m.Update(cmd())
	m = next.(Model)
	if *opened == nil || (*opened)[0] != provider.Codex {
		t.Fatalf("opened %v, want codex", *opened)
	}
	if m.spec.Name != provider.Codex {
		t.Errorf("spec = %q, want codex", m.spec.Name)
	}
	if cmd == nil || m.busy != "collecting" {
		t.Errorf("busy = %q with cmd %v, want a collect for the new tool", m.busy, cmd)
	}
	if frame := m.View().Content; !strings.Contains(frame, "Codex") {
		t.Errorf("the header does not name the tool shown:\n%s", frame)
	}
}

// A digit answers too: the rows are numbered for exactly that.
func TestADigitAnswersTheQuestion(t *testing.T) {
	open, opened := opening(t)
	m := NewModel(Options{Theme: render.Dark, Providers: bothTools, Open: open})
	next, _ := m.Update(askProviderMsg{})
	_, cmd := typed(next.(Model), "2")
	if cmd == nil {
		t.Fatal("2 produced no command")
	}
	cmd()
	if *opened == nil || (*opened)[0] != provider.Codex {
		t.Errorf("opened %v, want codex", *opened)
	}
}

// From a dashboard, p re-asks — with the current tool marked, and esc going
// back to the dashboard rather than out of the program.
func TestPReAsksWithTheCurrentToolMarked(t *testing.T) {
	m := twoAccounts(t)
	m.providers = bothTools
	m.open, _ = opening(t)

	m, _ = typed(m, "p")
	if m.modal == nil || m.modal.kind != modalPick {
		t.Fatal("p did not open the picker")
	}
	if m.modal.cancelQuits {
		t.Error("esc from a dashboard's picker must go back to the dashboard")
	}
	frame := m.View().Content
	if !strings.Contains(frame, "shown now") {
		t.Errorf("the current tool is not marked:\n%s", frame)
	}
	m, cmd := typed(m, "esc")
	if cmd != nil || m.modal != nil {
		t.Error("esc did not simply close the picker")
	}
	if !strings.Contains(m.hintBar(), "tool") || !strings.Contains(m.renderHelp(), "another tool") {
		t.Error("the key is not on the footer and the help screen")
	}
}

// With one tool there is nothing to choose, and the key is not offered.
func TestPDoesNothingWithOneTool(t *testing.T) {
	m := twoAccounts(t)
	m.providers = bothTools[:1]
	m.open, _ = opening(t)
	m, _ = typed(m, "p")
	if m.modal != nil {
		t.Error("p opened a picker over a single choice")
	}
	if strings.Contains(m.hintBar(), "tool") || strings.Contains(m.renderHelp(), "another tool") {
		t.Error("a key that does nothing is offered")
	}
}

// A tool that cannot be opened is said, and the question stays.
func TestAFailedOpenReturnsToTheQuestion(t *testing.T) {
	m := unpointed(t)
	next, _ := m.Update(askProviderMsg{})
	m = next.(Model)
	next, _ = m.Update(providerOpenedMsg{name: provider.Codex, err: errBoom})
	m = next.(Model)
	if m.modal == nil || m.modal.kind != modalPick {
		t.Fatal("the question did not come back")
	}
	if !m.statusErr || !strings.Contains(m.status, "store lock") {
		t.Errorf("status = %q, want the reason", m.status)
	}
}

// Run refuses a configuration that could show nothing.
func TestRunNeedsASwitcherOrAChoice(t *testing.T) {
	if err := Run(t.Context(), Options{Providers: bothTools[:1]}); err == nil {
		t.Error("Run accepted neither a switcher nor a choice")
	}
}
