package mappings

import (
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/testutil"
)

var testNow = time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

func newStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	s := NewForProvider(root, "claude")
	s.Now = func() time.Time { return testNow }
	return s, root
}

func TestSetGetRemove(t *testing.T) {
	s, root := newStore(t)
	dir := filepath.Join(root, "project")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	identity := Identity{Email: "a@example.com", OrganizationUUID: "org-1"}

	if _, err := s.Set(dir, identity); err != nil {
		t.Fatal(err)
	}
	entry, found, err := s.Get(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !found || entry.Identity() != identity {
		t.Errorf("Get = (%+v, %v), want the identity just set", entry, found)
	}
	if entry.Added != "2026-06-15T12:00:00Z" {
		t.Errorf("added = %q", entry.Added)
	}

	removed, err := s.Remove(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !removed {
		t.Error("Remove reported nothing to remove")
	}
	if _, found, _ := s.Get(dir); found {
		t.Error("the mapping survived its removal")
	}
	// Removing it again is not an error, just nothing to do.
	if removed, err := s.Remove(dir); err != nil || removed {
		t.Errorf("a second Remove = (%v, %v)", removed, err)
	}
}

// The most specific match wins, so a nested folder inherits the closest
// mapping rather than the outermost one.
func TestResolvePrefersTheNearestAncestor(t *testing.T) {
	s, root := newStore(t)
	repo := filepath.Join(root, "repo")
	pkg := filepath.Join(repo, "packages", "api")
	deep := filepath.Join(pkg, "src", "handlers")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}

	outer := Identity{Email: "outer@example.com"}
	inner := Identity{Email: "inner@example.com"}
	if _, err := s.Set(repo, outer); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Set(pkg, inner); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		dir  string
		want Identity
	}{
		{"the mapped directory itself", pkg, inner},
		{"a directory below the inner mapping", deep, inner},
		{"a directory below only the outer one", filepath.Join(repo, "docs"), outer},
		{"the outer mapping itself", repo, outer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, entry, found := s.Resolve(tt.dir)
			if !found || entry.Identity() != tt.want {
				t.Errorf("Resolve(%s) = (%+v, %v), want %+v", tt.dir, entry, found, tt.want)
			}
		})
	}

	t.Run("an unmapped directory resolves to nothing", func(t *testing.T) {
		if _, _, found := s.Resolve(root); found {
			t.Error("an unmapped directory resolved")
		}
	})
}

// A sibling whose name merely starts with a mapped one is NOT inside it.
func TestResolveComparesPathComponents(t *testing.T) {
	s, root := newStore(t)
	mapped := filepath.Join(root, "app")
	sibling := filepath.Join(root, "application")
	for _, dir := range []string{mapped, sibling} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Set(mapped, Identity{Email: "a@example.com"}); err != nil {
		t.Fatal(err)
	}

	if _, _, found := s.Resolve(sibling); found {
		t.Errorf("%q resolved through a mapping on %q", sibling, mapped)
	}
}

// The same directory must produce the same key however it was typed.
func TestPathNormalization(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.MkdirAll(filepath.Join(real, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}

	viaReal, err := NormalizePath(real)
	if err != nil {
		t.Fatal(err)
	}
	viaLink, err := NormalizePath(link)
	if err != nil {
		t.Fatal(err)
	}
	if viaReal != viaLink {
		t.Errorf("a symlink produced a different key: %q vs %q", viaLink, viaReal)
	}

	// A relative path and a path with redundant separators land on the same key.
	messy, err := NormalizePath(filepath.Join(real, "nested", "..", ".", ""))
	if err != nil {
		t.Fatal(err)
	}
	if messy != viaReal {
		t.Errorf("a messy path produced %q, want %q", messy, viaReal)
	}
}

// A directory that does not exist yet still maps: refusing would be a surprise
// with no upside.
func TestAnUncreatedDirectoryStillNormalizes(t *testing.T) {
	root := t.TempDir()
	future := filepath.Join(root, "not-yet")
	key, err := NormalizePath(future)
	if err != nil {
		t.Fatalf("normalizing a path that does not exist: %v", err)
	}
	if !filepath.IsAbs(key) {
		t.Errorf("key = %q, want an absolute path", key)
	}
}

// A mapping to an account that no longer exists would silently send `aaswap run`
// looking for it.
func TestPruneAccount(t *testing.T) {
	s, root := newStore(t)
	doomed := Identity{Email: "gone@example.com", OrganizationUUID: "org-1"}
	// The same address under a different organization is a different account.
	sibling := Identity{Email: "gone@example.com", OrganizationUUID: "org-2"}
	keeper := Identity{Email: "kept@example.com"}

	for dir, identity := range map[string]Identity{
		"a": doomed, "b": doomed, "c": sibling, "d": keeper,
	} {
		path := filepath.Join(root, dir)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Set(path, identity); err != nil {
			t.Fatal(err)
		}
	}

	removed, err := s.PruneAccount(doomed)
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("PruneAccount removed %d, want 2", removed)
	}
	if got := len(s.Load()); got != 2 {
		t.Errorf("%d mappings remain, want 2", got)
	}
	// The same address under another organization survives.
	if _, _, found := s.Resolve(filepath.Join(root, "c")); !found {
		t.Error("a sibling account's mapping was pruned")
	}

	// Pruning again removes nothing and does not fail.
	if removed, err := s.PruneAccount(doomed); err != nil || removed != 0 {
		t.Errorf("a second prune = (%d, %v)", removed, err)
	}
}

