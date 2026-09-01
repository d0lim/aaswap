package session

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/d0lim/aaswap/internal/testutil"
)

// defaultProfile builds the ~/.claude a session mirrors from.
func (f *fixture) defaultProfile() string {
	f.t.Helper()
	dir := filepath.Join(f.home, ".claude")
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o700); err != nil {
		f.t.Fatal(err)
	}
	for _, name := range []string{"settings.json", "CLAUDE.md", "history.jsonl"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("from the default profile"), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	return dir
}

// emptyProfile makes an empty session profile directory.
func (f *fixture) emptyProfile() string {
	f.t.Helper()
	dir := f.Dir("1", "a@example.com")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

func TestSharingLinksCustomizations(t *testing.T) {
	f := newFixture(t)
	source := f.defaultProfile()
	profile := f.emptyProfile()

	if err := f.SyncSharing(profile, source, ShareOptions{Customizations: true}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"settings.json", "CLAUDE.md", "skills"} {
		link := filepath.Join(profile, name)
		if !isSymlink(link) {
			t.Errorf("%s is not a link into the default profile", name)
			continue
		}
		target, err := os.Readlink(link)
		if err != nil {
			t.Fatal(err)
		}
		if target != filepath.Join(source, name) {
			t.Errorf("%s points at %q", name, target)
		}
	}
	// History is a separate concern and was not asked for.
	for _, name := range HistoryItems {
		if exists(filepath.Join(profile, name)) {
			t.Errorf("%s was shared without being asked for", name)
		}
	}
}

// The two flags are independent, so a bare profile with unified history is a
// combination that works.
func TestHistorySharingIsIndependent(t *testing.T) {
	f := newFixture(t)
	source := f.defaultProfile()
	profile := f.emptyProfile()

	if err := f.SyncSharing(profile, source, ShareOptions{History: true}); err != nil {
		t.Fatal(err)
	}
	for _, name := range HistoryItems {
		if !isSymlink(filepath.Join(profile, name)) {
			t.Errorf("%s was not shared", name)
		}
	}
	for _, name := range SharedItems {
		if exists(filepath.Join(profile, name)) {
			t.Errorf("%s was shared without being asked for", name)
		}
	}
}

// Sharing runs on every launch and must converge, not accumulate.
func TestSharingIsIdempotent(t *testing.T) {
	f := newFixture(t)
	source := f.defaultProfile()
	profile := f.emptyProfile()
	opts := ShareOptions{Customizations: true, History: true}

	for range 3 {
		if err := f.SyncSharing(profile, source, opts); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(profile)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	// Only the items the source actually has, plus the manifest — repeated
	// runs converge on exactly this set rather than accumulating.
	want := []string{
		ShareManifest, "CLAUDE.md", "history.jsonl", "projects", "settings.json", "skills",
	}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Errorf("the profile holds %v, want %v", names, want)
	}
}

// Turning a flag off removes the links aaswap made for it — and only those.
func TestTurningSharingOffRemovesOnlyManagedLinks(t *testing.T) {
	f := newFixture(t)
	source := f.defaultProfile()
	profile := f.emptyProfile()

	if err := f.SyncSharing(profile, source, ShareOptions{Customizations: true}); err != nil {
		t.Fatal(err)
	}
	// Something the user put there themselves.
	ownFile := filepath.Join(profile, "keybindings.json")
	if err := os.WriteFile(ownFile, []byte("mine"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := f.SyncSharing(profile, source, ShareOptions{}); err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"settings.json", "CLAUDE.md", "skills"} {
		if exists(filepath.Join(profile, name)) {
			t.Errorf("%s survived sharing being turned off", name)
		}
	}
	data, err := os.ReadFile(ownFile)
	if err != nil || string(data) != "mine" {
		t.Errorf("the user's own file was destroyed: %v", err)
	}
	// With nothing shared, the manifest goes too.
	if exists(filepath.Join(profile, ShareManifest)) {
		t.Error("an empty manifest was left behind")
	}
}

// Data the user already has in the profile is never touched.
func TestPreExistingProfileDataIsNotReplaced(t *testing.T) {
	f := newFixture(t)
	source := f.defaultProfile()
	profile := f.emptyProfile()
	own := filepath.Join(profile, "settings.json")
	if err := os.WriteFile(own, []byte("the profile's own settings"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := f.SyncSharing(profile, source, ShareOptions{Customizations: true}); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(own)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "the profile's own settings" {
		t.Errorf("the profile's own file was replaced with %q", data)
	}
	if isSymlink(own) {
		t.Error("the profile's own file was replaced by a link")
	}
	// And it is not claimed in the manifest, so turning sharing off cannot
	// remove it later.
	if slices.Contains(readManifest(filepath.Join(profile, ShareManifest)), "settings.json") {
		t.Error("a file aaswap did not create was claimed in the manifest")
	}
}

// A stale manifest — lock-free launches race — must never be able to delete
// real conversation history.
func TestAStaleManifestCannotDeleteRealHistory(t *testing.T) {
	f := newFixture(t)
	source := f.defaultProfile()
	profile := f.emptyProfile()

	// A manifest claiming history that is, in fact, the profile's own data.
	if err := writeManifest(filepath.Join(profile, ShareManifest), []string{"projects"}); err != nil {
		t.Fatal(err)
	}
	realHistory := filepath.Join(profile, "projects")
	if err := os.MkdirAll(realHistory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realHistory, "session.jsonl"), []byte("conversations"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := f.SyncSharing(profile, source, ShareOptions{Customizations: true}); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(realHistory, "session.jsonl")); err != nil {
		t.Errorf("real conversation history was deleted on the strength of a stale manifest: %v", err)
	}
}

// Linking to the unresolved path makes a link to a link, and Claude Code's
// atomic settings write resolves only one hop — replacing the user's own
// symlink with a regular file.
func TestSharingLinksThroughASymlinkedSource(t *testing.T) {
	f := newFixture(t)
	source := f.defaultProfile()
	profile := f.emptyProfile()

	// A dotfiles setup: ~/.claude/settings.json is itself a link.
	real := filepath.Join(f.home, "dotfiles", "claude-settings.json")
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte("from dotfiles"), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceLink := filepath.Join(source, "settings.json")
	if err := os.Remove(sourceLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, sourceLink); err != nil {
		t.Skipf("symlinks are unavailable here: %v", err)
	}

	if err := f.SyncSharing(profile, source, ShareOptions{Customizations: true}); err != nil {
		t.Fatal(err)
	}

	target, err := os.Readlink(filepath.Join(profile, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if target != resolved {
		t.Errorf("the share points at %q, want the fully resolved %q — a link to a link "+
			"gets replaced by Claude Code's own atomic write", target, resolved)
	}
}

// A source that never existed leaves no dangling link behind.
func TestAMissingSourceIsSkipped(t *testing.T) {
	f := newFixture(t)
	source := filepath.Join(f.home, ".claude")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	profile := f.emptyProfile()

	if err := f.SyncSharing(profile, source, ShareOptions{Customizations: true}); err != nil {
		t.Fatal(err)
	}
	for _, name := range SharedItems {
		if exists(filepath.Join(profile, name)) {
			t.Errorf("%s was linked despite having no source", name)
		}
	}
}

// A source that vanishes between launches has its link pruned rather than left
// dangling.
func TestAVanishedSourcePrunesItsLink(t *testing.T) {
	f := newFixture(t)
	source := f.defaultProfile()
	profile := f.emptyProfile()

	if err := f.SyncSharing(profile, source, ShareOptions{Customizations: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(source, "CLAUDE.md")); err != nil {
		t.Fatal(err)
	}
	if err := f.SyncSharing(profile, source, ShareOptions{Customizations: true}); err != nil {
		t.Fatal(err)
	}

	if exists(filepath.Join(profile, "CLAUDE.md")) {
		t.Error("a link to a vanished source was left in place")
	}
	if slices.Contains(readManifest(filepath.Join(profile, ShareManifest)), "CLAUDE.md") {
		t.Error("the manifest still claims a link that is gone")
	}
}

func TestMCPMirroring(t *testing.T) {
	f := newFixture(t)
	profile := f.emptyProfile()
	defaultConfig := filepath.Join(f.home, ".claude.json")
	if err := os.WriteFile(defaultConfig,
		[]byte(`{"mcpServers":{"local":{"command":"srv"}},"projects":{"/w":{}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	profileConfig := filepath.Join(profile, ".claude.json")
	if err := os.WriteFile(profileConfig,
		[]byte(`{"oauthAccount":{"emailAddress":"a@example.com"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := f.SyncMCPServers(profile, defaultConfig, true); err != nil {
		t.Fatal(err)
	}

	config := readConfig(t, profileConfig)
	servers := config["mcpServers"].(map[string]any)
	if _, present := servers["local"]; !present {
		t.Errorf("mcpServers = %v, want the mirrored one", servers)
	}
	// Only that key: the config holds the identity and the per-project state,
	// which are the profile's own.
	if _, present := config["projects"]; present {
		t.Error("the default profile's projects were mirrored too")
	}
	if config["oauthAccount"] == nil {
		t.Error("the profile's identity was lost")
	}
	if !exists(filepath.Join(profile, MCPMirrorMarker)) {
		t.Error("no adoption marker was written")
	}
}

// The removal is gated on the marker, so definitions written before the
// mirroring existed are never silently destroyed.
func TestUnsharedMCPIsRemovedOnlyWhenAdopted(t *testing.T) {
	t.Run("a profile's own definitions survive", func(t *testing.T) {
		f := newFixture(t)
		profile := f.emptyProfile()
		profileConfig := filepath.Join(profile, ".claude.json")
		if err := os.WriteFile(profileConfig,
			[]byte(`{"mcpServers":{"mine":{"command":"own"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := f.SyncMCPServers(profile, filepath.Join(f.home, ".claude.json"), false); err != nil {
			t.Fatal(err)
		}
		config := readConfig(t, profileConfig)
		if config["mcpServers"] == nil {
			t.Error("a profile's own MCP definitions were removed")
		}
	})

	t.Run("a mirror is removed", func(t *testing.T) {
		f := newFixture(t)
		profile := f.emptyProfile()
		profileConfig := filepath.Join(profile, ".claude.json")
		if err := os.WriteFile(profileConfig,
			[]byte(`{"mcpServers":{"local":{"command":"srv"}}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(profile, MCPMirrorMarker), nil, 0o600); err != nil {
			t.Fatal(err)
		}

		if err := f.SyncMCPServers(profile, filepath.Join(f.home, ".claude.json"), false); err != nil {
			t.Fatal(err)
		}
		config := readConfig(t, profileConfig)
		if _, present := config["mcpServers"]; present {
			t.Error("a mirror survived being turned off")
		}
	})
}

// The first mirror displaces whatever was there; those definitions land in a
// stash rather than vanishing, and only once.
func TestTheFirstMirrorStashesWhatItDisplaces(t *testing.T) {
	f := newFixture(t)
	profile := f.emptyProfile()
	defaultConfig := filepath.Join(f.home, ".claude.json")
	if err := os.WriteFile(defaultConfig,
		[]byte(`{"mcpServers":{"shared":{"command":"srv"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	profileConfig := filepath.Join(profile, ".claude.json")
	if err := os.WriteFile(profileConfig,
		[]byte(`{"mcpServers":{"mine":{"command":"own"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := f.SyncMCPServers(profile, defaultConfig, true); err != nil {
		t.Fatal(err)
	}

	stash := readConfig(t, filepath.Join(profile, MCPDisplacedStash))
	servers := stash["mcpServers"].(map[string]any)
	if _, present := servers["mine"]; !present {
		t.Errorf("the displaced definitions are not in the stash: %v", stash)
	}

	// A later mirror must not overwrite the original displacement.
	if err := os.WriteFile(defaultConfig,
		[]byte(`{"mcpServers":{"other":{"command":"srv2"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.SyncMCPServers(profile, defaultConfig, true); err != nil {
		t.Fatal(err)
	}
	again := readConfig(t, filepath.Join(profile, MCPDisplacedStash))
	if _, present := again["mcpServers"].(map[string]any)["mine"]; !present {
		t.Errorf("the stash was overwritten by a later mirror: %v", again)
	}
}

// A default profile with no MCP servers has nothing to mirror; that must not
// clear a profile's own.
func TestNoSourceServersMeansNoMirror(t *testing.T) {
	f := newFixture(t)
	profile := f.emptyProfile()
	defaultConfig := filepath.Join(f.home, ".claude.json")
	if err := os.WriteFile(defaultConfig, []byte(`{"projects":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	profileConfig := filepath.Join(profile, ".claude.json")
	if err := os.WriteFile(profileConfig,
		[]byte(`{"mcpServers":{"mine":{"command":"own"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := f.SyncMCPServers(profile, defaultConfig, true); err != nil {
		t.Fatal(err)
	}
	config := readConfig(t, profileConfig)
	if config["mcpServers"] == nil {
		t.Error("a profile's own definitions were cleared by an empty source")
	}
}

// The profile's config is owner-only: it names the account.
func TestTheSharedManifestIsOwnerOnly(t *testing.T) {
	f := newFixture(t)
	source := f.defaultProfile()
	profile := f.emptyProfile()
	if err := f.SyncSharing(profile, source, ShareOptions{Customizations: true}); err != nil {
		t.Fatal(err)
	}
	testutil.AssertPerm(t, filepath.Join(profile, ShareManifest), 0o600)
}
