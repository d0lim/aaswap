// Package mappings remembers which account a directory belongs to.
//
// `ccswap run` with no account resolves the working directory to its nearest
// mapped ancestor and launches that account, so a project directory always gets
// the same login without anyone naming it.
//
// Identity is stored as the stable (email, organization) composite, NOT the
// slot number: slot numbers are reused when accounts are removed and re-added,
// and a mapping that named one would silently start pointing at a different
// account.
//
// Deliberately decoupled from the switcher — it never imports it. A caller
// resolves an entry's identity to a live slot itself.
package mappings

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/d0lim/ccswap/internal/apperr"
	"github.com/d0lim/ccswap/internal/fsutil"
)

// SchemaVersion is the file's format.
const SchemaVersion = 1

// FileName is the mappings table inside the backup root.
const FileName = "mappings.json"

// Entry is one directory's account.
type Entry struct {
	Email            string `json:"email"`
	OrganizationUUID string `json:"organizationUuid"`
	Added            string `json:"added,omitzero"`

	Extra map[string]jsontext.Value `json:",embed"`
}

// Identity is the composite a mapping points at.
type Identity struct {
	Email            string
	OrganizationUUID string
}

// Identity narrows an entry to what identifies the account.
func (e *Entry) Identity() Identity {
	if e == nil {
		return Identity{}
	}
	return Identity{Email: e.Email, OrganizationUUID: e.OrganizationUUID}
}

type table struct {
	SchemaVersion int               `json:"schemaVersion"`
	Mappings      map[string]*Entry `json:"mappings"`

	Extra map[string]jsontext.Value `json:",embed"`
}

// Store reads and writes the mappings table.
type Store struct {
	path string
	// Now is the clock, injected so a test can pin the stamps.
	Now func() time.Time
}

// New returns a store over a backup root.
func New(backupRoot string) *Store {
	return &Store{path: filepath.Join(backupRoot, FileName), Now: time.Now}
}

// Path is the table's location.
func (s *Store) Path() string { return s.path }

func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// NormalizePath turns a path into a stable, comparable key.
//
// It expands a leading ~, makes the path absolute, and resolves symlinks, so
// the same directory produces the same key however it was typed — and so a
// mapping survives being reached through a different symlink than the one it
// was created under.
func NormalizePath(path string) (string, error) {
	expanded := path
	if rest, ok := strings.CutPrefix(path, "~"); ok && (rest == "" || rest[0] == '/' || rest[0] == os.PathSeparator) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding ~ in %q: %w", path, err)
		}
		expanded = filepath.Join(home, rest)
	}

	absolute, err := filepath.Abs(expanded)
	if err != nil {
		return "", fmt.Errorf("resolving %q: %w", path, err)
	}
	return resolveThroughExisting(absolute), nil
}

// resolveThroughExisting resolves as much of a path as exists and keeps the
// rest verbatim.
//
// A path that does not exist yet still has to normalize: refusing to map a
// directory the user is about to create would be a surprise with no upside. But
// resolving nothing at all is worse — on macOS every temp path runs through a
// symlinked /var, so a directory that exists and its not-yet-created child
// would normalize to two different roots and the child would stop resolving
// against its own parent's mapping.
//
// So: walk up to the longest existing ancestor, resolve THAT, and re-append
// what was trimmed.
func resolveThroughExisting(absolute string) string {
	var trailing []string
	current := absolute
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Join(append([]string{resolved}, trailing...)...)
		}
		parent := filepath.Dir(current)
		if parent == current {
			// Reached the root with nothing resolvable.
			return filepath.Clean(absolute)
		}
		trailing = append([]string{filepath.Base(current)}, trailing...)
		current = parent
	}
}

