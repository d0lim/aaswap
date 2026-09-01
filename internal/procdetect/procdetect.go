// Package procdetect finds running Claude Code instances.
//
// It reads the same records Claude Code writes for itself: a JSON file per
// process under sessions/, and a lockfile per editor connection under ide/.
// Nothing here starts or stops anything — it answers "is something running
// against this profile", which is what every destructive step has to ask
// first.
package procdetect

import (
	json "encoding/json/v2"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
)

// Session is one running Claude Code process.
type Session struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	// StartedAt is epoch milliseconds, as Claude Code writes it.
	StartedAt int64 `json:"startedAt"`
	// Kind is "interactive", "bg", "daemon" or "daemon-worker".
	Kind string `json:"kind"`
	// Entrypoint is "cli", "claude-vscode", "claude-desktop", "sdk-cli" or "mcp".
	Entrypoint string `json:"entrypoint"`
	// Status is "busy", "idle" or "waiting" when the instance reports one.
	Status string `json:"status"`
}

// IDEInstance is one editor connection.
type IDEInstance struct {
	// Port comes from the lockfile's name, not its contents.
	Port             int
	PID              int      `json:"pid"`
	IDEName          string   `json:"ideName"`
	WorkspaceFolders []string `json:"workspaceFolders"`
}

// Scan returns the live sessions under a config directory, and how many records
// could NOT be read.
//
// The count travels with the list because two kinds of caller need opposite
// things from an unreadable record. A LISTING wants it skipped — one bad file
// must not take out the whole display. A GUARD needs to know: "no live
// sessions" and "no readable records" produce the same empty list, and only the
// first is safe to act on. The steps behind those guards overwrite a profile's
// credentials or delete an account, so reading "could not tell" as "nobody
// there" runs them underneath a live instance.
func Scan(configDir string) (sessions []Session, unreadable int) {
	dir := filepath.Join(configDir, "sessions")
	entries, err := os.ReadDir(dir)
	if err != nil {
		// An absent directory is not an unreadable record: nothing has ever run
		// against this profile.
		if !os.IsNotExist(err) {
			slog.Debug("the sessions directory could not be listed", "path", dir, "error", err)
			// Unlistable IS indeterminate, and a guard must treat it as such.
			return nil, 1
		}
		return nil, 0
	}

	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			unreadable++
			slog.Debug("skipping an unreadable session record", "path", path, "error", err)
			continue
		}
		var session Session
		if err := json.Unmarshal(data, &session); err != nil || session.PID == 0 {
			unreadable++
			slog.Debug("skipping a malformed session record", "path", path, "error", err)
			continue
		}
		if !PIDAlive(session.PID) {
			// A record for a process that is gone is not unreadable — it read
			// perfectly and says nothing is running.
			continue
		}
		sessions = append(sessions, session)
	}
	return sessions, unreadable
}

// Quiescent reports that nothing is running against a profile AND every record
// was readable.
//
// The predicate every "is it safe to rewrite or remove this profile" site
// wants. False when a record could not be read: not knowing is not the same as
// knowing nothing is there, and the step behind these callers cannot be undone.
func Quiescent(configDir string) bool {
	sessions, unreadable := Scan(configDir)
	return len(sessions) == 0 && unreadable == 0
}

// PIDs lists the live process ids under a config directory.
func PIDs(configDir string) []int {
	sessions, _ := Scan(configDir)
	pids := make([]int, len(sessions))
	for i, session := range sessions {
		pids[i] = session.PID
	}
	return pids
}

// IDEInstances lists the editor connections under a config directory.
func IDEInstances(configDir string) []IDEInstance {
	dir := filepath.Join(configDir, "ide")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var out []IDEInstance
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".lock" {
			continue
		}
		port, err := strconv.Atoi(name[:len(name)-len(".lock")])
		if err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var instance IDEInstance
		if err := json.Unmarshal(data, &instance); err != nil || instance.PID == 0 {
			continue
		}
		if !PIDAlive(instance.PID) {
			continue
		}
		instance.Port = port
		if instance.IDEName == "" {
			instance.IDEName = "Unknown IDE"
		}
		out = append(out, instance)
	}
	return out
}

// PIDAlive reports whether a process is running.
//
// Signal 0 performs the existence and permission checks without delivering
// anything. A permission error means the process EXISTS and belongs to someone
// else, which is still alive — reading that as dead would let a guard run
// underneath another user's instance.
//
// PIDs at or below 1 are never a Claude Code instance: 0 addresses a process
// group and 1 is init.
func PIDAlive(pid int) bool {
	if pid <= 1 {
		return false
	}
	return pidAlive(pid)
}
