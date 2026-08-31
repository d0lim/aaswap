package credstore

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/realiti4/claude-swap/internal/apperr"
	"github.com/realiti4/claude-swap/internal/fsutil"
)

// The stash preserves credentials of unknown provenance.
//
// A switch overwrites the machine's live credential with a slot's stored one.
// When the live bytes are positively NOT the departing slot's — another
// account's token, an unmanaged login, bytes something outside cswap wrote —
// they may go into no slot, because filing them would destroy that slot's only
// refresh token. But they may not simply be destroyed either: they can be the
// only live copy of some account's refresh token anywhere.
//
// So they are stashed, and a successful stash is the LICENSE to overwrite the
// live store. A failed stash aborts the switch.
const (
	stashPrefix       = ".unclaimed-"
	stashSuffix       = ".enc"
	stashManifestName = ".unclaimed-manifest.json"
)

// StashVerdict is how a manifest read went.
//
// "No rows" and "could not establish the rows" have opposite consequences: a
// stash row is the sole record of a generation something already consumed, so a
// caller reading a failure as "nothing stashed" would spend the spent
// generation. The failure is correlated rather than independent, too — the stash
// exists BECAUSE storage I/O already failed once.
type StashVerdict string

const (
	// StashOK means the rows were established, empty or not.
	StashOK StashVerdict = "ok"
	// StashUnreadable means the bytes exist and could not be read — a locked
	// Keychain, an I/O error, a mode. Transient and self-clearing, so a caller
	// defers: it costs a pass and spends nothing.
	StashUnreadable StashVerdict = "unreadable"
	// StashCorrupt means the bytes were read and are not a manifest. Permanent,
	// so a blanket refusal would deadlock the slot forever.
	StashCorrupt StashVerdict = "corrupt"
)

// StashEntry is one preserved credential's metadata.
//
// Extra keeps members a newer writer added, so a manifest rewritten by an older
// reader does not lose them.
type StashEntry struct {
	CreatedAt string `json:"createdAt,omitzero"`
	// Reason names why the credential could not be filed — the classifier's
	// verdict.
	Reason string `json:"reason,omitzero"`
	// ConfigSlot is the slot the live config named at the time, which is not
	// the same as the slot the bytes belong to. That is the whole point.
	ConfigSlot string `json:"configSlot,omitzero"`
	// Fingerprint identifies the lineage without storing the token.
	Fingerprint string `json:"fingerprint,omitzero"`
	// LiveOAuthAccount and ResolvedIdentity are the two identities that
	// disagreed: what the config claimed, and what the endpoint answered.
	LiveOAuthAccount jsontext.Value `json:"liveOauthAccount,omitzero"`
	ResolvedIdentity jsontext.Value `json:"resolvedIdentity,omitzero"`
	// CredentialsMtime is when the live credentials file was last written.
	// Evidence for identifying what rewrote it.
	CredentialsMtime string `json:"credentialsMtime,omitzero"`
	// ConsumedFP is the generation this entry's credential SUPERSEDED, and is
	// the adoption key: a later pass that finds the slot still holding exactly
	// that generation knows this entry is the persist that never completed.
	ConsumedFP string `json:"consumedFp,omitzero"`

	Extra map[string]jsontext.Value `json:",embed"`
}

type stashManifest struct {
	Entries map[string]*StashEntry    `json:"entries"`
	Extra   map[string]jsontext.Value `json:",embed"`
}

func (s *Store) stashManifestPath() string {
	return filepath.Join(s.CredentialsDir(), stashManifestName)
}

func (s *Store) stashEntryPath(entryID string) string {
	return filepath.Join(s.CredentialsDir(), stashPrefix+entryID+stashSuffix)
}

// newStashID mints an entry id.
//
// The timestamp makes the directory listing chronological, the digest ties the
// name to the bytes, and the nonce keeps ids unique for identical bytes
// preserved in the same second — the stash is append-only, so no write may ever
// land on an existing id.
func newStashID(credentials string, now time.Time) (string, error) {
	digest := sha256.Sum256([]byte(credentials))
	var nonce [3]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("generating a stash id: %w", err)
	}
	return fmt.Sprintf("%s-%s-%s",
		now.UTC().Format("20060102T150405"),
		hex.EncodeToString(digest[:])[:12],
		hex.EncodeToString(nonce[:]),
	), nil
}

// WriteUnclaimed preserves a credential of unknown provenance, returning its
// entry id.
//
// It fails loudly, because the caller uses success as the license to overwrite
// the live store. The entry file is written BEFORE the manifest row: an entry
// with no metadata is recoverable by hand, while a metadata row with no bytes
// is not.
func (s *Store) WriteUnclaimed(credentials string, entry StashEntry, now time.Time) (string, error) {
	entryID, err := newStashID(credentials, now)
	if err != nil {
		return "", err
	}

	encoded := base64.StdEncoding.EncodeToString([]byte(credentials))
	if err := fsutil.WriteFileAtomic(s.stashEntryPath(entryID), []byte(encoded)); err != nil {
		return "", fmt.Errorf("%w: preserving an unclaimed credential: %w",
			apperr.ErrCredentialWrite, err)
	}

	entry.CreatedAt = now.UTC().Format("2006-01-02T15:04:05Z")
	if err := s.mutateStashManifest(func(entries map[string]*StashEntry) {
		entries[entryID] = &entry
	}); err != nil {
		return "", err
	}
	return entryID, nil
}