// The table holds conveniences, not credentials: rebuilding it costs a few
// commands, and refusing to start over it would cost far more.
func TestAnUnusableTableReadsAsEmpty(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"malformed JSON", `{"mappings":`},
		{"not an object", `[1,2,3]`},
		{"no mappings key", `{"schemaVersion":1}`},
		{"empty", ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newStore(t)
			if err := os.WriteFile(s.Path(), []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if got := s.Load(); len(got) != 0 {
				t.Errorf("Load = %v, want empty", got)
			}
		})
	}
}

// A key this version does not know must survive a rewrite: two implementations
// share the file during the migration.
func TestUnknownEntryFieldsSurviveARewrite(t *testing.T) {
	s, root := newStore(t)
	other := filepath.Join(root, "other")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatal(err)
	}

	key, err := NormalizePath(filepath.Join(root, "kept"))
	if err != nil {
		t.Fatal(err)
	}
	// The key is a real path, and on Windows that means backslashes — which a
	// raw string splices in as invalid JSON escapes. Marshal it instead of
	// concatenating, or the fixture this test rests on never parses.
	quotedKey, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	original := `{"schemaVersion":1,"mappings":{` + string(quotedKey) +
		`:{"email":"a@example.com","organizationUuid":"","futureField":{"x":1}}}}`
	if err := os.WriteFile(s.Path(), []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := s.Set(other, Identity{Email: "b@example.com"}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Mappings map[string]map[string]any `json:"mappings"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw.Mappings[key]["futureField"]; !present {
		t.Errorf("an unknown field was dropped: %v", raw.Mappings[key])
	}
}

func TestTheTableIsOwnerOnly(t *testing.T) {
	s, root := newStore(t)
	if _, err := s.Set(root, Identity{Email: "a@example.com"}); err != nil {
		t.Fatal(err)
	}
	testutil.AssertPerm(t, s.Path(), 0o600)
}

// A directory belongs to one account per TOOL. One table for all of them let a
// second provider's `dir map` overwrite the first's, gave every provider the
// same answer for a directory, and let removing an account prune another
// provider's mapping for the same address.
func TestEachProviderKeepsItsOwnTable(t *testing.T) {
	root := t.TempDir()
	dir := t.TempDir()

	claude := NewForProvider(root, "claude")
	claude.Now = func() time.Time { return testNow }
	codex := NewForProvider(root, "codex")
	codex.Now = func() time.Time { return testNow }

	// The same address at both, which is the ordinary case and what made the
	// crossing invisible: nothing in the entry itself would look wrong.
	shared := Identity{Email: "me@example.com", OrganizationUUID: "org-1"}
	if _, err := claude.Set(dir, shared); err != nil {
		t.Fatal(err)
	}
	if _, err := codex.Set(dir, Identity{Email: "me@example.com", OrganizationUUID: "acct-9"}); err != nil {
		t.Fatal(err)
	}

	entry, found, err := claude.Get(dir)
	if err != nil || !found {
		t.Fatalf("the Claude mapping is gone: found = %v, err = %v", found, err)
	}
	if entry.Identity() != shared {
		t.Errorf("the Claude mapping reads %+v, want %+v — the tables are crossed",
			entry.Identity(), shared)
	}

	// And a prune stops at its own table.
	if _, err := codex.PruneAccount(shared); err != nil {
		t.Fatal(err)
	}
	if _, found, _ := claude.Get(dir); !found {
		t.Error("pruning a Codex account removed the Claude mapping")
	}
}

// The file already on disk was written before providers existed, and it is
// Claude's. Renaming it would lose every mapping a user had.
func TestClaudeKeepsTheUnsuffixedTable(t *testing.T) {
	if got := FileNameFor("claude"); got != FileName {
		t.Errorf("claude's table is %q, want the existing %q", got, FileName)
	}
	if got := FileNameFor(""); got != FileName {
		t.Errorf("an unscoped store reads %q, want the existing %q", got, FileName)
	}
	if got := FileNameFor("codex"); got == FileName {
		t.Errorf("codex shares claude's table at %q", got)
	}
}
