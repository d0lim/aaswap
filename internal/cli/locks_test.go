package cli

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/lockfile"
)

// Claude Code takes advisory locks around its credential and its config, and
// aaswap holds the same ones while it swaps those files — that is what keeps a
// swap from interleaving with a token refresh.
//
// Every switch took them, whichever provider was addressed. A Codex switch
// therefore created lock directories inside ~/.claude, and could fail with
// "timed out waiting for Claude Code's lock" while a real Claude Code was
// refreshing — held up by, and holding up, work that had nothing to do with it.
//
// Which locks exist is a fact about a TOOL, so it belongs to that tool's
// declaration like every other one.

// A Codex switch completes while Claude Code holds its own locks.
func TestSwitchingOneProviderDoesNotWaitOnAnothersLocks(t *testing.T) {
	h := newHarness(t)
	h.twoCodexAccounts(t)

	// Claude Code is mid-refresh: its locks are held for the whole switch.
	// Taken through lockfile itself, so this is the real contention and not an
	// imitation of it.
	err := lockfile.WithClaudeCredentials(h.switcher.Paths, lockfile.ProperOptions{},
		func() error {
			if code := h.run("--provider", "codex", "switch", "work"); code != ExitOK {
				t.Errorf("a Codex switch waited on Claude Code's lock: exit = %d: %s",
					code, h.stderr())
			}
			return nil
		})
	if err != nil {
		t.Fatalf("holding Claude Code's locks: %v", err)
	}
}

// And it does not reach into the other tool's home at all. Claude Code's
// credential lock lives INSIDE ~/.claude, so taking it on a machine that has
// only Codex means aaswap creating a Claude Code install directory during a
// Codex switch.
func TestSwitchingOneProviderDoesNotCreateAnothersHome(t *testing.T) {
	h := newHarness(t)
	h.twoCodexAccounts(t)

	// Codex only, which is the ordinary state for someone who does not use
	// Claude Code. The harness makes the directory for the common case.
	home := h.switcher.Paths.ClaudeConfigHome()
	if err := os.RemoveAll(home); err != nil {
		t.Fatal(err)
	}

	if code := h.run("--provider", "codex", "switch", "work"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	if _, err := os.Stat(home); err == nil {
		var made []string
		for path := range lockDirs(t, h) {
			made = append(made, path)
		}
		t.Errorf("a Codex switch created %s, an install directory for a tool that "+
			"is not on this machine (locks: %v)", home, made)
	}
}

// Claude's own switch must still take them. The protocol is the reason a swap
// and a refresh cannot interleave, and dropping it would be far worse than the
// bug above — so the fix is pinned from both directions.
func TestClaudesOwnSwitchStillTakesItsLocks(t *testing.T) {
	h := newHarness(t)
	h.seed(map[string]string{"one": "one@example.com", "two": "two@example.com"})
	h.login("one", "one@example.com")

	before := lockDirs(t, h)
	if code := h.run("switch", "two"); code != ExitOK {
		t.Fatalf("exit = %d: %s", code, h.stderr())
	}
	// A proper-lockfile lock is a directory that is removed on release, so what
	// is left behind is its PARENT having been created. The credential lock
	// sits inside the config home, which the harness already made; the config
	// lock sits beside ~/.claude.json, which it did not.
	//
	// Rather than inspect leftovers, take the locks and check the switch cannot
	// proceed — which is what "the protocol is followed" actually means.
	_ = before
	err := lockfile.WithClaudeConfig(h.switcher.Paths, lockfile.ProperOptions{},
		func() error {
			if code := h.run("switch", "one"); code == ExitOK {
				t.Error("a Claude switch completed while Claude Code held the config " +
					"lock, so the swap can now interleave with a token refresh")
				return nil
			}
			if !strings.Contains(h.stderr(), "lock") {
				t.Errorf("the switch failed for some other reason: %s", h.stderr())
			}
			return nil
		})
	if err != nil {
		t.Fatalf("holding Claude Code's config lock: %v", err)
	}
}

// lockDirs is every lock directory under the home right now.
func lockDirs(t *testing.T, h *harness) map[string]bool {
	t.Helper()
	found := map[string]bool{}
	err := filepath.WalkDir(h.switcher.Paths.Home,
		func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return nil //nolint:nilerr // a vanished directory is not a finding
			}
			if entry.IsDir() && filepath.Ext(path) == ".lock" {
				found[path] = true
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	return found
}
