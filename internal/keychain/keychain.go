// Package keychain stores secrets in the macOS Keychain through the system
// security(1) CLI.
//
// # Why shell out instead of calling Security.framework
//
// Keychain items are created and read by the same stable /usr/bin/security
// binary, so reads stay silent across upgrades. An in-process Security.framework
// call — which is what a keyring library does — anchors the item's access
// control to the *calling binary*. Every `aaswap upgrade` would then produce a
// new binary that macOS does not recognize as the item's creator, and the user
// gets a "aaswap wants to use your keychain" prompt. security(1) never
// changes, so creator == reader and there is no prompt.
//
// The read/write/delete shapes mirror Claude Code's own implementation
// (utils/secureStorage/macOsKeychainStorage.ts), because both tools read and
// write the same items.
//
// # Caveat: printable text only
//
// find-generic-password -w prints stored data raw only when it is printable;
// data with non-printable bytes comes back hex-encoded, so a write/read round
// trip would not be the identity. That is fine here — credentials are ASCII
// JSON — but do not reuse this package for arbitrary binary data. Claude Code's
// own -w reads share the constraint.
//
// Every function is safe to call on any platform: nothing is executed at import
// time, and the operations are only meaningful on macOS.
package keychain

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// SecurityBinary is pinned to Apple's system binary rather than resolved
// through PATH. This is a credential tool: an attacker-controlled `security`
// earlier on PATH must not be able to intercept secrets. The path exists on
// every macOS.
const SecurityBinary = "/usr/bin/security"

// notFoundExit is errSecItemNotFound as surfaced by find- and
// delete-generic-password. It means "no such item", which is an answer, not a
// failure — telling it apart from a locked or denied Keychain is what lets the
// caller distinguish "this account has no stored credential" from "the Keychain
// is unusable, fall back to file storage".
const notFoundExit = 44

// stdinLineLimit is the largest command `security -i` will read intact.
//
// security -i reads stdin with a 4096-byte fgets() buffer (BUFSIZ on darwin). A
// longer command line is truncated mid-argument: the write fails while leaving
// any previous entry intact (Claude Code #30337). The 64 bytes of headroom
// guard against line-terminator accounting differences.
const stdinLineLimit = 4096 - 64

// DefaultTimeout bounds every security spawn so a wedged Keychain — a locked
// login keychain prompting for an unlock that never comes on a headless or SSH
// host — cannot hang the CLI.
//
// Deliberately short: a credential operation that has to fall back to the file
// may be followed by a best-effort cleanup spawn, so the per-operation budget
// doubles in the worst case. A healthy Keychain answers in well under 100ms.
const DefaultTimeout = 5 * time.Second

// ErrUnavailable marks every failure that means "the Keychain could not answer"
// — a non-zero exit that is not "not found", a timeout, or a missing binary.
//
// Callers treat a match as "Keychain unusable, use file storage" rather than as
// a defect. Matching on this sentinel rather than on any error at all is what
// keeps a genuine bug loud instead of silently routing to the file backend
// mid-invocation.
var ErrUnavailable = errors.New("keychain unavailable")

// Result is one security(1) invocation's outcome.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

// Runner executes security(1). It exists so tests can drive every branch — and
// so the real binary is unreachable from a test binary; see exec.go.
type Runner interface {
	Run(ctx context.Context, args []string, stdin string) (Result, error)
}

// Keychain is a handle on the system Keychain.
type Keychain struct {
	runner  Runner
	timeout time.Duration
}

// New returns a Keychain backed by the system security(1) binary.
func New() *Keychain {
	return &Keychain{runner: execRunner{}, timeout: DefaultTimeout}
}

// NewWithRunner returns a Keychain driven by a caller-supplied runner.
func NewWithRunner(r Runner, timeout time.Duration) *Keychain {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &Keychain{runner: r, timeout: timeout}
}

func (k *Keychain) run(args []string, stdin string) (Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), k.timeout)
	defer cancel()

	res, err := k.runner.Run(ctx, args, stdin)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, fmt.Errorf("security %s timed out after %s: %w",
				args[0], k.timeout, ErrUnavailable)
		}
		return res, fmt.Errorf("security %s: %w: %w", args[0], ErrUnavailable, err)
	}
	return res, nil
}

