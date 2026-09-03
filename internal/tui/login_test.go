package tui

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/d0lim/aaswap/internal/paths"
	"github.com/d0lim/aaswap/internal/platform"
	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/swap"
)

// stubOnPath puts an executable named binary on PATH, so LookPath resolves it
// without the real tool. Nothing here runs it.
func stubOnPath(t *testing.T, binary string) {
	t.Helper()
	dir := t.TempDir()
	stub := filepath.Join(dir, binary)
	body := "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		stub += ".cmd"
		body = "@exit /b 0\r\n"
	}
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// loginFixture is a dashboard over a real, empty, file-backed store — the one
// place these tests need a switcher, because a login sandbox is opened by one.
// The tool is never run: execTool records what it would have run.
func loginFixture(t *testing.T) (Model, *exec.Cmd, *func(error) tea.Msg) {
	t.Helper()
	stubOnPath(t, "claude")
	s := swap.NewForProvider(paths.New(t.TempDir(), platform.Linux), provider.Claude)
	m := twoAccounts(t)
	m.panes[0].switcher = s
	var recorded *exec.Cmd
	var done func(error) tea.Msg
	m.execTool = func(c *exec.Cmd, fn func(error) tea.Msg) tea.Cmd {
		recorded, done = c, fn
		return nil
	}
	return m, recorded, &done
}

// n hands the terminal to the tool's own login, pointed at a sandbox, and files
// what lands when it exits. Nothing on screen asks the person to go anywhere.
func TestNRunsTheToolsLoginIntoASandbox(t *testing.T) {
	m, _, done := loginFixture(t)
	var recorded *exec.Cmd
	m.execTool = func(c *exec.Cmd, fn func(error) tea.Msg) tea.Cmd {
		recorded, *done = c, fn
		return nil
	}

	next, cmd := m.handleKey(press("n"))
	m = next.(Model)
	if m.busy != "opening a login" || cmd == nil {
		t.Fatalf("busy = %q with cmd %v, want a login being opened", m.busy, cmd)
	}
	begun, ok := cmd().(loginBegunMsg)
	if !ok || begun.err != nil {
		t.Fatalf("opening the sandbox: %+v", begun)
	}
	defer begun.sandbox.Discard()

	next, _ = m.Update(begun)
	m = next.(Model)
	if m.busy != "logging in" {
		t.Errorf("busy = %q, want the tool to be running", m.busy)
	}
	if recorded == nil {
		t.Fatal("the tool was not handed the terminal")
	}
	if got := strings.Join(recorded.Args[1:], " "); got != "auth login --claudeai" {
		t.Errorf("ran %q, want Claude Code's login subcommand", got)
	}
	home := "CLAUDE_CONFIG_DIR=" + begun.sandbox.Home
	found := false
	for _, entry := range recorded.Env {
		found = found || entry == home
	}
	if !found {
		t.Errorf("the tool was not pointed at the sandbox %q", begun.sandbox.Home)
	}

	next, cmd = m.Update((*done)(nil))
	m = next.(Model)
	if m.busy != "storing the login" || cmd == nil {
		t.Errorf("busy = %q with cmd %v, want the login being stored", m.busy, cmd)
	}
}

// A tool that did not complete is reported, and its sandbox — a directory
// that may hold a half-finished credential — is removed.
func TestAFailedToolLoginIsReportedAndTheSandboxRemoved(t *testing.T) {
	m, _, _ := loginFixture(t)
	sandbox, err := m.panes[0].switcher.BeginLogin()
	if err != nil {
		t.Fatal(err)
	}
	next, _ := m.Update(loginRanMsg{sandbox: sandbox, err: errBoom})
	m = next.(Model)
	if m.modal == nil || m.modal.kind != modalNotice || !m.modal.danger {
		t.Fatalf("modal = %+v, want a failure notice", m.modal)
	}
	if _, err := os.Stat(sandbox.Home); !os.IsNotExist(err) {
		t.Errorf("the sandbox %q survived the failure", sandbox.Home)
	}
}

// A login the machine had none of becomes the live one, and the status says
// so: otherwise the person is left wondering why the switch key is needed.
func TestAnActivatedLoginIsSaidToBeLive(t *testing.T) {
	m := twoAccounts(t)
	next, _ := m.handleAdded(addedMsg{outcome: swap.AddOutcome{
		Name: "3", Email: "new@example.com", Activated: true}})
	if status := next.(Model).status; !strings.Contains(status, "now the live login") {
		t.Errorf("status = %q, want it to say the account went live", status)
	}
}

// The key is on the footer and in the help, described as what it now is.
func TestTheLoginKeyIsOfferedAsALogin(t *testing.T) {
	m := twoAccounts(t)
	if !strings.Contains(m.hintBar(), "log in") {
		t.Errorf("the footer does not offer the login key:\n%s", m.hintBar())
	}
	help := m.renderHelp()
	for _, want := range []string{"log in to add another account", "sandbox", "not touched"} {
		if !strings.Contains(help, want) {
			t.Errorf("the help does not say %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "cannot log you in") {
		t.Error("the help still says aaswap cannot log anyone in")
	}
}
