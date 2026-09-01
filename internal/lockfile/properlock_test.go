package lockfile

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/d0lim/ccswap/internal/apperr"
)

// fastOpts keeps the lock protocol's shape while collapsing its timings, so the
// tests exercise real behaviour without real waits.
func fastOpts(staleness time.Duration) ProperOptions {
	return ProperOptions{
		Timeout:       50 * time.Millisecond,
		Staleness:     staleness,
		TouchInterval: 20 * time.Millisecond,
	}
}

func TestProperLockCreatesAndRemovesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".oauth_refresh.lock")

	h, err := acquireProper(dir, fastOpts(time.Minute))
	if err != nil {
		t.Fatalf("acquireProper: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("lock directory was not created: %v", err)
	}
	if !info.IsDir() {
		t.Error("lock artifact is not a directory; proper-lockfile compatibility relies on mkdir atomicity")
	}

	h.release()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("lock directory survived release")
	}
}

func TestProperLockReacquiresAfterRelease(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config.lock")

	h, err := acquireProper(dir, fastOpts(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	h.release()

	h2, err := acquireProper(dir, fastOpts(time.Minute))
	if err != nil {
		t.Fatalf("reacquire after release: %v", err)
	}
	h2.release()
}

func TestProperLockCreatesMissingParent(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing", "parent", "x.lock")

	h, err := acquireProper(dir, fastOpts(time.Minute))
	if err != nil {
		t.Fatalf("acquireProper: %v", err)
	}
	defer h.release()
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("lock directory was not created under a missing parent: %v", err)
	}
}

// A lock held by a live holder must time out rather than be stolen, and the
// error has to be the Claude-Code-specific one so the CLI can say "retry in a
// few seconds" instead of reporting a defect.
func TestProperLockContentionTimesOut(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "held.lock")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	_, err := acquireProper(dir, fastOpts(time.Minute))
	if !errors.Is(err, apperr.ErrClaudeCodeLockTimeout) {
		t.Fatalf("error = %v, want it to wrap apperr.ErrClaudeCodeLockTimeout", err)
	}
	if !errors.Is(err, apperr.ErrLock) {
		t.Error("a Claude Code lock timeout must still register as a lock error")
	}
	// The live holder's directory must be left alone.
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("contended lock directory was disturbed: %v", err)
	}
}

func TestProperLockTakesOverAStaleLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "abandoned.lock")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-time.Second)
	if err := os.Chtimes(dir, stale, stale); err != nil {
		t.Fatal(err)
	}

	h, err := acquireProper(dir, fastOpts(100*time.Millisecond))
	if err != nil {
		t.Fatalf("a lock older than the staleness budget was not taken over: %v", err)
	}
	defer h.release()
}

// Releasing a lock that someone else already removed (took over as stale) must
// not blow up: the release path runs in a defer, after the real work.
func TestProperLockReleaseToleratesAStolenLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stolen.lock")

	h, err := acquireProper(dir, fastOpts(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	h.release() // must not panic
}

// The toucher is what stops a *live* holder from being deemed stale while it
// does slow work under the lock.
func TestProperLockTouchesWhileHeld(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "touched.lock")

	h, err := acquireProper(dir, fastOpts(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	defer h.release()

	// Age the directory, then let the toucher run.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(dir, old, old); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)

	after, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().After(old) {
		t.Errorf("mtime %v was not refreshed while the lock was held", after.ModTime())
	}
}

func TestWithProperRunsAndReleases(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scoped.lock")

	ran := false
	if err := withProper(dir, fastOpts(time.Minute), func() error {
		ran = true
		if _, err := os.Stat(dir); err != nil {
			t.Errorf("lock was not held inside the callback: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatalf("withProper: %v", err)
	}
	if !ran {
		t.Error("withProper did not run the callback")
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("lock survived withProper")
	}
}

func TestWithProperPropagatesCallbackErrorAndStillReleases(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "scoped.lock")
	sentinel := errors.New("boom")

	if err := withProper(dir, fastOpts(time.Minute), func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("withProper = %v, want the callback's error", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Error("lock survived a failing callback")
	}
}

func TestProperOptionsDefaults(t *testing.T) {
	got := ProperOptions{}.withDefaults()
	if got.Timeout != DefaultProperTimeout {
		t.Errorf("Timeout = %v, want %v", got.Timeout, DefaultProperTimeout)
	}
	if got.Staleness != ConfigStaleness {
		t.Errorf("Staleness = %v, want %v", got.Staleness, ConfigStaleness)
	}
	if got.TouchInterval != TouchInterval {
		t.Errorf("TouchInterval = %v, want %v", got.TouchInterval, TouchInterval)
	}
	if got.Logger == nil {
		t.Error("Logger was left nil")
	}
}

// The staleness split is load-bearing: Claude Code runs stale: 60000 on the
// credential locks and the older 10s on the config lock. Getting these the
// wrong way round would let ccswap steal a lock a live Claude Code still holds.
func TestStalenessConstantsMatchClaudeCode(t *testing.T) {
	if CredentialsStaleness != 60*time.Second {
		t.Errorf("CredentialsStaleness = %v, want 60s to match Claude Code's stale: 60000", CredentialsStaleness)
	}
	if ConfigStaleness != 10*time.Second {
		t.Errorf("ConfigStaleness = %v, want 10s to match proper-lockfile's default", ConfigStaleness)
	}
	if TouchInterval >= 5*time.Second {
		t.Errorf("TouchInterval = %v, want it below Claude Code's 5s update interval for margin", TouchInterval)
	}
}
