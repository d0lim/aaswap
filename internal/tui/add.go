package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "charm.land/bubbletea/v2"

	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/swap"
)

// --- messages ---------------------------------------------------------------
//
// Each carries the pane it is about. With every tool on one screen, a result
// that did not say which tool it came from could only be matched to the
// cursor — which may have moved while the work ran.

// liveProbedMsg carries who the machine is logged in as, for one tool.
//
// A separate round trip rather than a read inside the key handler: opening the
// live config is disk work, and the UI loop does none.
type liveProbedMsg struct {
	pane  int
	state swap.LiveState
	err   error
}

// addedMsg carries a finished capture, token registration, or sandboxed login.
type addedMsg struct {
	pane    int
	outcome swap.AddOutcome
	err     error
}

// loginBegunMsg carries an opened login sandbox, ready for the tool.
type loginBegunMsg struct {
	pane    int
	sandbox *swap.LoginSandbox
	err     error
}

// loginRanMsg carries the tool's exit after a login was run into a sandbox.
type loginRanMsg struct {
	pane    int
	sandbox *swap.LoginSandbox
	err     error
}

// --- commands ---------------------------------------------------------------

func probeLiveCmd(s *swap.Switcher, index int) tea.Cmd {
	return func() tea.Msg {
		state, err := s.LiveState()
		return liveProbedMsg{pane: index, state: state, err: err}
	}
}

func addCmd(s *swap.Switcher, index int) tea.Cmd {
	return func() tea.Msg {
		outcome, err := s.Add(context.Background(), swap.AddRequest{})
		return addedMsg{pane: index, outcome: outcome, err: err}
	}
}

func addTokenCmd(s *swap.Switcher, index int, token string) tea.Cmd {
	return func() tea.Msg {
		outcome, err := s.AddToken(swap.AddTokenRequest{Token: token})
		return addedMsg{pane: index, outcome: outcome, err: err}
	}
}

func beginLoginCmd(s *swap.Switcher, index int) tea.Cmd {
	return func() tea.Msg {
		sandbox, err := s.BeginLogin()
		return loginBegunMsg{pane: index, sandbox: sandbox, err: err}
	}
}

func finishLoginCmd(s *swap.Switcher, index int, sandbox *swap.LoginSandbox) tea.Cmd {
	return func() tea.Msg {
		outcome, err := s.FinishLogin(context.Background(), sandbox, swap.AddRequest{})
		return addedMsg{pane: index, outcome: outcome, err: err}
	}
}

// --- key handlers -----------------------------------------------------------

// askAdd starts the capture of whatever is logged in now, for the tool the
// cursor is on.
//
// The cursor decides here where `n` asks, because the subject is different:
// a login is a NEW account for some tool, and nothing on screen says which;
// a capture is the account some tool is already logged in as, and the
// section the cursor sits in names that tool. The prompt that follows names
// it too, so a capture pointed at the wrong section is refused there rather
// than discovered later.
//
// It probes first and asks second. aaswap's `add` decides between registering a
// new slot and refreshing an existing one from the live identity alone, and a
// prompt that cannot say which of those it is about is not a prompt worth
// showing.
func (m Model) askAdd() (tea.Model, tea.Cmd) {
	if m.busy != "" {
		return m, nil
	}
	index := m.paneIndex()
	m.busy = "reading the live login"
	return m, probeLiveCmd(m.panes[index].switcher, index)
}

// handleLiveProbed turns what the probe found into the question to ask.
func (m Model) handleLiveProbed(msg liveProbedMsg) (tea.Model, tea.Cmd) {
	m.busy = ""
	if msg.err != nil {
		m.status, m.statusErr = msg.err.Error(), true
		return m, clearStatusCmd()
	}
	if !msg.state.LoggedIn {
		// Nothing to capture and nothing to ask about. Logging in IS the
		// answer to "add an account" on a machine with no login.
		return m.startLogin(msg.pane)
	}

	st := m.styles
	p := m.panes[msg.pane]
	identity := msg.state.Identity
	body := []string{
		"",
		"  " + st.email.Render(identity.Email) + st.tag.Render("  ["+identity.DisplayTag()+"]"),
		"",
	}
	title := fmt.Sprintf("Add the %s account you are logged in as?", p.spec.DisplayName())
	if slot := msg.state.Slot; slot != "" {
		title = fmt.Sprintf("Refresh %s Account %s from the live login?", p.spec.DisplayName(), slot)
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
		busyLabel: "adding", run: addCmd(p.switcher, msg.pane),
	}
	return m, nil
}

