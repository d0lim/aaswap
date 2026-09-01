package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/d0lim/aaswap/internal/swap"
)

// errBoom stands in for any failure the store can return.
var errBoom = errors.New("the store lock is held")

func press(s string) tea.KeyPressMsg {
	if s == "ctrl+c" {
		return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
	}
	if runes := []rune(s); len(runes) == 1 {
		return tea.KeyPressMsg{Code: runes[0], Text: s}
	}
	return tea.KeyPressMsg{Code: keyCodeFor(s)}
}

// keyCodeFor names the non-printable keys these tests press.
func keyCodeFor(name string) rune {
	switch name {
	case "enter":
		return tea.KeyEnter
	case "esc":
		return tea.KeyEscape
	case "backspace":
		return tea.KeyBackspace
	}
	panic("unknown key " + name)
}

// The UI loop performs no disk work, so `a` cannot read the live config
// itself — it has to hand that to a command and wait for the answer.
func TestAddProbesTheLiveLoginOffTheLoop(t *testing.T) {
	m := twoAccounts(t)
	next, cmd := m.handleKey(press("a"))
	if cmd == nil {
		t.Fatal("a started no probe")
	}
	if got := next.(Model).busy; got == "" {
		t.Error("the probe left the header idle, so the dashboard looks unresponsive")
	}
	if next.(Model).modal != nil {
		t.Error("a modal opened before the probe answered, so it cannot say what it is about")
	}
}

// The prompt has to say which of add's two outcomes this is. A person cannot
// consent to "something happens to some slot".
func TestTheAddPromptNamesWhatWillHappen(t *testing.T) {
	tests := []struct {
		name  string
		state swap.LiveState
		want  []string
	}{
		{
			name: "an account with no slot is a new registration",
			state: swap.LiveState{LoggedIn: true, Identity: swap.LiveIdentity{
				Email: "new@example.com", OrganizationName: "Acme"}},
			want: []string{"Add the account", "new@example.com", "Acme", "new slot"},
		},
		{
			name: "an account already in a slot is a refresh of that slot",
			state: swap.LiveState{LoggedIn: true, Slot: "2", Identity: swap.LiveIdentity{
				Email: "spare@example.com"}},
			want: []string{"Refresh Account 2", "spare@example.com", "stored credential"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := twoAccounts(t)
			next, _ := m.handleLiveProbed(liveProbedMsg{state: tt.state})
			model := next.(Model)
			if model.modal == nil || model.modal.kind != modalConfirm {
				t.Fatalf("modal = %+v, want a confirmation", model.modal)
			}
			frame := m.renderModal(model.modal)
			for _, want := range tt.want {
				if !strings.Contains(frame, want) {
					t.Errorf("the prompt does not say %q:\n%s", want, frame)
				}
			}
		})
	}
}

// With nothing logged in there is nothing to confirm and nothing to capture.
// Asking "add the account you are logged in as?" would be asking about nothing.
func TestAddWithNoLoginGoesStraightToWaiting(t *testing.T) {
	m := twoAccounts(t)
	next, cmd := m.handleLiveProbed(liveProbedMsg{state: swap.LiveState{}})
	model := next.(Model)
	if model.modal == nil || model.modal.kind != modalWaiting {
		t.Fatalf("modal = %+v, want the waiting modal", model.modal)
	}
	if model.awaitCancel == nil {
		t.Error("nothing is waiting, so nothing will ever capture the login")
	}
	if cmd == nil {
		t.Error("the wait started no command")
	}
	model.stopAwait()
}

// The instructions are the whole content of the wait: the person has to leave
// to do the thing, and the one way to do it wrong destroys a stored token.
func TestTheWaitingModalSaysWhatToDoAndWhatNotTo(t *testing.T) {
	m := twoAccounts(t)
	frame := m.renderModal(m.waitingModal())
	for _, want := range []string{"claude", "/login", "Do not run /logout first"} {
		if !strings.Contains(frame, want) {
			t.Errorf("the waiting modal does not say %q:\n%s", want, frame)
		}
	}
}

