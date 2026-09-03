package tui

import (
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/provider"
)

// The dashboard's `t` key opens a field whose placeholder reads "sk-ant-oat01-…" and stores
// whatever is typed as a Claude credential. Offered for every provider, it is
// the same defect as the CLI's `login --token`, with one difference that makes
// it worse: the CLI at least names the flag, while here the key is simply on
// the help screen, so there is nothing to warn the person that the tool they
// are looking at is not the one the prompt describes.

// A provider that declares no token format has no such key.
func TestTheTokenKeyIsAbsentWhereATokenCannotBeStored(t *testing.T) {
	m := twoAccounts(t)
	m.panes[0].spec = provider.MustLookup(provider.Codex)

	next, cmd := m.handleKey(press("t"))
	model := next.(Model)
	if model.modal != nil {
		t.Fatalf("the field opened for a provider whose token format is unknown: %+v",
			model.modal)
	}
	if cmd != nil {
		t.Error("pressing t started work")
	}
	// Silently ignoring the key would be its own failure: the person pressed
	// it because the help screen offered it.
	if model.status == "" {
		t.Error("nothing was said about why the key did nothing")
	}
	if !strings.Contains(model.renderHelp(), "collect now") {
		t.Fatal("the help screen no longer lists the keys that do work")
	}
	if strings.Contains(model.renderHelp(), "setup token") {
		t.Errorf("the help screen still offers the token key:\n%s", model.renderHelp())
	}
}

// Claude declares one, so the field still opens — and its placeholder comes
// from the declaration rather than being written into the dashboard.
func TestTheTokenFieldDescribesTheProvidersOwnFormat(t *testing.T) {
	m := twoAccounts(t)
	m.panes[0].spec = provider.MustLookup(provider.Claude)

	next, _ := m.handleKey(press("t"))
	model := next.(Model)
	if model.modal == nil {
		t.Fatal("the field did not open for the provider that declares a token format")
	}
	want := provider.MustLookup(provider.Claude).Token.Hint()
	if model.modal.placeholder != want {
		t.Errorf("placeholder = %q, want the declaration's %q",
			model.modal.placeholder, want)
	}
	if !strings.Contains(model.renderHelp(), "token") {
		t.Errorf("the help screen omits the token key:\n%s", model.renderHelp())
	}
}

// The dashboard's prose named Claude Code whichever provider it was showing.
// With every tool on one screen the prose names none, and each section names
// its own.
func TestTheScreenNamesTheProviderItIsShowing(t *testing.T) {
	m := twoAccounts(t)
	m.panes[0].spec = provider.MustLookup(provider.Codex)

	if help := m.renderHelp(); strings.Contains(help, "Claude Code") {
		t.Errorf("a Codex-only dashboard's help talks about Claude Code:\n%s", help)
	}
	frame := m.View().Content
	if strings.Contains(frame, "Claude Code") || !strings.Contains(frame, "Codex") {
		t.Errorf("the section does not name the tool it is showing:\n%s", frame)
	}
}
