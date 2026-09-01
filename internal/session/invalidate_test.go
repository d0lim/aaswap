package session

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// seedProfile gives a slot a bootstrapped profile holding a credential.
func (f *fixture) seedProfile(num, email, credentials string) string {
	f.t.Helper()
	dir := f.Dir(num, email)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".credentials.json"), []byte(credentials), 0o600); err != nil {
		f.t.Fatal(err)
	}
	return dir
}

// markLive plants a session record naming a process that is certainly running,
// which is what a profile with Claude Code attached to it looks like.
func (f *fixture) markLive(sessionDir string) {
	f.t.Helper()
	dir := filepath.Join(sessionDir, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	record := fmt.Sprintf(
		`{"pid":%d,"sessionId":"s-1","cwd":"/w","startedAt":1700000000000,`+
			`"kind":"interactive","entrypoint":"cli","status":"idle"}`, os.Getpid())
	if err := os.WriteFile(filepath.Join(dir, "1.json"), []byte(record), 0o600); err != nil {
		f.t.Fatal(err)
	}
}

// A quiet profile's credential is dropped so the next launch re-bootstraps.
// Everything else it accumulated is not the credential's business.
func TestAQuietProfileLosesOnlyItsCredential(t *testing.T) {
	f := newFixture(t)
	dir := f.seedProfile("1", "a@example.com", `{"claudeAiOauth":{"accessToken":"old"}}`)
	history := filepath.Join(dir, "history.jsonl")
	if err := os.WriteFile(history, []byte("kept\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	outcome, err := f.InvalidateForSlot("1", "a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Cleared {
		t.Errorf("outcome = %v, want Cleared", outcome)
	}
	if f.MayHaveCredentialMaterial(dir) {
		t.Error("the superseded credential survived, so the reuse check will hand it back")
	}
	if _, err := os.Stat(history); err != nil {
		t.Errorf("the profile's history was destroyed with its credential: %v", err)
	}
}

// A live profile is left strictly alone. Claude Code manages that file while it
// runs, and pulling credentials out from under it is worse than the drift — so
// the marker is what makes the next quiet launch re-bootstrap.
func TestALiveProfileIsMarkedRatherThanTouched(t *testing.T) {
	f := newFixture(t)
	dir := f.seedProfile("1", "a@example.com", `{"claudeAiOauth":{"accessToken":"in-use"}}`)
	f.markLive(dir)

	outcome, err := f.InvalidateForSlot("1", "a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if outcome != Marked {
		t.Fatalf("outcome = %v, want Marked", outcome)
	}
	if !IsStale(dir) {
		t.Error("no stale marker, so the profile will never re-bootstrap")
	}
	data, err := os.ReadFile(filepath.Join(dir, ".credentials.json"))
	if err != nil || string(data) != `{"claudeAiOauth":{"accessToken":"in-use"}}` {
		t.Error("a running session's credential was modified underneath it")
	}
}

// A slot nobody ever ran has nothing to invalidate, and must not have a profile
// directory conjured for it.
func TestASlotWithNoProfileIsLeftAlone(t *testing.T) {
	f := newFixture(t)

	outcome, err := f.InvalidateForSlot("1", "a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if outcome != NoProfile {
		t.Errorf("outcome = %v, want NoProfile", outcome)
	}
	if _, err := os.Stat(f.Dir("1", "a@example.com")); err == nil {
		t.Error("invalidation created a profile directory for a slot that had none")
	}
}

// Invalidating twice is not an error, and the second pass has nothing to do.
func TestInvalidationIsIdempotent(t *testing.T) {
	f := newFixture(t)
	f.seedProfile("1", "a@example.com", `{"claudeAiOauth":{"accessToken":"old"}}`)

	if _, err := f.InvalidateForSlot("1", "a@example.com"); err != nil {
		t.Fatal(err)
	}
	outcome, err := f.InvalidateForSlot("1", "a@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if outcome != AlreadyClear {
		t.Errorf("outcome = %v on a second pass, want AlreadyClear", outcome)
	}
}