// A wait outlives the keypress that began it, so both exits have to end it:
// esc, and quitting the dashboard outright.
func TestAWaitEndsOnEscapeAndOnQuit(t *testing.T) {
	tests := []struct{ name, key string }{
		{"esc stops waiting", "esc"},
		{"ctrl+c stops waiting", "ctrl+c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := twoAccounts(t)
			started, _ := m.startAwait()
			model := started.(Model)

			next, _ := model.handleKey(press(tt.key))
			if next.(Model).awaitCancel != nil {
				t.Error("the wait survived, and its goroutine keeps polling the config")
			}
		})
	}
}

// Only esc. Cancelling a wait someone means to keep, because they pressed a key
// while reading it, loses the login they were about to make.
func TestAnUnrelatedKeyDoesNotCancelTheWait(t *testing.T) {
	m := twoAccounts(t)
	started, _ := m.startAwait()
	model := started.(Model)

	next, _ := model.handleKey(press("j"))
	if next.(Model).awaitCancel == nil {
		t.Error("j cancelled the wait")
	}
	surviving := next.(Model)
	surviving.stopAwait()
}

// A cancelled wait is what esc does. Reporting it as a failure would make the
// escape hatch look like a fault.
func TestACancelledWaitIsNotAnError(t *testing.T) {
	m := twoAccounts(t)
	next, _ := m.handleAdded(addedMsg{awaited: true, err: context.Canceled})
	model := next.(Model)
	if model.modal != nil {
		t.Errorf("modal = %+v, want no error modal", model.modal)
	}
	if model.statusErr {
		t.Errorf("status %q was marked an error", model.status)
	}
}

// A real failure must not be swallowed by the same path.
func TestAFailedAddIsReported(t *testing.T) {
	m := twoAccounts(t)
	next, _ := m.handleAdded(addedMsg{awaited: true, err: errBoom})
	model := next.(Model)
	if model.modal == nil || model.modal.kind != modalNotice || !model.modal.danger {
		t.Fatalf("modal = %+v, want a failure notice", model.modal)
	}
}

// Registering with the ownership question unanswered is allowed, but never
// silently: a person who is not told cannot act on it.
func TestAnUnverifiedAddSaysSo(t *testing.T) {
	m := twoAccounts(t)
	next, _ := m.handleAdded(addedMsg{outcome: swap.AddOutcome{
		Number: "3", Email: "new@example.com", Unverified: "the lookup did not resolve",
	}})
	model := next.(Model)
	if model.modal == nil || model.modal.kind != modalNotice {
		t.Fatalf("modal = %+v, want a notice", model.modal)
	}
	if frame := m.renderModal(model.modal); !strings.Contains(frame, "new@example.com") {
		t.Errorf("the notice does not name the account:\n%s", frame)
	}
}

func TestTheTokenField(t *testing.T) {
	m := twoAccounts(t)
	opened, _ := m.askAddToken()
	model := opened.(Model)
	if model.modal == nil || model.modal.kind != modalInput {
		t.Fatalf("modal = %+v, want an input field", model.modal)
	}

	t.Run("an empty field submits nothing", func(t *testing.T) {
		_, cmd := model.handleInputKey(press("enter"))
		if cmd != nil {
			t.Error("enter on an empty field submitted")
		}
	})

	t.Run("typing, backspace and clear", func(t *testing.T) {
		typed := model
		// Text carries a whole pasted run in one message, which is how a token
		// actually arrives.
		typed, _ = mustModel(typed.handleInputKey(tea.KeyPressMsg{Text: "sk-ant-oat01-abc"}))
		typed, _ = mustModel(typed.handleInputKey(press("backspace")))
		if got := typed.modal.input; got != "sk-ant-oat01-ab" {
			t.Errorf("input = %q after a backspace", got)
		}
		typed, _ = mustModel(typed.handleInputKey(tea.KeyPressMsg{Code: 'u', Mod: tea.ModCtrl}))
		if got := typed.modal.input; got != "" {
			t.Errorf("input = %q after ctrl+u", got)
		}
	})

	t.Run("esc closes the field", func(t *testing.T) {
		next, _ := model.handleInputKey(press("esc"))
		if next.(Model).modal != nil {
			t.Error("esc left the field open")
		}
	})
}

