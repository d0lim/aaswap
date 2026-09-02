package keychain

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// call records one security(1) invocation as the fake runner saw it.
type call struct {
	args  []string
	stdin string
}

// fakeRunner stands in for security(1). Every branch of the wrapper is reachable
// through it, and no test can reach the developer's real Keychain.
type fakeRunner struct {
	calls  []call
	result Result
	err    error
}

func (f *fakeRunner) Run(_ context.Context, args []string, stdin string) (Result, error) {
	f.calls = append(f.calls, call{args: slices.Clone(args), stdin: stdin})
	return f.result, f.err
}

func (f *fakeRunner) lastArgs() []string {
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1].args
}

func withRunner(t *testing.T, res Result, err error) (*Keychain, *fakeRunner) {
	t.Helper()
	r := &fakeRunner{result: res, err: err}
	return NewWithRunner(r, DefaultTimeout), r
}

// ---------------------------------------------------------------- Get

func TestGet(t *testing.T) {
	tests := []struct {
		name      string
		result    Result
		wantValue string
		wantFound bool
		wantErr   bool
	}{
		{
			name:      "rc 0 returns the value",
			result:    Result{ExitCode: 0, Stdout: "secret-value\n"},
			wantValue: "secret-value", wantFound: true,
		},
		{
			// -w prints exactly one trailing newline; only that one is
			// stripped, so a value whose own trailing whitespace matters
			// survives intact.
			name:      "only one trailing newline is stripped",
			result:    Result{ExitCode: 0, Stdout: "value with trailing space  \n"},
			wantValue: "value with trailing space  ", wantFound: true,
		},
		{
			name:      "an empty value round-trips",
			result:    Result{ExitCode: 0, Stdout: "\n"},
			wantValue: "", wantFound: true,
		},
		{
			// The one code that means "no such item" rather than "the Keychain
			// could not answer".
			name:   "rc 44 is a miss, not a failure",
			result: Result{ExitCode: notFoundExit},
		},
		{
			name:    "any other non-zero exit is a failure",
			result:  Result{ExitCode: 45, Stderr: "keychain locked"},
			wantErr: true,
		},
		{
			name:    "rc 1 is a failure, not a miss",
			result:  Result{ExitCode: 1},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, _ := withRunner(t, tt.result, nil)
			value, found, err := k.Get("svc", "acct")

			if tt.wantErr {
				if !errors.Is(err, ErrUnavailable) {
					t.Fatalf("err = %v, want it to wrap ErrUnavailable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if found != tt.wantFound {
				t.Errorf("found = %v, want %v", found, tt.wantFound)
			}
			if value != tt.wantValue {
				t.Errorf("value = %q, want %q", value, tt.wantValue)
			}
		})
	}
}

func TestGetArgv(t *testing.T) {
	k, r := withRunner(t, Result{ExitCode: notFoundExit}, nil)
	if _, _, err := k.Get("the service", "the account"); err != nil {
		t.Fatal(err)
	}
	want := []string{"find-generic-password", "-a", "the account", "-w", "-s", "the service"}
	if got := r.lastArgs(); !slices.Equal(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------- Exists

func TestExists(t *testing.T) {
	tests := []struct {
		name   string
		result Result
		err    error
		want   bool
	}{
		{"rc 0 is present", Result{ExitCode: 0}, nil, true},
		{"rc 44 is absent", Result{ExitCode: notFoundExit}, nil, false},
		// Non-raising by design: callers use Exists for cleanup verification,
		// so a failure must read as "couldn't tell", never feed a capability
		// decision.
		{"an error exit is not present", Result{ExitCode: 45}, nil, false},
		{"a timeout is not present", Result{}, context.DeadlineExceeded, false},
		{"a missing binary is not present", Result{}, os.ErrNotExist, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, _ := withRunner(t, tt.result, tt.err)
			if got := k.Exists("svc", "acct"); got != tt.want {
				t.Errorf("Exists = %v, want %v", got, tt.want)
			}
		})
	}
}

// An attribute-only lookup decrypts nothing, so it can never raise a Keychain
// prompt — even for an item another app owns. Passing -w here would.
func TestExistsNeverRequestsTheSecret(t *testing.T) {
	k, r := withRunner(t, Result{ExitCode: 0}, nil)
	k.Exists("svc", "acct")

	args := r.lastArgs()
	if slices.Contains(args, "-w") {
		t.Errorf("argv %v contains -w; Exists must not request the secret", args)
	}
	want := []string{"find-generic-password", "-a", "acct", "-s", "svc"}
	if !slices.Equal(args, want) {
		t.Errorf("argv = %v, want %v", args, want)
	}
}

// ---------------------------------------------------------------- Set

// The secret must not appear in argv, where a process monitor or endpoint agent
// would capture it. It goes through `security -i` on stdin instead.
func TestSetUsesStdinForNormalPayloads(t *testing.T) {
	k, r := withRunner(t, Result{ExitCode: 0}, nil)
	const secret = `{"accessToken":"sk-ant-oat01-abc"}`
	if err := k.Set("Claude Code-credentials", "alice", secret); err != nil {
		t.Fatalf("Set: %v", err)
	}

	c := r.calls[len(r.calls)-1]
	if !slices.Equal(c.args, []string{"-i"}) {
		t.Errorf("argv = %v, want [-i] so the secret stays off the command line", c.args)
	}
	for _, arg := range c.args {
		if strings.Contains(arg, secret) || strings.Contains(arg, hex.EncodeToString([]byte(secret))) {
			t.Errorf("argv %v carries the secret", c.args)
		}
	}
	if !strings.HasPrefix(c.stdin, "add-generic-password -U ") {
		t.Errorf("stdin = %q, want an add-generic-password command", c.stdin)
	}
	if !strings.HasSuffix(c.stdin, "\n") {
		t.Error("stdin command is not newline-terminated; security -i reads by line")
	}
	// -X takes the value as hex, which sidesteps escaping entirely.
	if !strings.Contains(c.stdin, "-X "+hex.EncodeToString([]byte(secret))) {
		t.Errorf("stdin = %q, want the hex-encoded value after -X", c.stdin)
	}
	// The service name contains a space, which is why quoting is required
	// rather than merely defensive.
	if !strings.Contains(c.stdin, `-s "Claude Code-credentials"`) {
		t.Errorf("stdin = %q, want the service name quoted", c.stdin)
	}
}

// A command longer than security -i's 4096-byte fgets buffer is truncated
// mid-argument: the write fails while leaving the previous entry intact
// (Claude Code #30337). Falling back to argv is worse for secrecy but far
// better than silent corruption.
func TestSetFallsBackToArgvForOversizedPayloads(t *testing.T) {
	k, r := withRunner(t, Result{ExitCode: 0}, nil)
	big := strings.Repeat("x", stdinLineLimit) // hex doubles the length

	if err := k.Set("svc", "acct", big); err != nil {
		t.Fatalf("Set: %v", err)
	}

	c := r.calls[len(r.calls)-1]
	if c.stdin != "" {
		t.Error("oversized payload was still sent through stdin")
	}
	if got := c.args[:2]; !slices.Equal(got, []string{"add-generic-password", "-U"}) {
		t.Errorf("argv starts with %v, want the add-generic-password argv form", got)
	}
	// Passed as raw list elements: no shell, so no quoting.
	if !slices.Contains(c.args, hex.EncodeToString([]byte(big))) {
		t.Error("argv does not carry the hex value")
	}
	if !slices.Contains(c.args, "acct") || !slices.Contains(c.args, "svc") {
		t.Errorf("argv = %v, want the account and service as plain elements", c.args)
	}
}

// The boundary itself: a command exactly at the limit still goes through stdin.
func TestSetStdinBoundary(t *testing.T) {
	// Work back from the limit to a payload whose full command is exactly
	// stdinLineLimit bytes.
	overhead := len(`add-generic-password -U -a "acct" -s "svc" -X ` + "\n")
	payload := strings.Repeat("a", (stdinLineLimit-overhead)/2)

	k, r := withRunner(t, Result{ExitCode: 0}, nil)
	if err := k.Set("svc", "acct", payload); err != nil {
		t.Fatal(err)
	}
	if got := r.calls[0].stdin; got == "" {
		t.Error("a command at the size limit was pushed to argv; the limit is inclusive")
	} else if len(got) > stdinLineLimit {
		t.Errorf("stdin command is %d bytes, over the %d limit", len(got), stdinLineLimit)
	}
}

func TestSetReportsFailure(t *testing.T) {
	k, _ := withRunner(t, Result{ExitCode: 45, Stderr: "nope"}, nil)
	if err := k.Set("svc", "acct", "secret"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Set = %v, want it to wrap ErrUnavailable", err)
	}
}

// ---------------------------------------------------------------- Delete

func TestDelete(t *testing.T) {
	tests := []struct {
		name    string
		result  Result
		wantErr bool
	}{
		{"rc 0 succeeds", Result{ExitCode: 0}, false},
		// Deletion is idempotent: an item that is already gone is the outcome
		// the caller wanted.
		{"rc 44 succeeds", Result{ExitCode: notFoundExit}, false},
		{"any other exit fails", Result{ExitCode: 45, Stderr: "denied"}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, _ := withRunner(t, tt.result, nil)
			err := k.Delete("svc", "acct")
			if tt.wantErr != (err != nil) {
				t.Fatalf("Delete = %v, wantErr = %v", err, tt.wantErr)
			}
			if tt.wantErr && !errors.Is(err, ErrUnavailable) {
				t.Errorf("err = %v, want it to wrap ErrUnavailable", err)
			}
		})
	}
}

func TestDeleteArgv(t *testing.T) {
	k, r := withRunner(t, Result{ExitCode: 0}, nil)
	if err := k.Delete("svc", "acct"); err != nil {
		t.Fatal(err)
	}
	want := []string{"delete-generic-password", "-a", "acct", "-s", "svc"}
	if got := r.lastArgs(); !slices.Equal(got, want) {
		t.Errorf("argv = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------- Timeouts

// A wedged Keychain — a locked login keychain prompting for an unlock that
// never comes on a headless host — must not hang the CLI.
func TestTimeoutBecomesUnavailable(t *testing.T) {
	// A runner that outlives its context, the way a hung security(1) would.
	slow := runnerFunc(func(ctx context.Context, _ []string, _ string) (Result, error) {
		<-ctx.Done()
		return Result{}, ctx.Err()
	})
	k := NewWithRunner(slow, 10*time.Millisecond)

	if _, _, err := k.Get("svc", "acct"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Get on a timeout = %v, want it to wrap ErrUnavailable", err)
	}
	if err := k.Set("svc", "acct", "x"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Set on a timeout = %v, want it to wrap ErrUnavailable", err)
	}
	if err := k.Delete("svc", "acct"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Delete on a timeout = %v, want it to wrap ErrUnavailable", err)
	}
	if k.Exists("svc", "acct") {
		t.Error("Exists on a timeout = true, want false")
	}
}

func TestDefaultTimeoutIsBounded(t *testing.T) {
	// Short enough that a stuck Keychain cannot hang the CLI, and the budget
	// doubles when a fallback is followed by a cleanup spawn.
	if DefaultTimeout != 5*time.Second {
		t.Errorf("DefaultTimeout = %v, want 5s", DefaultTimeout)
	}
	if got := NewWithRunner(&fakeRunner{}, 0).timeout; got != DefaultTimeout {
		t.Errorf("a zero timeout became %v, want the %v default", got, DefaultTimeout)
	}
}

// A missing binary is "the Keychain cannot answer", not a defect: off macOS
// there is no security(1) at all.
func TestMissingBinaryIsUnavailable(t *testing.T) {
	k, _ := withRunner(t, Result{}, os.ErrNotExist)
	if _, _, err := k.Get("svc", "acct"); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Get = %v, want it to wrap ErrUnavailable", err)
	}
}

// ---------------------------------------------------------------- Contract

// The binary is pinned by absolute path: this is a credential tool, and an
// attacker-controlled `security` earlier on PATH must not intercept secrets.
func TestSecurityBinaryIsAbsolute(t *testing.T) {
	if SecurityBinary != "/usr/bin/security" {
		t.Errorf("SecurityBinary = %q, want Apple's system binary by absolute path", SecurityBinary)
	}
}

func TestNotFoundExitIsFortyFour(t *testing.T) {
	// errSecItemNotFound. Claude Code relies on the same code, and confusing it
	// with a failure would make a missing credential look like a broken
	// Keychain.
	if notFoundExit != 44 {
		t.Errorf("notFoundExit = %d, want 44", notFoundExit)
	}
}

func TestQuoteForStdin(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"alice", `"alice"`},
		{"Claude Code-credentials", `"Claude Code-credentials"`},
		{`with"quote`, `"with\"quote"`},
		{`with\backslash`, `"with\\backslash"`},
		// Backslashes are escaped before quotes, so an escaped quote does not
		// get double-processed.
		{`both\"`, `"both\\\""`},
		{"", `""`},
	}
	for _, tt := range tests {
		if got := quoteForStdin(tt.in); got != tt.want {
			t.Errorf("quoteForStdin(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------- AccountName

func TestAccountName(t *testing.T) {
	t.Run("prefers $USER", func(t *testing.T) {
		t.Setenv("USER", "alice")
		if got := AccountName(); got != "alice" {
			t.Errorf("AccountName = %q, want alice", got)
		}
	})

	// The old default was the bare string "user", which mismatches Claude
	// Code's OS-username on headless hosts where $USER is unset — the two would
	// then key different Keychain items and could not see each other's active
	// credential.
	t.Run("without $USER it never falls back to \"user\"", func(t *testing.T) {
		t.Setenv("USER", "")
		got := AccountName()
		if got == "" {
			t.Fatal("AccountName is empty")
		}
		if got == "user" {
			t.Error(`AccountName = "user", the legacy default that mismatches Claude Code`)
		}
	})
}

// ---------------------------------------------------------------- Safety net

// The Keychain half of the safety net: aaswap's items are live Claude Code
// logins, so a test must not be able to reach the real binary by accident.
func TestRealBinaryIsUnreachableFromTests(t *testing.T) {
	defer func() {
		got := recover()
		if got == nil {
			t.Fatal("a test was allowed to execute the real security(1) binary")
		}
		if msg, ok := got.(string); !ok || !strings.Contains(msg, AllowRealKeychainEnv) {
			t.Errorf("panic %v does not explain the opt-in", got)
		}
	}()
	guardRealKeychain()
}

func TestGuardYieldsToAnExplicitOptIn(t *testing.T) {
	t.Setenv(AllowRealKeychainEnv, "1")
	defer func() {
		if got := recover(); got != nil {
			t.Fatalf("the guard fired despite an explicit opt-in: %v", got)
		}
	}()
	guardRealKeychain()
}

// runnerFunc adapts a function to the Runner interface.
type runnerFunc func(context.Context, []string, string) (Result, error)

func (f runnerFunc) Run(ctx context.Context, args []string, stdin string) (Result, error) {
	return f(ctx, args, stdin)
}
