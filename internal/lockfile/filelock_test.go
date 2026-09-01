package lockfile

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/apperr"
)

func TestAcquireAndRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")
	l := New(path, time.Second)

	acquired, err := l.Acquire()
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if !acquired {
		t.Fatal("Acquire returned false on an uncontended lock")
	}
	if !l.Held() {
		t.Error("Held() = false after a successful Acquire")
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if l.Held() {
		t.Error("Held() = true after Release")
	}
}

// A deferred Release is the normal shape, so releasing twice — or releasing a
// lock that was never taken — has to be harmless.
func TestDoubleReleaseIsSafe(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "store.lock"), time.Second)
	if err := l.Release(); err != nil {
		t.Errorf("Release on an unheld lock = %v, want nil", err)
	}
	if _, err := l.Acquire(); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("first Release: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Errorf("second Release = %v, want nil", err)
	}
}

func TestAcquireCreatesParentDirectories(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "store.lock")
	if err := With(path, time.Second, func() error { return nil }); err != nil {
		t.Fatalf("With: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("lock file was not created: %v", err)
	}
}

func TestWithRunsAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")

	ran := false
	if err := With(path, time.Second, func() error {
		ran = true
		return nil
	}); err != nil {
		t.Fatalf("With: %v", err)
	}
	if !ran {
		t.Error("With did not run the callback")
	}
	// The lock must be free again immediately afterwards.
	if err := With(path, time.Second, func() error { return nil }); err != nil {
		t.Errorf("second With: %v, want the lock to have been released", err)
	}
}

func TestWithPropagatesCallbackError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")
	sentinel := errors.New("boom")

	err := With(path, time.Second, func() error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Errorf("With = %v, want the callback's error", err)
	}
	// A failing callback must still release the lock.
	if err := With(path, time.Second, func() error { return nil }); err != nil {
		t.Errorf("lock was not released after a failing callback: %v", err)
	}
}

// flock is owned by the open file description, so two independent opens
// conflict even inside one process — which is what makes this testable without
// a second process.
func TestConcurrentAccessIsBlocked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")

	first := New(path, time.Second)
	if acquired, err := first.Acquire(); err != nil || !acquired {
		t.Fatalf("first Acquire: acquired=%v err=%v", acquired, err)
	}
	defer func() { _ = first.Release() }()

	second := New(path, 200*time.Millisecond)
	acquired, err := second.Acquire()
	if err != nil {
		t.Fatalf("second Acquire returned an error rather than a timeout: %v", err)
	}
	if acquired {
		t.Fatal("second Acquire succeeded while the lock was held")
	}

	// Once the holder lets go, the same path is takeable again.
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	third := New(path, time.Second)
	if acquired, err := third.Acquire(); err != nil || !acquired {
		t.Fatalf("Acquire after release: acquired=%v err=%v", acquired, err)
	}
	_ = third.Release()
}

// With must report a timeout as an error: every caller mutates the account
// store, and doing that unlocked is exactly what the lock prevents.
func TestWithReportsTimeoutAsLockError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "store.lock")

	holder := New(path, time.Second)
	if acquired, err := holder.Acquire(); err != nil || !acquired {
		t.Fatalf("holder Acquire: acquired=%v err=%v", acquired, err)
	}
	defer func() { _ = holder.Release() }()

	ran := false
	err := With(path, 150*time.Millisecond, func() error {
		ran = true
		return nil
	})
	if !errors.Is(err, apperr.ErrLock) {
		t.Errorf("With on a held lock = %v, want it to wrap apperr.ErrLock", err)
	}
	if ran {
		t.Error("With ran the callback despite failing to acquire the lock")
	}
}

func TestAcquireIsIdempotentWhileHeld(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "store.lock"), time.Second)
	if _, err := l.Acquire(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Release() }()

	// Re-acquiring a lock this instance already holds must not deadlock
	// against itself.
	acquired, err := l.Acquire()
	if err != nil || !acquired {
		t.Errorf("re-Acquire while held: acquired=%v err=%v", acquired, err)
	}
}

func TestZeroTimeoutUsesDefault(t *testing.T) {
	l := New(filepath.Join(t.TempDir(), "store.lock"), 0)
	if l.timeout != DefaultTimeout {
		t.Errorf("timeout = %v, want the %v default", l.timeout, DefaultTimeout)
	}
}

// The operating system drops an advisory lock when its holder dies, so a
// crashed aaswap cannot leave the account store permanently locked. Verified
// with a real second process, since that is the only way the guarantee is
// actually exercised.
func TestLockIsReleasedWhenTheHolderProcessExits(t *testing.T) {
	if os.Getenv("AASWAP_LOCK_HELPER") != "" {
		// Child: take the lock, announce it, and exit without releasing.
		l := New(os.Getenv("AASWAP_LOCK_HELPER"), 5*time.Second)
		acquired, err := l.Acquire()
		if err != nil || !acquired {
			os.Exit(2)
		}
		os.Exit(0)
	}

	path := filepath.Join(t.TempDir(), "store.lock")
	cmd := exec.Command(os.Args[0], "-test.run=TestLockIsReleasedWhenTheHolderProcessExits")
	cmd.Env = append(os.Environ(), "AASWAP_LOCK_HELPER="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper process failed: %v\n%s", err, out)
	}

	// The child never released; its exit must have.
	l := New(path, 500*time.Millisecond)
	acquired, err := l.Acquire()
	if err != nil {
		t.Fatalf("Acquire after the holder exited: %v", err)
	}
	if !acquired {
		t.Fatal("lock was still held after its owning process exited")
	}
	_ = l.Release()
}