// The field masks a live credential, but never the kind marker: the prefix is
// what decides whether this becomes an OAuth slot or an API-key slot, and a
// field that hides it leaves no way to see the wrong thing was pasted.
func TestTheTokenFieldMasksTheSecretButNotItsKind(t *testing.T) {
	md := &modal{kind: modalInput, input: "sk-ant-api03-supersecretvalue", hint: tokenKindNote}
	shown := md.visibleInput()
	if !strings.HasPrefix(shown, "sk-ant-api03-") {
		t.Errorf("visibleInput = %q, want the kind marker readable", shown)
	}
	if strings.Contains(shown, "supersecret") {
		t.Errorf("visibleInput = %q, want the secret masked", shown)
	}
	if got := md.hint(md.input); got != "managed API key" {
		t.Errorf("hint = %q, want it to name the kind", got)
	}
	if got := tokenKindNote("sk-ant-oat01-abc"); got != "OAuth setup token" {
		t.Errorf("hint = %q for a setup token", got)
	}
}

// The hint bar is one line. On a terminal too narrow for every key it drops
// them rather than wrapping, which would push an account off a short screen —
// but never the way out and never the way to read what it dropped.
func TestTheHintBarShedsKeysButNeverTheWayOut(t *testing.T) {
	for _, width := range []int{120, 80, 40, 20} {
		m := twoAccounts(t)
		m.width = width
		bar := m.hintBar()
		if lipgloss.Width(bar) > width-1 && width > 20 {
			t.Errorf("at width %d the bar is %d wide:\n%s", width, lipgloss.Width(bar), bar)
		}
		for _, pinned := range []string{"quit", "help"} {
			if !strings.Contains(bar, pinned) {
				t.Errorf("at width %d the bar dropped %s:\n%s", width, pinned, bar)
			}
		}
	}
}

// While one credential operation is in flight, another must not start.
func TestAddIsRefusedWhileSomethingElseRuns(t *testing.T) {
	m := twoAccounts(t)
	m.busy = "switching"
	for _, key := range []string{"a", "n", "t"} {
		next, cmd := m.handleKey(press(key))
		if cmd != nil || next.(Model).modal != nil {
			t.Errorf("%q started a second operation mid-switch", key)
		}
	}
}

// A wait blocks the same way: two waits would both capture, and the second
// would act on a roster the first had already changed.
func TestAddIsRefusedWhileAWaitRuns(t *testing.T) {
	m := twoAccounts(t)
	started, _ := m.startAwait()
	model := started.(Model)
	defer model.stopAwait()

	next, cmd := model.askAdd()
	if cmd != nil || next.(Model).busy != "" {
		t.Error("a capture started while a wait was already running")
	}
}

func mustModel(model tea.Model, cmd tea.Cmd) (Model, tea.Cmd) { return model.(Model), cmd }

// ctrl+c is the one key that must work from anywhere. The waiting modal takes
// only esc by design, and an overlay that can swallow the interrupt is a
// dashboard someone cannot get out of.
func TestCtrlCQuitsFromEveryOverlay(t *testing.T) {
	tests := []struct {
		name string
		open func(Model) Model
	}{
		{"from the list", func(m Model) Model { return m }},
		{"from help", func(m Model) Model { m.showHelp = true; return m }},
		{"from a confirmation", func(m Model) Model {
			next, _ := m.askSwitch()
			return next.(Model)
		}},
		{"from the token field", func(m Model) Model {
			next, _ := m.askAddToken()
			return next.(Model)
		}},
		{"from a wait", func(m Model) Model {
			next, _ := m.startAwait()
			return next.(Model)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := tt.open(twoAccounts(t))
			m.cursor = 1
			next, cmd := m.handleKey(press("ctrl+c"))
			if cmd == nil || !next.(Model).quitting {
				t.Error("ctrl+c did not quit")
			}
			model := next.(Model)
			model.stopAwait()
		})
	}
}
