package procdetect

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/d0lim/ccswap/internal/testutil"
)

// writeSession puts one session record under a config directory.
func writeSession(t *testing.T, configDir, name, content string) {
	t.Helper()
	dir := filepath.Join(configDir, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// livePID is this process, which is certainly running.
func livePID() int { return os.Getpid() }

// deadPID is a process id nothing plausibly holds.
const deadPID = 4194303

func TestScanFindsLiveSessions(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "1.json", fmt.Sprintf(
		`{"pid":%d,"sessionId":"s-1","cwd":"/w","startedAt":1700000000000,"kind":"interactive","entrypoint":"cli","status":"idle"}`,
		livePID()))

	sessions, unreadable := Scan(dir)
	if unreadable != 0 {
		t.Errorf("unreadable = %d, want 0", unreadable)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %v", sessions)
	}
	got := sessions[0]
	if got.PID != livePID() || got.SessionID != "s-1" || got.Kind != "interactive" {
		t.Errorf("session = %+v", got)
	}
	if got.Status != "idle" || got.StartedAt != 1700000000000 {
		t.Errorf("session = %+v", got)
	}
}

// A record for a process that is gone read perfectly; it just says nothing is
// running. It is not unreadable.
func TestADeadProcessIsNotUnreadable(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "1.json", fmt.Sprintf(`{"pid":%d}`, deadPID))

	sessions, unreadable := Scan(dir)
	if len(sessions) != 0 {
		t.Errorf("sessions = %v, want none", sessions)
	}
	if unreadable != 0 {
		t.Errorf("unreadable = %d, want 0 — the record was perfectly readable", unreadable)
	}
	if !Quiescent(dir) {
		t.Error("a profile with only dead records is not quiescent")
	}
}

// "No live sessions" and "no readable records" produce the same empty list, and
// only the first is safe to act on.
func TestAnUnreadableRecordIsCounted(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"malformed JSON", `{"pid":`},
		{"not an object", `[1,2,3]`},
		{"no pid", `{"sessionId":"s-1"}`},
		{"a pid of the wrong type", `{"pid":"one"}`},
		{"empty", ``},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSession(t, dir, "1.json", tt.content)

			sessions, unreadable := Scan(dir)
			if len(sessions) != 0 {
				t.Errorf("sessions = %v", sessions)
			}
			if unreadable != 1 {
				t.Errorf("unreadable = %d, want 1", unreadable)
			}
			// The whole point: a guard must not read this as "nobody there".
			if Quiescent(dir) {
				t.Error("a profile with an unreadable record read as quiescent")
			}
		})
	}
}

// One bad file must not take out the whole listing.
func TestOneBadRecordDoesNotHideTheOthers(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "good.json", fmt.Sprintf(`{"pid":%d,"sessionId":"s-1"}`, livePID()))
	writeSession(t, dir, "bad.json", `{oops`)

	sessions, unreadable := Scan(dir)
	if len(sessions) != 1 || sessions[0].SessionID != "s-1" {
		t.Errorf("sessions = %v, want the readable one", sessions)
	}
	if unreadable != 1 {
		t.Errorf("unreadable = %d, want 1", unreadable)
	}
}

// Nothing has ever run against a profile with no sessions directory. That is an
// answer, not a gap.
func TestAnAbsentDirectoryIsQuiescent(t *testing.T) {
	dir := t.TempDir()
	sessions, unreadable := Scan(dir)
	if len(sessions) != 0 || unreadable != 0 {
		t.Errorf("Scan = (%v, %d), want nothing and no doubt", sessions, unreadable)
	}
	if !Quiescent(dir) {
		t.Error("a profile that has never run is not quiescent")
	}
}

// A directory that exists and cannot be listed IS indeterminate.
func TestAnUnlistableDirectoryIsNotQuiescent(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	testutil.MakeUnreadable(t, sessions)
	t.Cleanup(func() { _ = os.Chmod(sessions, 0o700) })

	if Quiescent(dir) {
		t.Error("an unlistable sessions directory read as quiescent")
	}
}

// Non-JSON files in the directory are not records.
func TestNonJSONFilesAreIgnored(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "notes.txt", "nothing to see")
	writeSession(t, dir, "1.json", fmt.Sprintf(`{"pid":%d}`, livePID()))

	sessions, unreadable := Scan(dir)
	if len(sessions) != 1 || unreadable != 0 {
		t.Errorf("Scan = (%v, %d)", sessions, unreadable)
	}
}

func TestPIDs(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "1.json", fmt.Sprintf(`{"pid":%d}`, livePID()))
	writeSession(t, dir, "2.json", fmt.Sprintf(`{"pid":%d}`, deadPID))

	got := PIDs(dir)
	if len(got) != 1 || got[0] != livePID() {
		t.Errorf("PIDs = %v, want just the live one", got)
	}
}

func TestPIDAlive(t *testing.T) {
	tests := []struct {
		name string
		pid  int
		want bool
	}{
		{"this process", os.Getpid(), true},
		{"a pid nothing holds", deadPID, false},
		// A process group, not a process.
		{"zero", 0, false},
		// init, which is never a Claude Code instance.
		{"one", 1, false},
		{"negative", -1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PIDAlive(tt.pid); got != tt.want {
				t.Errorf("PIDAlive(%d) = %v, want %v", tt.pid, got, tt.want)
			}
		})
	}
}

func TestIDEInstances(t *testing.T) {
	dir := t.TempDir()
	ide := filepath.Join(dir, "ide")
	if err := os.MkdirAll(ide, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string) {
		if err := os.WriteFile(filepath.Join(ide, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("40123.lock", fmt.Sprintf(
		`{"pid":%d,"ideName":"Visual Studio Code","workspaceFolders":["/w"]}`, livePID()))
	write("40124.lock", fmt.Sprintf(`{"pid":%d,"ideName":"Cursor"}`, deadPID))
	// The port comes from the NAME, so a lockfile with an unparseable one is
	// not an instance.
	write("notaport.lock", fmt.Sprintf(`{"pid":%d}`, livePID()))
	write("40125.lock", `{oops`)

	got := IDEInstances(dir)
	if len(got) != 1 {
		t.Fatalf("IDEInstances = %+v, want just the live one", got)
	}
	if got[0].Port != 40123 || got[0].IDEName != "Visual Studio Code" {
		t.Errorf("instance = %+v", got[0])
	}
	if len(got[0].WorkspaceFolders) != 1 || got[0].WorkspaceFolders[0] != "/w" {
		t.Errorf("workspaceFolders = %v", got[0].WorkspaceFolders)
	}
}

// An instance that names no editor still has to render as something.
func TestAnUnnamedIDEGetsAPlaceholder(t *testing.T) {
	dir := t.TempDir()
	ide := filepath.Join(dir, "ide")
	if err := os.MkdirAll(ide, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ide, "40123.lock"),
		fmt.Appendf(nil, `{"pid":%d}`, livePID()), 0o600); err != nil {
		t.Fatal(err)
	}
	got := IDEInstances(dir)
	if len(got) != 1 || got[0].IDEName != "Unknown IDE" {
		t.Errorf("IDEInstances = %+v", got)
	}
}