// Get returns the stored password and whether an item exists.
//
// A missing item is (", false, nil") — an answer, not an error. Any other
// non-zero exit or a timeout returns an error wrapping [ErrUnavailable], so a
// genuine miss is never confused with a transient failure.
func (k *Keychain) Get(service, account string) (string, bool, error) {
	res, err := k.run([]string{"find-generic-password", "-a", account, "-w", "-s", service}, "")
	if err != nil {
		return "", false, err
	}
	switch res.ExitCode {
	case 0:
		// -w prints the value followed by exactly one newline; strip that one
		// so values with meaningful trailing whitespace survive intact.
		return strings.TrimSuffix(res.Stdout, "\n"), true, nil
	case notFoundExit:
		return "", false, nil
	default:
		return "", false, fmt.Errorf(
			"security find-generic-password failed (rc=%d): %s: %w",
			res.ExitCode, strings.TrimSpace(res.Stderr), ErrUnavailable)
	}
}

// Exists reports whether an item is present, without touching its secret.
//
// This is an attribute-only lookup (no -w), so nothing is decrypted and it can
// never trigger a Keychain prompt, even for items owned by another app.
//
// Deliberately non-raising: "not found", an error exit, a timeout and a missing
// binary all report false. Callers use it for cleanup verification, not access
// decisions, so its answer must never feed the capability cache — a timeout
// here means "couldn't tell", not "the Keychain works".
func (k *Keychain) Exists(service, account string) bool {
	res, err := k.run([]string{"find-generic-password", "-a", account, "-s", service}, "")
	return err == nil && res.ExitCode == 0
}

// Set creates or updates a generic-password item.
//
// The value is hex-encoded (-X), which sidesteps escaping entirely, and the
// command is piped through `security -i` so the secret never appears in the
// process argv, where a process monitor or endpoint agent would capture it.
func (k *Keychain) Set(service, account, password string) error {
	hexValue := hex.EncodeToString([]byte(password))
	command := fmt.Sprintf("add-generic-password -U -a %s -s %s -X %s\n",
		quoteForStdin(account), quoteForStdin(service), hexValue)

	var res Result
	var err error
	if len(command) <= stdinLineLimit {
		res, err = k.run([]string{"-i"}, command)
	} else {
		// Overflows the stdin line buffer, which would truncate mid-argument
		// and silently corrupt the entry. Fall back to argv: hex there is
		// recoverable by a determined observer but defeats naive
		// plaintext-grep rules, and silent corruption is strictly worse.
		res, err = k.run([]string{
			"add-generic-password", "-U",
			"-a", account, "-s", service, "-X", hexValue,
		}, "")
	}
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("security add-generic-password failed (rc=%d): %s: %w",
			res.ExitCode, strings.TrimSpace(res.Stderr), ErrUnavailable)
	}
	return nil
}

// Delete removes a generic-password item. An item that is already absent
// (rc 44) counts as success, so deletion is idempotent.
func (k *Keychain) Delete(service, account string) error {
	res, err := k.run([]string{"delete-generic-password", "-a", account, "-s", service}, "")
	if err != nil {
		return err
	}
	if res.ExitCode == 0 || res.ExitCode == notFoundExit {
		return nil
	}
	return fmt.Errorf("security delete-generic-password failed (rc=%d): %s: %w",
		res.ExitCode, strings.TrimSpace(res.Stderr), ErrUnavailable)
}

// quoteForStdin quotes a value for a `security -i` command line.
//
// security -i re-parses each line shell-style, so the value is wrapped in
// double quotes with embedded backslashes and quotes escaped. The
// active-credential service name contains a space, which is what makes this
// necessary rather than merely defensive.
func quoteForStdin(value string) string {
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// AccountName returns the account name for the active-credential Keychain item,
// mirroring Claude Code's getUsername()
// (utils/secureStorage/macOsKeychainHelpers.ts): $USER first, then the OS
// username, then a stable final fallback.
//
// Matching this exactly matters on headless, launchd and cron hosts where $USER
// is unset: a divergent default would key a *different* Keychain item than
// Claude Code, and the two could not see each other's active credential.
func AccountName() string {
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	// Look up by effective uid, matching the original's pwd.getpwuid(geteuid()).
	if u, err := user.LookupId(strconv.Itoa(os.Geteuid())); err == nil && u.Username != "" {
		return u.Username
	}
	return "claude-code-user"
}
