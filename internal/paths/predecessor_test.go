package paths

import (
	"path/filepath"
	"testing"

	"github.com/d0lim/aaswap/internal/platform"
)

// Two predecessors now: the project this one was renamed from, and the Python
// project it was ported from. Both are foreign stores that only ever get read,
// and only when someone asks.
func TestPredecessorsAreSearchedNewestFirst(t *testing.T) {
	home := t.TempDir()
	r := New(home, platform.Linux)

	got := r.Predecessors()
	if len(got) != 2 {
		t.Fatalf("predecessors = %+v, want ccswap and claude-swap", got)
	}
	// ccswap first: it is the closer ancestor, so a machine holding both is
	// far likelier to want that one.
	if got[0].Name != "ccswap" || got[1].Name != "claude-swap" {
		t.Errorf("order = %q, %q, want ccswap before claude-swap", got[0].Name, got[1].Name)
	}
	for _, p := range got {
		if len(p.Roots) == 0 {
			t.Errorf("%s has nowhere to look", p.Name)
		}
	}
}

// A store is a directory with an account table in it. An empty directory left
// by an uninstall is not one, and offering to import nothing is worse than
// saying nothing.
func TestFindPredecessorNeedsATable(t *testing.T) {
	home := t.TempDir()
	r := New(home, platform.Linux)

	bare := filepath.Join(home, ".local", "share", "ccswap")
	mkdirAll(t, bare)
	if found, ok := r.FindPredecessor(); ok {
		t.Errorf("an empty directory reported as a store: %+v", found)
	}

	writeFile(t, filepath.Join(bare, RosterFileName), "{}")
	found, ok := r.FindPredecessor()
	if !ok {
		t.Fatal("a store with a table was not found")
	}
	if found.Name != "ccswap" || found.Root != bare {
		t.Errorf("found = %+v, want the ccswap store at %s", found, bare)
	}
	if found.KeychainService != "ccswap" {
		t.Errorf("keychain service = %q, want the predecessor's own", found.KeychainService)
	}
}

// A machine that ran both keeps them apart, and the closer ancestor wins.
func TestTheCloserPredecessorWins(t *testing.T) {
	home := t.TempDir()
	r := New(home, platform.Linux)
	for _, name := range []string{"ccswap", "claude-swap"} {
		dir := filepath.Join(home, ".local", "share", name)
		mkdirAll(t, dir)
		writeFile(t, filepath.Join(dir, RosterFileName), "{}")
	}

	found, ok := r.FindPredecessor()
	if !ok {
		t.Fatal("no store found")
	}
	if found.Name != "ccswap" {
		t.Errorf("found = %q, want the closer ancestor", found.Name)
	}
}

// This tool's own store is not a predecessor. Finding it would offer to import
// the store already in use.
func TestOurOwnStoreIsNotAPredecessor(t *testing.T) {
	home := t.TempDir()
	r := New(home, platform.Linux)
	own := r.BackupRoot()
	mkdirAll(t, own)
	writeFile(t, filepath.Join(own, RosterFileName), "{}")

	if found, ok := r.FindPredecessor(); ok {
		t.Errorf("our own store at %s reported as a predecessor: %+v", own, found)
	}
}