// startLogin logs a new account in for one tool: the tool's own login, run
// into a sandbox, and what lands there stored.
//
// The dashboard hands the terminal to the tool for the duration — a browser
// login has a URL to print and possibly a code to paste — and takes it back
// when the tool exits. The login the person already has is never touched.
func (m Model) startLogin(index int) (tea.Model, tea.Cmd) {
	if m.busy != "" {
		return m, nil
	}
	m.busy = "opening a login"
	return m, beginLoginCmd(m.panes[index].switcher, index)
}

// handleLoginBegun runs the tool into the sandbox that was opened.
func (m Model) handleLoginBegun(msg loginBegunMsg) (tea.Model, tea.Cmd) {
	m.busy = ""
	if msg.err != nil {
		return m.failed("Login failed", msg.err)
	}
	argv := msg.sandbox.Argv()
	binary, err := exec.LookPath(argv[0])
	if err != nil {
		msg.sandbox.Discard()
		return m.failed("Login failed", fmt.Errorf("`%s` was not found on your PATH", argv[0]))
	}
	tool := exec.Command(binary, argv[1:]...)
	tool.Env = msg.sandbox.Environment(os.Environ())
	m.busy = "logging in"
	sandbox, index := msg.sandbox, msg.pane
	return m, m.execTool(tool, func(err error) tea.Msg {
		return loginRanMsg{pane: index, sandbox: sandbox, err: err}
	})
}

// handleLoginRan files what the tool left, or says why it cannot.
func (m Model) handleLoginRan(msg loginRanMsg) (tea.Model, tea.Cmd) {
	m.busy = ""
	if msg.err != nil {
		msg.sandbox.Discard()
		return m.failed("Login failed", fmt.Errorf("%s did not complete: %w",
			strings.Join(msg.sandbox.Argv(), " "), msg.err))
	}
	m.busy = "storing the login"
	return m, finishLoginCmd(m.panes[msg.pane].switcher, msg.pane, msg.sandbox)
}

// failed shows a failure notice.
func (m Model) failed(title string, err error) (tea.Model, tea.Cmd) {
	m.modal = &modal{
		kind: modalNotice, danger: true, title: title,
		body: []string{"", m.styles.red.Render(err.Error())},
	}
	return m, nil
}

// askAddToken opens the field for a pasted token, for the tool the cursor is
// on.
//
// Refused where the provider declares no token format: aaswap would have to
// invent both the recognition and the stored shape, and the shape it would
// invent is Claude's — which, written into another tool's credential file,
// replaces a working login with one that tool cannot read.
func (m Model) askAddToken() (tea.Model, tea.Cmd) {
	if m.busy != "" {
		return m, nil
	}
	index := m.paneIndex()
	p := m.panes[index]
	if !p.spec.Can(provider.CapToken) {
		// Said rather than ignored: the key was pressed because the help
		// screen offered it, and a key that does nothing reads as a freeze.
		m.status, m.statusErr = p.spec.Why(provider.CapToken), true
		return m, nil
	}
	st := m.styles
	m.modal = &modal{
		kind:        modalInput,
		title:       "Add a " + p.spec.DisplayName() + " token",
		placeholder: p.spec.Token.Hint(),
		body: []string{
			"",
			st.muted.Render("  A token this tool accepts. Nothing is sent anywhere: the"),
			st.muted.Render("  kind is read off the value itself."),
		},
		hint:      tokenKindNote,
		busyLabel: "adding",
		submit:    func(token string) tea.Cmd { return addTokenCmd(p.switcher, index, token) },
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
	if msg.err != nil {
		return m.failed("Add failed", msg.err)
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
	switch {
	case msg.outcome.Activated:
		m.status += " — now the live login"
	case msg.outcome.ActivationFailed != "":
		m.status += " — stored, but not made live: " + msg.outcome.ActivationFailed
	}
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
	next, cmd := m.collectAll()
	return next, tea.Batch(cmd, clearStatusCmd())
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
