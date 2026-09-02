package usagestore

import (
	"path/filepath"
	"testing"
)

// The usage table was one file for every provider, keyed by account name. Two
// providers each holding an account called "work" therefore shared a row.
//
// The identity guard saves the DATA — a row whose stored address differs is
// invisible to reads — but not the state: each provider's collect pass replaces
// the other's row. What is lost with it is everything the row exists to
// remember. The 429 backoff and the poll plan are in that row, so alternating
// between two providers threw away the backoff and let the next pass fetch
// immediately after a rate-limit response.

func TestEachProviderKeepsItsOwnTable(t *testing.T) {
	cache := t.TempDir()
	claude := NewForProvider(cache, "claude")
	codex := NewForProvider(cache, "codex")

	if claude.Path() == codex.Path() {
		t.Fatalf("both providers write %s", claude.Path())
	}
	if filepath.Dir(claude.Path()) != cache {
		t.Errorf("the table moved out of the cache directory: %s", claude.Path())
	}
}

// The table already on disk was written before providers existed, and it is
// Claude's. Unlike the mappings table this one is regenerable, so the reason to
// keep the name is smaller — but a rename costs every user a full round of
// re-fetching for nothing.
func TestClaudeKeepsTheExistingTable(t *testing.T) {
	cache := t.TempDir()
	if got := filepath.Base(NewForProvider(cache, "claude").Path()); got != "usage.json" {
		t.Errorf("claude's table is %q, want the existing usage.json", got)
	}
	if got := filepath.Base(NewForProvider(cache, "").Path()); got != "usage.json" {
		t.Errorf("an unscoped store reads %q, want the existing usage.json", got)
	}
}

// Two providers writing at once must not serialize on one lock, and must not
// take a lock that guards a file neither of them is writing.
func TestEachProvidersTableHasItsOwnLock(t *testing.T) {
	cache := t.TempDir()
	if NewForProvider(cache, "claude").LockPath() == NewForProvider(cache, "codex").LockPath() {
		t.Error("both providers contend on one lock for two different files")
	}
}
