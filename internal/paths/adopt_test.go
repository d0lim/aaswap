package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/d0lim/aaswap/internal/platform"
)

// seedStore writes a minimal store: a roster plus one file to prove the whole
// tree moves, not just the roster.
func seedStore(t *testing.T, root string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "credentials"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, RosterFileName),
		[]byte(`{"accounts":{"1":{"email":"a@example.com"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "credentials", ".creds-1-a@example.com.enc"),
		[]byte("dG9rZW4="), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The two projects must not share a store. Their roots have to differ on every
// platform, or the separation is only a comment.
func TestCcswapAndClaudeSwapRootsNeverCollide(t *testing.T) {
	for _, p := range []platform.Platform{platform.MacOS, platform.Linux, platform.WSL, platform.Windows} {
		t.Run(p.String(), func(t *testing.T) {
			r := New(t.TempDir(), p)
			ours := r.BackupRoot()
			for _, theirs := range r.Predecessors()[0].Roots {
				if samePath(ours, theirs) {
					t.Errorf("aaswap's root %q is also claude-swap's", ours)
				}
			}
		})
	}
}

// A store is a roster, not a directory. An empty leftover from an uninstall
// must not produce an offer to import nothing.
func TestOnlyAStoreWithARosterIsFound(t *testing.T) {
	home := t.TempDir()
	r := New(home, platform.MacOS)
	bare := r.Predecessors()[0].Roots[0]
	if err := os.MkdirAll(bare, 0o700); err != nil {
		t.Fatal(err)
	}

	if _, found := func() (string, bool) { f, ok := r.FindPredecessor(); return f.Root, ok }(); found {
		t.Error("an empty directory was offered as a store to import")
	}

	seedStore(t, bare)
	got, found := func() (string, bool) { f, ok := r.FindPredecessor(); return f.Root, ok }()
	if !found || got != bare {
		t.Errorf("FindClaudeSwapStore() = %q, %v; want %q, true", got, found, bare)
	}
}

// Adoption moves the whole tree. Anything left behind is a credential in two
// places, which is the state the move exists to avoid.
func TestAdoptionMovesTheWholeTree(t *testing.T) {
	home := t.TempDir()
	r := New(home, platform.MacOS)
	source := r.Predecessors()[0].Roots[0]
	seedStore(t, source)

	if err := r.AdoptStore(source); err != nil {
		t.Fatal(err)
	}

	target := r.BackupRoot()
	for _, name := range []string{RosterFileName, "credentials/.creds-1-a@example.com.enc"} {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(name))); err != nil {
			t.Errorf("%s did not come across: %v", name, err)
		}
	}
	if _, err := os.Stat(source); err == nil {
		t.Error("the claude-swap store still exists, so the same credential is now in two places")
	}
}

// Merging two account tables is a decision about which slot wins, and nothing
// here is entitled to make it.
func TestAdoptionRefusesToMergeIntoAPopulatedStore(t *testing.T) {
	home := t.TempDir()
	r := New(home, platform.MacOS)
	source := r.Predecessors()[0].Roots[0]
	seedStore(t, source)
	seedStore(t, r.BackupRoot())

	err := r.AdoptStore(source)
	if err == nil {
		t.Fatal("adoption overwrote an existing aaswap store")
	}
	if !strings.Contains(err.Error(), "already has accounts") {
		t.Errorf("error = %v, want it to name the collision", err)
	}
	if _, statErr := os.Stat(filepath.Join(source, RosterFileName)); statErr != nil {
		t.Error("a refused adoption still consumed the source store")
	}
}

// One `aaswap list` on a fresh machine leaves a cache and a log. That is not a
// store, and it must not block an import.
func TestAdoptionIgnoresThrowawayArtifacts(t *testing.T) {
	home := t.TempDir()
	r := New(home, platform.MacOS)
	source := r.Predecessors()[0].Roots[0]
	seedStore(t, source)

	target := r.BackupRoot()
	if err := os.MkdirAll(filepath.Join(target, "cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "aaswap.log"), []byte("noise"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := r.AdoptStore(source); err != nil {
		t.Fatalf("a cache and a log blocked the import: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, RosterFileName)); err != nil {
		t.Errorf("the roster did not arrive: %v", err)
	}
}
