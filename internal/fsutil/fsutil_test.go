package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// testPolicy is the default policy with the clock removed and the transient
// predicate supplied by the caller, so the retry loop is exercised on every
// host instead of only on Windows.
func testPolicy(attempts int, transient func(error) bool, slept *[]time.Duration) policy {
	return policy{
		attempts:     attempts,
		initialDelay: defaultInitialDelay,
		maxDelay:     defaultMaxDelay,
		sleep:        func(d time.Duration) { *slept = append(*slept, d) },
		transient:    transient,
	}
}

func alwaysTransient(error) bool { return true }
func neverTransient(error) bool  { return false }

func TestRetriesUntilSuccess(t *testing.T) {
	var slept []time.Duration
	p := testPolicy(defaultAttempts, alwaysTransient, &slept)

	calls := 0
	err := p.do(func() error {
		calls++
		if calls < 4 {
			return syscall.Errno(32)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("do() = %v, want nil once the operation succeeds", err)
	}
	if calls != 4 {
		t.Errorf("operation ran %d times, want 4", calls)
	}
	if len(slept) != 3 {
		t.Errorf("slept %d times, want 3 (one per retry)", len(slept))
	}
}

// The backoff doubles and then holds at the ceiling, so a persistent failure
// costs a bounded amount of wall clock rather than growing without limit.
func TestBackoffDoublesAndCaps(t *testing.T) {
	var slept []time.Duration
	p := testPolicy(defaultAttempts, alwaysTransient, &slept)

	_ = p.do(func() error { return syscall.Errno(5) })

	want := []time.Duration{2, 4, 8, 16, 32, 64, 128, 250, 250}
	if len(slept) != len(want) {
		t.Fatalf("slept %d times, want %d", len(slept), len(want))
	}
	for i, w := range want {
		if got := slept[i]; got != w*time.Millisecond {
			t.Errorf("delay %d = %v, want %v", i, got, w*time.Millisecond)
		}
	}
}

func TestGivesUpAfterAttempts(t *testing.T) {
	var slept []time.Duration
	p := testPolicy(3, alwaysTransient, &slept)

	calls := 0
	sentinel := syscall.Errno(5)
	err := p.do(func() error {
		calls++
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("do() = %v, want the underlying error to surface", err)
	}
	if calls != 3 {
		t.Errorf("operation ran %d times, want the 3-attempt budget", calls)
	}
}

// A genuine failure — a missing source, a cross-device link — must surface
// immediately rather than after a ~0.75s stall.
func TestDoesNotRetryNonTransientErrors(t *testing.T) {
	var slept []time.Duration
	p := testPolicy(defaultAttempts, neverTransient, &slept)

	calls := 0
	err := p.do(func() error {
		calls++
		return syscall.Errno(2) // ERROR_FILE_NOT_FOUND
	})
	if err == nil {
		t.Fatal("do() = nil, want the error to surface")
	}
	if calls != 1 {
		t.Errorf("operation ran %d times, want exactly 1", calls)
	}
	if len(slept) != 0 {
		t.Errorf("slept %d times, want none", len(slept))
	}
}

// A zero budget would skip the operation and report success, which for a
// credential write means silently losing the write.
func TestRejectsNonPositiveAttempts(t *testing.T) {
	for _, attempts := range []int{0, -1} {
		var slept []time.Duration
		p := testPolicy(attempts, alwaysTransient, &slept)
		calls := 0
		err := p.do(func() error {
			calls++
			return nil
		})
		if err == nil {
			t.Errorf("attempts=%d: do() = nil, want an error", attempts)
		}
		if calls != 0 {
			t.Errorf("attempts=%d: operation ran %d times, want 0", attempts, calls)
		}
	}
}

func TestReadTextRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sequence.json")
	if err := os.WriteFile(path, []byte(`{"k": 1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadText(path)
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if got != `{"k": 1}` {
		t.Errorf("ReadText = %q", got)
	}
}

func TestReadTextMissingFile(t *testing.T) {
	_, err := ReadText(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadText on a missing file = %v, want fs.ErrNotExist", err)
	}
}

func TestReplaceFilePublishes(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "tmp.tmp"), filepath.Join(dir, "target.json")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceFile(src, dst); err != nil {
		t.Fatalf("ReplaceFile: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Errorf("target = %q, want %q", got, "payload")
	}
	if _, err := os.Lstat(src); !errors.Is(err, os.ErrNotExist) {
		t.Error("source survived the rename")
	}
}

func TestReplaceFileMissingSource(t *testing.T) {
	dir := t.TempDir()
	err := ReplaceFile(filepath.Join(dir, "absent"), filepath.Join(dir, "target"))
	if err == nil {
		t.Fatal("ReplaceFile on a missing source = nil, want an error")
	}
}

func TestIsTransientContention(t *testing.T) {
	tests := []struct {
		name string
		err  error
		// want is the answer on Windows; off Windows every case is false,
		// because an EACCES there is a genuine, persistent permission problem.
		want bool
	}{
		{"ERROR_ACCESS_DENIED", syscall.Errno(5), true},
		{"ERROR_SHARING_VIOLATION", syscall.Errno(32), true},
		{"ERROR_LOCK_VIOLATION", syscall.Errno(33), true},
		{"ERROR_FILE_NOT_FOUND", syscall.Errno(2), false},
		{"not an errno", errors.New("plain"), false},
		{"nil", nil, false},
		// The predicate has to see through wrapping, since callers get errors
		// out of os.Rename as *os.LinkError.
		{"wrapped sharing violation", &os.LinkError{Err: syscall.Errno(32)}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.want && runtime.GOOS == "windows"
			if got := isTransientContention(tt.err); got != want {
				t.Errorf("isTransientContention(%v) = %v, want %v on %s", tt.err, got, want, runtime.GOOS)
			}
		})
	}
}
