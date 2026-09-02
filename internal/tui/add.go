package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/swap"
)

// awaitFrameInterval is how fast the waiting modal's marker advances.
//
// Purely cosmetic, and that is the point: a wait for a person to go and log in
// somewhere else can run for minutes, and a screen that never changes reads as
// a hang.
const awaitFrameInterval = 400 * time.Millisecond

// awaitFrames is the marker's cycle.
var awaitFrames = [...]string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// --- messages ---------------------------------------------------------------

// liveProbedMsg carries who the machine is logged in as.
//
// A separate round trip rather than a read inside the key handler: opening the
// live config is disk work, and the UI loop does none.
type liveProbedMsg struct {
	state swap.LiveState
	err   error
}

// addedMsg carries a finished capture, token registration, or awaited login.
type addedMsg struct {
	outcome swap.AddOutcome
	// awaited marks the result of a wait, whose cancellation is a normal
	// outcome rather than a failure to report.
	awaited bool
	err     error
}

// awaitTickMsg advances the waiting modal's marker.
type awaitTickMsg time.Time

// --- commands ---------------------------------------------------------------

func probeLiveCmd(s *swap.Switcher) tea.Cmd {
	return func() tea.Msg {
		state, err := s.LiveState()
		return liveProbedMsg{state: state, err: err}
	}
}

func addCmd(s *swap.Switcher) tea.Cmd {
	return func() tea.Msg {
		outcome, err := s.Add(context.Background(), swap.AddRequest{})
		return addedMsg{outcome: outcome, err: err}
	}
}

func addTokenCmd(s *swap.Switcher, token string) tea.Cmd {
	return func() tea.Msg {
		outcome, err := s.AddToken(swap.AddTokenRequest{Token: token})
		return addedMsg{outcome: outcome, err: err}
	}
}

// awaitAddCmd waits for a login and captures it, as one command.
//
// Both halves belong to the same goroutine because they are one operation: the
// only reason to wait is to capture what lands, and handing the wait's result
// back to the UI loop to start a second command would put a scheduling gap
// between the login and the read of it.
func awaitAddCmd(ctx context.Context, s *swap.Switcher) tea.Cmd {
	return func() tea.Msg {
		if _, err := s.AwaitNewLogin(ctx, swap.AwaitOptions{}); err != nil {
			return addedMsg{awaited: true, err: err}
		}
		outcome, err := s.Add(ctx, swap.AddRequest{})
		return addedMsg{outcome: outcome, awaited: true, err: err}
	}
}

func awaitTickCmd() tea.Cmd {
	return tea.Tick(awaitFrameInterval, func(t time.Time) tea.Msg { return awaitTickMsg(t) })
}

// --- key handlers -----------------------------------------------------------

// askAdd starts the capture of whatever is logged in now.
//
// It probes first and asks second. aaswap's `add` decides between registering a
// new slot and refreshing an existing one from the live identity alone, and a
// prompt that cannot say which of those it is about is not a prompt worth
// showing.
func (m Model) askAdd() (tea.Model, tea.Cmd) {
	if m.busy != "" || m.awaitCancel != nil {
		return m, nil
	}
	m.busy = "reading the live login"
	return m, probeLiveCmd(m.switcher)
}

// handleLiveProbed turns what the probe found into the question to ask.
func (m Model) handleLiveProbed(msg liveProbedMsg) (tea.Model, tea.Cmd) {
	m.busy = ""
	if msg.err != nil {
		m.status, m.statusErr = msg.err.Error(), true
		return m, clearStatusCmd()
	}
	if !msg.state.LoggedIn {
		// Nothing to capture and nothing to ask about. The wait IS the answer
		// to "add an account" on a machine with no login.
		return m.startAwait()
	}

	st := m.styles
	identity := msg.state.Identity
	body := []string{
		"",
		"  " + st.email.Render(identity.Email) + st.tag.Render("  ["+identity.DisplayTag()+"]"),
		"",
	}
	title := "Add the account you are logged in as?"
	if slot := msg.state.Slot; slot != "" {
		title = fmt.Sprintf("Refresh Account %s from the live login?", slot)
		body = append(body, st.muted.Render(
			"  Replaces the stored credential for "+slot+". Nothing else changes."))
	} else {
		body = append(body, st.muted.Render(
			"  Stores its credential as a new account."))
	}
	body = append(body, "",
		st.muted.Render("  To add a DIFFERENT account, press esc and then ")+
			st.accent.Render("n")+st.muted.Render("."))

	m.modal = &modal{
		kind: modalConfirm, title: title, body: body,
		busyLabel: "adding", run: addCmd(m.switcher),
	}
	return m, nil
}

// startAwait waits for a login to land, then captures it.
//
// This is the closest aaswap gets to logging anyone in. Claude Code owns the
// OAuth flow, so the person has to go and run /login somewhere else — but they
// do not have to come back, quit, and re-run anything: the dashboard is
// watching and captures the account the moment it appears.
func (m Model) startAwait() (tea.Model, tea.Cmd) {
	if m.busy != "" || m.awaitCancel != nil {
		return m, nil
	}
	// Background rather than the program's context: cancellation here is the
	// esc key, and it is held as the cancel func below. Quitting cancels it on
	// the way out.
	ctx, cancel := context.WithCancel(context.Background())
	m.awaitCancel = cancel
	m.awaitFrame = 0
	m.modal = m.waitingModal()
	return m, tea.Batch(awaitAddCmd(ctx, m.switcher), awaitTickCmd())
}

