package session

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/platform"
)

// ShareOptions selects what a profile mirrors from the default one.
//
// The two are independent concerns, so turning customizations off while keeping
// history on gives a bare profile with unified conversations — which is a
// combination people actually want.
type ShareOptions struct {
	// Customizations mirrors settings, skills, commands and agents.
	Customizations bool
	// History mirrors conversation history.
	History bool
}

// manifest records which entries in a profile aaswap created.
//
// Without it, turning sharing off could not tell aaswap's own links from files
// the user put there — and removing the wrong ones would destroy their work.
type manifest struct {
	Managed []string `json:"managed"`
}

// SyncSharing mirrors the shared items into a profile, or undoes it.
//
// Idempotent, and run on every launch. It sources from the DEFAULT profile
// rather than whatever CLAUDE_CONFIG_DIR currently points at: sharing always
// mirrors the default, even when the invoking shell is itself inside a session.
//
// Lock-free. Concurrent launches with different flags are last-writer-wins and
// self-heal on the next launch; nothing here can lose data, because the only
// thing it ever removes is a symlink it created.
func (m *Manager) SyncSharing(sessionDir, defaultProfile string, opts ShareOptions) error {
	if !isDirectory(sessionDir) {
		// Nothing to mirror into. The next bootstrap makes the directory, and
		// sharing runs again then.
		return nil
	}

	if m.Platform == platform.Windows {
		// Sharing on Windows would mean copies, which fork history rather than
		// sharing it. Dropped rather than half-done — and this also clears
		// links left behind by a profile that moved across platforms.
		opts.History = false
	}

	var active []string
	if opts.Customizations {
		active = append(active, SharedItems...)
	}
	if opts.History {
		active = append(active, HistoryItems...)
	}

	manifestPath := filepath.Join(sessionDir, ShareManifest)
	AdoptLegacyMarker(manifestPath)
	managed := readManifest(manifestPath)

	// A flag turned off since the last launch: remove the links created for it,
	// never plain files the user accumulated themselves.
	for _, name := range managed {
		if slices.Contains(active, name) {
			continue
		}
		dest := filepath.Join(sessionDir, name)
		if slices.Contains(HistoryItems, name) && isRegularOrDir(dest) {
			// A stale manifest — lock-free launches race — must never be able
			// to delete real conversation history. Only ever unlink a symlink.
			continue
		}
		removeManaged(dest)
	}

	if len(active) == 0 {
		if err := os.Remove(manifestPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("could not remove the share manifest", "path", manifestPath, "error", err)
		}
		return nil
	}

	var nowManaged []string
	for _, name := range active {
		if m.shareOne(sessionDir, defaultProfile, name, managed) {
			nowManaged = append(nowManaged, name)
		}
	}
	return writeManifest(manifestPath, nowManaged)
}

// shareOne mirrors a single item, reporting whether aaswap now manages it.
func (m *Manager) shareOne(sessionDir, defaultProfile, name string, managed []string) bool {
	source := filepath.Join(defaultProfile, name)
	dest := filepath.Join(sessionDir, name)

	if slices.Contains(HistoryItems, name) && !prepareHistoryShare(source, dest) {
		return false
	}

	target, err := resolveShareTarget(source)
	if err != nil {
		// The source vanished, or never existed. Prune aaswap's own entry rather
		// than leaving a link to nothing.
		if slices.Contains(managed, name) {
			removeManaged(dest)
		}
		return false
	}

	switch {
	case isSymlink(dest):
		// Only aaswap puts a symlink here, so an unmanaged one is adopted rather
		// than treated as user data.
		if existing, err := os.Readlink(dest); err == nil && existing == target {
			return true
		}
		removeManaged(dest)
	case exists(dest) && !slices.Contains(managed, name):
		// Pre-existing data in the profile: never touched.
		slog.Info("not sharing an item the session profile already has its own copy of",
			"item", name, "profile", sessionDir)
		return false
	case exists(dest):
		removeManaged(dest)
	}

	if err := os.Symlink(target, dest); err != nil {
		slog.Warn("could not share an item into a session profile",
			"item", name, "error", err)
		return false
	}
	return true
}

// resolveShareTarget is what a share link points at.
//
// The FULLY RESOLVED target, not the path as written. When the source is itself
// a symlink — a dotfiles setup — linking to the unresolved path makes a link to
// a link, and Claude Code's atomic settings write resolves only one hop: it
// renames its temporary file over the intermediate link, silently replacing the
// user's symlink with a regular file.
func resolveShareTarget(source string) (string, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink == 0 {
		return source, nil
	}
	return filepath.EvalSymlinks(source)
}

