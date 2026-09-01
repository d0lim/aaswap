package swap

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/d0lim/ccswap/internal/apperr"
	"github.com/d0lim/ccswap/internal/fsutil"
)

// object is a decoded JSON object with every member preserved.
//
// jsontext.Value rather than any, so a member this version does not understand
// round-trips byte-for-byte. The live Claude Code config passes through here,
// and dropping a key it owns would silently delete the user's projects or MCP
// servers.
type object = map[string]jsontext.Value

// readObject reads a JSON object, distinguishing three outcomes that callers
// keep conflating:
//
//   - the file is ABSENT: (nil, false, nil). A genuine empty start.
//   - the file is THERE but unreadable: (nil, false, error). It must NOT be
//     overwritten unread.
//   - success: (object, true, nil).
//
// A file holding a JSON array, string or number is "unreadable": every caller
// here expects an object, and letting a bare `123` through only moves the
// failure to whichever line first indexes it.
//
// The distinction is the whole point. A torn ~/.claude.json read as "absent"
// once made a switch write a one-key backup config over the user's entire file,
// taking projects, mcpServers and userID with it — and a torn sequence.json
// read as "no accounts" made the next write rebuild the roster from nothing,
// overwriting a live credential backup on the way.
func readObject(path string) (object, bool, error) {
	text, err := fsutil.ReadText(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		slog.Warn("could not read a JSON file", "path", path, "error", err)
		return nil, false, fmt.Errorf("%w: %s exists but could not be read (%w); "+
			"fix what is blocking the read, then retry", apperr.ErrConfig, path, err)
	}

	var data object
	if err := json.Unmarshal([]byte(text), &data); err != nil {
		slog.Warn("invalid JSON", "path", path, "error", err)
		return nil, false, fmt.Errorf("%w: %s exists but could not be parsed (%w); "+
			"repair or move it, then retry — refusing to overwrite it unread",
			apperr.ErrConfig, path, err)
	}
	if data == nil {
		// A literal `null`, or a payload that is not an object at all.
		return nil, false, fmt.Errorf("%w: %s does not hold a JSON object; "+
			"repair or move it, then retry", apperr.ErrConfig, path)
	}
	return data, true, nil
}

// readObjectLenient reads a JSON object, treating an unreadable file as absent.
//
// For the one caller that deliberately REPLACES a malformed file it is about to
// seed. Everything else wants [readObject]: a blanket "unreadable means empty"
// is what destroyed configs before the distinction existed.
func readObjectLenient(path string) (object, bool) {
	data, ok, err := readObject(path)
	if err != nil {
		return nil, false
	}
	return data, ok
}

// writeJSON writes a JSON document atomically with owner-only permissions.
//
// Indented two spaces, matching what the Python implementation writes, so a
// user diffing the file across a version change sees their own edits and not a
// reformat.
func writeJSON(path string, value any) error {
	data, err := json.Marshal(value, jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Errorf("%w: generated invalid JSON for %s: %w", apperr.ErrConfig, path, err)
	}
	// A trailing newline: these are files people open in an editor.
	return fsutil.WriteFileAtomic(path, append(data, '\n'))
}

// salvageUnreadable copies an unreadable file aside before it is replaced, and
// returns the copy's path.
//
// The promise is that the bytes survive and the user is told. Four details
// carry it, each one learned the hard way:
//
//   - The copy is made WITHOUT the source's mode and then chmod'ed to 0600. A
//     world-readable ~/.claude.json holding an API key would otherwise be
//     salvaged into a world-readable copy ccswap itself created.
//   - The name is disambiguated with a counter. The stamp is second-resolution,
//     and two failed switches inside one second would otherwise leave one file —
//     losing the first user's data exactly when the retry, which is what a user
//     does next, made the guard matter.
//   - The stamp is a Unix second, not an ISO timestamp. ISO carries a colon,
//     which is forbidden in a Windows filename, and a salvage that raises
//     aborts the switch — worse than the loss it exists to prevent.
//   - A failure here aborts rather than proceeding. Replacing a file whose
//     bytes could not be preserved is the one outcome this must never produce.
func salvageUnreadable(path string, now time.Time) (string, error) {
	dir, name := filepath.Split(path)
	stem := fmt.Sprintf("%s.unreadable-%d", name, now.Unix())

	salvage := filepath.Join(dir, stem)
	for n := 1; ; n++ {
		if _, err := os.Lstat(salvage); errors.Is(err, fs.ErrNotExist) {
			break
		}
		salvage = filepath.Join(dir, fmt.Sprintf("%s.%d", stem, n))
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%w: %s could not be parsed and the salvage copy failed (%w); "+
			"aborting rather than destroying it", apperr.ErrSwitch, path, err)
	}
	if err := os.WriteFile(salvage, data, 0o600); err != nil {
		return "", fmt.Errorf("%w: %s could not be parsed and the salvage copy failed (%w); "+
			"aborting rather than destroying it", apperr.ErrSwitch, path, err)
	}
	if runtime.GOOS != "windows" {
		// WriteFile's mode is masked by the umask, so it is set explicitly.
		if err := os.Chmod(salvage, 0o600); err != nil {
			slog.Warn("could not restrict the salvage copy's permissions",
				"path", salvage, "error", err)
		}
	}

	slog.Warn("a file could not be parsed; a copy was kept before it was replaced",
		"path", path, "salvage", salvage)
	return salvage, nil
}