// ListUnclaimed returns every stashed entry by id, with the verdict for the
// manifest read.
//
// An entry file with no manifest row still appears, carrying no metadata: the
// bytes are what matter, and a row lost to an interrupted write must not hide
// them.
func (s *Store) ListUnclaimed() (map[string]*StashEntry, StashVerdict) {
	entries, verdict := s.readStashManifest()
	if entries == nil {
		entries = map[string]*StashEntry{}
	}

	names, err := os.ReadDir(s.CredentialsDir())
	if err != nil {
		return entries, verdict
	}
	for _, name := range names {
		base := name.Name()
		if !strings.HasPrefix(base, stashPrefix) || !strings.HasSuffix(base, stashSuffix) {
			continue
		}
		entryID := strings.TrimSuffix(strings.TrimPrefix(base, stashPrefix), stashSuffix)
		if entryID == "" {
			continue
		}
		if _, known := entries[entryID]; !known {
			entries[entryID] = &StashEntry{}
		}
	}
	return entries, verdict
}

// ReadUnclaimed decodes one stashed credential, reporting whether the bytes
// exist but could not be read.
//
// The two negative answers are kept apart for the same reason the backup read
// keeps them apart: "there is nothing here" and "there is something here I
// cannot see" lead to opposite decisions.
func (s *Store) ReadUnclaimed(entryID string) (value string, unreadable bool) {
	encoded, err := fsutil.ReadText(s.stashEntryPath(entryID))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false
		}
		slog.Warn("a stashed credential could not be read", "entry", entryID, "error", err)
		return "", true
	}
	decoded, err := decodeBackup(encoded)
	if err != nil {
		slog.Warn("a stashed credential is corrupt", "entry", entryID, "error", err)
		return "", true
	}
	return decoded, false
}

// DeleteUnclaimed retires a stashed entry: its bytes and its manifest row.
//
// The bytes go first. An orphaned manifest row is cosmetic; orphaned bytes are
// a credential nobody knows about.
func (s *Store) DeleteUnclaimed(entryID string) error {
	if err := os.Remove(s.stashEntryPath(entryID)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: retiring stashed credential %s: %w",
			apperr.ErrCredentialWrite, entryID, err)
	}
	return s.mutateStashManifest(func(entries map[string]*StashEntry) {
		delete(entries, entryID)
	})
}

// readStashManifest reads the manifest with a three-way verdict.
func (s *Store) readStashManifest() (map[string]*StashEntry, StashVerdict) {
	// Read as bytes so the two failure classes cannot cross: undecodable bytes
	// were READABLE and their content is garbage, which is corrupt, not
	// unreadable.
	raw, err := os.ReadFile(s.stashManifestPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]*StashEntry{}, StashOK
		}
		slog.Warn("the unclaimed manifest is unreadable", "error", err)
		return nil, StashUnreadable
	}

	var manifest stashManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		slog.Warn("the unclaimed manifest is corrupt", "error", err)
		return nil, StashCorrupt
	}
	if manifest.Entries == nil {
		return map[string]*StashEntry{}, StashOK
	}
	return manifest.Entries, StashOK
}

// mutateStashManifest applies a change to the manifest and republishes it.
//
// A read failure aborts rather than rewriting from an assumed-empty manifest:
// that would drop every existing row, and each row is the only record of a
// generation that may already have been consumed.
func (s *Store) mutateStashManifest(apply func(entries map[string]*StashEntry)) error {
	entries, verdict := s.readStashManifest()
	if verdict == StashUnreadable {
		return fmt.Errorf("%w: the unclaimed manifest could not be read, so it cannot "+
			"be updated without dropping the rows already in it", apperr.ErrCredentialWrite)
	}
	// A CORRUPT manifest is rebuilt: the rows are unrecoverable either way, and
	// refusing forever would wedge every future stash.
	if entries == nil {
		entries = map[string]*StashEntry{}
	}
	apply(entries)

	data, err := json.Marshal(stashManifest{Entries: entries},
		jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("%w: encoding the unclaimed manifest: %w",
			apperr.ErrCredentialWrite, err)
	}
	if err := fsutil.WriteFileAtomic(s.stashManifestPath(), append(data, '\n')); err != nil {
		return fmt.Errorf("%w: writing the unclaimed manifest: %w",
			apperr.ErrCredentialWrite, err)
	}
	return nil
}

// StashEntryFilesExist reports whether any stashed credential's bytes are on
// disk.
//
// Consulted when the manifest is CORRUPT: with no entry files, an empty scan is
// not a guess about a pending successor — there provably is none — and
// proceeding lets the next write set the bad manifest aside, which is the only
// repair. With entry files present, an empty scan would make a caller spend a
// generation some pass already superseded.
func (s *Store) StashEntryFilesExist() bool {
	names, err := os.ReadDir(s.CredentialsDir())
	if err != nil {
		// Unreadable: assume the worst, because the alternative discards
		// evidence of a spent generation.
		return true
	}
	for _, name := range names {
		base := name.Name()
		if strings.HasPrefix(base, stashPrefix) && strings.HasSuffix(base, stashSuffix) {
			return true
		}
	}
	return false
}

// SortedStashIDs orders entry ids, which sorts them chronologically because the
// id begins with a timestamp.
func SortedStashIDs(entries map[string]*StashEntry) []string {
	ids := make([]string, 0, len(entries))
	for id := range entries {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// CredentialsMtime is when the live credentials file was last written, reporting
// false when there is no such file — a Keychain-backed store, or a logged-out
// machine.
func (s *Store) CredentialsMtime() (time.Time, bool) {
	info, err := os.Stat(s.paths.CredentialsPath())
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime().UTC(), true
}