// Load returns the whole table, empty when there is none or it is unusable.
//
// A corrupt mappings file is not worth refusing to start over: it holds
// conveniences, not credentials, and rebuilding it costs the user a few
// commands.
func (s *Store) Load() map[string]*Entry {
	text, err := fsutil.ReadText(s.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Warn("the mappings table could not be read", "path", s.path, "error", err)
		}
		return map[string]*Entry{}
	}
	var parsed table
	if err := json.Unmarshal([]byte(text), &parsed); err != nil || parsed.Mappings == nil {
		if err != nil {
			slog.Warn("the mappings table is malformed", "path", s.path, "error", err)
		}
		return map[string]*Entry{}
	}
	return parsed.Mappings
}

// Get looks up one directory exactly, with no ancestor walk.
func (s *Store) Get(path string) (*Entry, bool, error) {
	key, err := NormalizePath(path)
	if err != nil {
		return nil, false, err
	}
	entry, ok := s.Load()[key]
	return entry, ok, nil
}

// Set maps a directory to an account.
func (s *Store) Set(path string, identity Identity) (string, error) {
	key, err := NormalizePath(path)
	if err != nil {
		return "", err
	}
	table := s.Load()
	table[key] = &Entry{
		Email:            identity.Email,
		OrganizationUUID: identity.OrganizationUUID,
		Added:            s.now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	if err := s.write(table); err != nil {
		return "", err
	}
	return key, nil
}

// Remove drops one directory's mapping, reporting whether there was one.
func (s *Store) Remove(path string) (bool, error) {
	key, err := NormalizePath(path)
	if err != nil {
		return false, err
	}
	table := s.Load()
	if _, ok := table[key]; !ok {
		return false, nil
	}
	delete(table, key)
	return true, s.write(table)
}

// PruneAccount drops every mapping pointing at an account, returning how many
// went.
//
// Called when an account is removed: a mapping to an account that no longer
// exists would silently send `ccswap run` looking for it.
func (s *Store) PruneAccount(identity Identity) (int, error) {
	table := s.Load()
	removed := 0
	for key, entry := range table {
		if entry.Identity() == identity {
			delete(table, key)
			removed++
		}
	}
	if removed == 0 {
		return 0, nil
	}
	return removed, s.write(table)
}

// Resolve returns the nearest mapped ancestor of a directory.
//
// The MOST SPECIFIC match wins, so a nested folder inherits the closest
// mapping rather than the outermost one — a monorepo can map one package to a
// different account than the repository root.
func (s *Store) Resolve(dir string) (key string, entry *Entry, found bool) {
	target, err := NormalizePath(dir)
	if err != nil {
		return "", nil, false
	}

	best, bestDepth := "", -1
	var bestEntry *Entry
	for candidate, mapped := range s.Load() {
		if !isSelfOrAncestor(candidate, target) {
			continue
		}
		// Depth in path SEPARATORS, not string length: a longer name at the
		// same depth is not a closer match.
		depth := strings.Count(candidate, string(os.PathSeparator))
		if depth > bestDepth {
			best, bestEntry, bestDepth = candidate, mapped, depth
		}
	}
	if bestEntry == nil {
		return "", nil, false
	}
	return best, bestEntry, true
}

// isSelfOrAncestor reports whether candidate is target or contains it.
//
// Compared on path components rather than as a string prefix: "/home/a" is not
// an ancestor of "/home/abc", however much the strings agree.
func isSelfOrAncestor(candidate, target string) bool {
	if candidate == target {
		return true
	}
	rel, err := filepath.Rel(candidate, target)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// write publishes the table atomically.
func (s *Store) write(mappings map[string]*Entry) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("%w: creating the backup directory: %w", apperr.ErrConfig, err)
	}
	// Deterministic so the file is stable across writes; a user may keep it in
	// version control alongside their dotfiles.
	data, err := json.Marshal(
		table{SchemaVersion: SchemaVersion, Mappings: mappings},
		jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("%w: encoding the mappings table: %w", apperr.ErrConfig, err)
	}
	return fsutil.WriteFileAtomic(s.path, append(data, '\n'))
}