// waitingModal is the instruction sheet shown while waiting.
func (m Model) waitingModal() *modal {
	st := m.styles
	// The instruction is the declaration's, not Claude's: "claude and then
	// /login" told a Codex user to run a command Codex does not have.
	launch, then := m.spec.Login.Steps()
	var instruction string
	switch {
	case launch == "":
		instruction = st.muted.Render("  In another terminal, log in with " + m.spec.DisplayName() + ",")
	case then == "":
		instruction = st.muted.Render("  In another terminal, run ") + st.accent.Render(launch) +
			st.muted.Render(",")
	default:
		instruction = st.muted.Render("  In another terminal, run ") + st.accent.Render(launch) +
			st.muted.Render(" and then ") + st.accent.Render(then) + st.muted.Render(",")
	}
	return &modal{
		kind:  modalWaiting,
		title: awaitFrames[m.awaitFrame%len(awaitFrames)] + " Waiting for a login",
		body: []string{
			"",
			instruction,
			st.muted.Render("  with the account you want to add."),
			"",
			st.yellow.Render("  Do not log out first — the tool may revoke the token"),
			st.yellow.Render("  stored for the account you are leaving."),
			"",
			st.muted.Render("  The account is captured as soon as the login finishes."),
		},
	}
}

// askAddToken opens the field for a pasted token.
//
// Refused where the provider declares no token format: aaswap would have to
// invent both the recognition and the stored shape, and the shape it would
// invent is Claude's — which, written into another tool's credential file,
// replaces a working login with one that tool cannot read.
func (m Model) askAddToken() (tea.Model, tea.Cmd) {
	if m.busy != "" || m.awaitCancel != nil {
		return m, nil
	}
	if !m.spec.Can(provider.CapToken) {
		// Said rather than ignored: the key was pressed because the help
		// screen offered it, and a key that does nothing reads as a freeze.
		m.status, m.statusErr = m.spec.Why(provider.CapToken), true
		return m, nil
	}
	st := m.styles
	m.modal = &modal{
		kind:        modalInput,
		title:       "Add a token",
		placeholder: m.spec.Token.Hint(),
		body: []string{
			"",
			st.muted.Render("  A token this tool accepts. Nothing is sent anywhere: the"),
			st.muted.Render("  kind is read off the value itself."),
		},
		hint:      tokenKindNote,
		busyLabel: "adding",
		submit:    func(token string) tea.Cmd { return addTokenCmd(m.switcher, token) },
	}
	return m, nil
}

// handleInputKey types into the open field.
func (m Model) handleInputKey(key tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	md := m.modal
	switch {
	case key.String() == "esc":
		m.modal = nil
		return m, nil
	case key.String() == "ctrl+u":
		md.input = ""
		return m, nil
	case key.String() == "backspace":
		runes := []rune(md.input)
		if len(runes) > 0 {
			md.input = string(runes[:len(runes)-1])
		}
		return m, nil
	case key.String() == "enter":
		token := strings.TrimSpace(md.input)
		if token == "" {
			return m, nil
		}
		cmd := md.submit(token)
		m.modal = nil
		m.busy = md.busyLabel
		return m, cmd
	case key.Text != "":
		// Text is only non-empty for printable input, which is exactly what a
		// token is made of — and it carries a whole pasted run in one message
		// rather than a keypress per character.
		md.input += key.Text
		return m, nil
	}
	return m, nil
}

// --- result handling --------------------------------------------------------

func (m Model) handleAdded(msg addedMsg) (tea.Model, tea.Cmd) {
	m.busy = ""
	if msg.awaited {
		m.stopAwait()
		m.modal = nil
	}
	if msg.err != nil {
		// A cancelled wait is what esc does. Reporting it as a failure would
		// make the escape hatch look like a fault.
		if msg.awaited && errors.Is(msg.err, context.Canceled) {
			m.status, m.statusErr = "Stopped waiting", false
			return m, clearStatusCmd()
		}
		m.modal = &modal{
			kind: modalNotice, danger: true, title: "Add failed",
			body: []string{"", m.styles.red.Render(msg.err.Error())},
		}
		return m, nil
	}
	if msg.outcome.Cancelled {
		m.status, m.statusErr = "Cancelled", false
		return m, clearStatusCmd()
	}

	verb := "Added"
	if msg.outcome.Refreshed {
		verb = "Refreshed"
	}
	m.status = fmt.Sprintf("%s Account %s (%s)", verb, msg.outcome.Name, msg.outcome.Email)
	m.statusErr = false
	if msg.outcome.Unverified != "" {
		// Never silently. Registering with the ownership question unanswered is
		// allowed, but a person who is not told cannot act on it.
		m.modal = &modal{
			kind: modalNotice, title: "Added, but unverified",
			body: []string{
				"",
				m.styles.yellow.Width(56).Render(fmt.Sprintf(
					"Could not confirm that the stored credential belongs to %s (%s). "+
						"Re-run where the check can complete to confirm.",
					msg.outcome.Email, msg.outcome.Unverified)),
			},
		}
	}
	// The roster gained or changed a slot, so every row on screen is stale.
	m.busy = "collecting"
	return m, tea.Batch(collectCmd(m.switcher), clearStatusCmd())
}

// stopAwait tears down a wait in flight. Safe to call when none is running.
func (m *Model) stopAwait() {
	if m.awaitCancel == nil {
		return
	}
	m.awaitCancel()
	m.awaitCancel = nil
}

// tokenKindNote names what a pasted value will register as, for the field's
// hint line.
func tokenKindNote(token string) string {
	switch {
	case token == "":
		return ""
	case credstore.LooksLikeAPIKey(token):
		return "managed API key"
	default:
		return "OAuth setup token"
	}
}