// prepareHistoryShare makes sure a history link can be created.
//
// Conversation history is the one shared item a user cannot regenerate, so a
// profile that already holds its own is left alone entirely.
func prepareHistoryShare(source, dest string) bool {
	if isRegularOrDir(dest) {
		slog.Info("not sharing history: the session profile has its own", "path", dest)
		return false
	}
	// The source has to exist for a link to be worth making; a profile that
	// links to a missing history would show Claude Code an empty one.
	if !exists(source) {
		return false
	}
	return true
}

// removeManaged removes an entry aaswap created.
func removeManaged(path string) {
	if err := os.RemoveAll(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("could not remove a shared entry", "path", path, "error", err)
	}
}

func readManifest(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var parsed manifest
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	return parsed.Managed
}

// writeManifest publishes the manifest atomically, so a concurrent reader never
// sees a truncated one.
func writeManifest(path string, managed []string) error {
	if managed == nil {
		managed = []string{}
	}
	return writeJSONPrivate(path, manifest{Managed: managed})
}

// MCPKey is the one user-scoped key mirrored out of the default profile's
// config. The file itself stays unshared — it holds the identity and the
// per-project state — but MCP servers are a machine-wide setup a session should
// inherit.
const MCPKey = "mcpServers"

// SyncMCPServers mirrors the default profile's MCP servers into a session's
// config, or removes a mirror it previously made.
//
// The removal is gated on an adoption marker, so a profile's OWN pre-existing
// definitions — written before this mirroring existed, or by hand — are never
// silently destroyed. The first mirror stashes whatever it displaces, once.
func (m *Manager) SyncMCPServers(sessionDir, defaultConfigPath string, share bool) error {
	configPath := filepath.Join(sessionDir, ".claude.json")
	config, ok := readJSONObject(configPath)
	if !ok {
		// A profile with no readable config has nothing to mirror into; the
		// next bootstrap writes one.
		return nil
	}

	markerPath := filepath.Join(sessionDir, MCPMirrorMarker)
	AdoptLegacyMarker(markerPath)
	adopted := exists(markerPath)

	if !share {
		if !adopted {
			// Not ours to remove.
			return nil
		}
		if _, present := config[MCPKey]; !present {
			return nil
		}
		delete(config, MCPKey)
		return writeJSONPrivate(configPath, config)
	}

	source, ok := readJSONObject(defaultConfigPath)
	if !ok {
		// Nothing readable to mirror FROM. Leaving the profile as it is beats
		// clearing its servers on the strength of a file that would not parse.
		return nil
	}
	servers, present := source[MCPKey]
	if !present || string(servers) == "null" {
		return nil
	}

	if existing, had := config[MCPKey]; had && !adopted && string(existing) != string(servers) {
		// The one-time migration: definitions this profile had before the
		// mirror existed land in a stash rather than vanishing. Write-once, so
		// a later re-mirror cannot overwrite the original displacement.
		stashPath := filepath.Join(sessionDir, MCPDisplacedStash)
		AdoptLegacyMarker(stashPath)
		if !exists(stashPath) {
			if err := writeJSONPrivate(stashPath, map[string]jsontext.Value{MCPKey: existing}); err != nil {
				return fmt.Errorf("%w: stashing the profile's own MCP servers before "+
					"mirroring: %w", apperr.ErrSession, err)
			}
			slog.Info("stashed a session profile's own MCP servers before mirroring",
				"profile", sessionDir, "stash", stashPath)
		}
	}

	if existing, had := config[MCPKey]; had && string(existing) == string(servers) && adopted {
		return nil
	}
	config[MCPKey] = servers
	if err := writeJSONPrivate(configPath, config); err != nil {
		return err
	}
	if !adopted {
		if err := os.WriteFile(markerPath, nil, 0o600); err != nil {
			slog.Warn("could not write the MCP adoption marker", "path", markerPath, "error", err)
		}
	}
	return nil
}

// readJSONObject reads a JSON object, reporting false for anything unusable.
//
// A bool rather than an error: every caller here treats "cannot read it" and
// "it is not an object" identically, and neither is worth surfacing — the file
// belongs to Claude Code, and mirroring is a convenience.
func readJSONObject(path string) (map[string]jsontext.Value, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var out map[string]jsontext.Value
	if err := json.Unmarshal(data, &out); err != nil || out == nil {
		slog.Debug("a config file does not hold a JSON object", "path", path)
		return nil, false
	}
	return out, true
}

func isDirectory(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func isSymlink(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink != 0
}

// isRegularOrDir reports whether a path exists and is NOT a symlink — that is,
// whether it holds real data rather than a link aaswap made.
func isRegularOrDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode()&os.ModeSymlink == 0
}
