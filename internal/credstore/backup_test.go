package credstore

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/d0lim/ccswap/internal/keychain"
	"github.com/d0lim/ccswap/internal/paths"
	"github.com/d0lim/ccswap/internal/platform"
	"github.com/d0lim/ccswap/internal/testutil"
)

// fakeKeychain is an in-memory stand-in for security(1), with per-service
// failure injection so both the "answers" and "cannot answer" worlds are
// reachable.
type fakeKeychain struct {
	items map[string]string
	// failGet, failSet and failDelete make the corresponding operation report
	// the Keychain as unavailable.
	failGet, failSet, failDelete bool
	getCalls                     int
	// getCallsByService counts lookups per service, so a test can tell a
	// retry of one item apart from separate probes of different items.
	getCallsByService map[string]int
}

func newFakeKeychain() *fakeKeychain {
	return &fakeKeychain{items: map[string]string{}, getCallsByService: map[string]int{}}
}

func (f *fakeKeychain) Run(_ context.Context, args []string, stdin string) (keychain.Result, error) {
	switch args[0] {
	case "find-generic-password":
		f.getCalls++
		f.getCallsByService[serviceOf(args)]++
		if f.failGet {
			return keychain.Result{ExitCode: 45, Stderr: "locked"}, nil
		}
		value, ok := f.items[fakeKey(args)]
		if !ok {
			return keychain.Result{ExitCode: 44}, nil
		}
		return keychain.Result{ExitCode: 0, Stdout: value + "\n"}, nil

	case "-i", "add-generic-password":
		if f.failSet {
			return keychain.Result{ExitCode: 45, Stderr: "denied"}, nil
		}
		service, account, value := parseAdd(args, stdin)
		f.items[service+"\x00"+account] = value
		return keychain.Result{ExitCode: 0}, nil

	case "delete-generic-password":
		if f.failDelete {
			return keychain.Result{ExitCode: 45, Stderr: "denied"}, nil
		}
		delete(f.items, fakeKey(args))
		return keychain.Result{ExitCode: 0}, nil
	}
	return keychain.Result{ExitCode: 1, Stderr: "unknown command"}, nil
}

// serviceOf extracts the -s value from a find/delete argv.
func serviceOf(args []string) string {
	for i := range args {
		if args[i] == "-s" {
			return args[i+1]
		}
	}
	return ""
}

// fakeKey extracts (service, account) from a find/delete argv.
func fakeKey(args []string) string {
	var account, service string
	for i := range args {
		switch args[i] {
		case "-a":
			account = args[i+1]
		case "-s":
			service = args[i+1]
		}
	}
	return service + "\x00" + account
}

// parseAdd recovers the stored value from either the stdin form or the argv
// fallback, so the fake covers both write paths.
func parseAdd(args []string, stdin string) (service, account, value string) {
	fields := args
	if len(args) == 1 && args[0] == "-i" {
		fields = splitCommand(stdin)
	}
	for i := range fields {
		switch fields[i] {
		case "-a":
			account = unquote(fields[i+1])
		case "-s":
			service = unquote(fields[i+1])
		case "-X":
			value = decodeHex(fields[i+1])
		}
	}
	return service, account, value
}

func splitCommand(s string) []string {
	var out []string
	var cur []rune
	inQuote, escaped := false, false
	for _, r := range s {
		switch {
		case escaped:
			cur = append(cur, r)
			escaped = false
		case r == '\\':
			escaped = true
		case r == '"':
			inQuote = !inQuote
			cur = append(cur, r)
		case (r == ' ' || r == '\n') && !inQuote:
			if len(cur) > 0 {
				out = append(out, string(cur))
				cur = nil
			}
		default:
			cur = append(cur, r)
		}
	}
	if len(cur) > 0 {
		out = append(out, string(cur))
	}
	return out
}

func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

func decodeHex(s string) string {
	out := make([]byte, 0, len(s)/2)
	for i := 0; i+1 < len(s); i += 2 {
		var b byte
		for _, c := range []byte{s[i], s[i+1]} {
			b <<= 4
			switch {
			case c >= '0' && c <= '9':
				b |= c - '0'
			case c >= 'a' && c <= 'f':
				b |= c - 'a' + 10
			case c >= 'A' && c <= 'F':
				b |= c - 'A' + 10
			}
		}
		out = append(out, b)
	}
	return string(out)
}

func newTestStore(t *testing.T, p platform.Platform) (*Store, *fakeKeychain) {
	t.Helper()
	fake := newFakeKeychain()
	r := paths.New(t.TempDir(), p)
	s := New(r, t.TempDir(), keychain.NewWithRunner(fake, 0))
	return s, fake
}

func writeEnc(t *testing.T, s *Store, num, email, value string) {
	t.Helper()
	path := s.backupEncPath(num, email)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte(value))
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------- Reads

func TestReadAccountRoundTripsThroughTheEncFile(t *testing.T) {
	for _, p := range []platform.Platform{platform.MacOS, platform.Linux, platform.Windows} {
		t.Run(p.String(), func(t *testing.T) {
			s, _ := newTestStore(t, p)
			writeEnc(t, s, "1", "a@example.com", "the-credential")

			got, unreadable := s.ReadAccount("1", "a@example.com")
			if got != "the-credential" {
				t.Errorf("value = %q, want the .enc contents", got)
			}
			if unreadable {
				t.Error("a successful read was reported as unreadable")
			}
		})
	}
}

// A fallback .enc — written while the Keychain was unusable — is authoritative
// over a possibly-stale Keychain copy, so a Keychain that recovers cannot
// shadow a newer file.
func TestEncWinsOverTheKeychain(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	fake.items[BackupService+"\x00"+backupUsername("1", "a@example.com")] = "stale-keychain"
	writeEnc(t, s, "1", "a@example.com", "fresh-file")

	got, _ := s.ReadAccount("1", "a@example.com")
	if got != "fresh-file" {
		t.Errorf("value = %q, want the .enc to win", got)
	}
	if fake.getCalls != 0 {
		t.Error("the Keychain was consulted even though a valid .enc was present")
	}
}

func TestReadFallsThroughToTheKeychain(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, s *Store)
	}{
		{"no .enc at all", func(*testing.T, *Store) {}},
		{
			// An empty .enc is not a real backup.
			name: "an empty .enc",
			setup: func(t *testing.T, s *Store) {
				writeEnc(t, s, "1", "a@example.com", "")
			},
		},
		{
			// Corrupt content is a documented recovery path, not a read failure.
			name: "a corrupt .enc",
			setup: func(t *testing.T, s *Store) {
				path := s.backupEncPath("1", "a@example.com")
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("!!!!not base64!!!!"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, fake := newTestStore(t, platform.MacOS)
			fake.items[BackupService+"\x00"+backupUsername("1", "a@example.com")] = "from-keychain"
			tt.setup(t, s)

			got, unreadable := s.ReadAccount("1", "a@example.com")
			if got != "from-keychain" {
				t.Errorf("value = %q, want the Keychain copy", got)
			}
			if unreadable {
				t.Error("a content-level .enc problem was reported as a read failure")
			}
		})
	}
}

// A lenient base64 decoder silently turns "!!!!" into empty bytes, which would
// let a corrupt .enc shadow a valid Keychain copy rather than fall through.
func TestCorruptEncDoesNotShadowTheKeychain(t *testing.T) {
	if _, err := decodeBackup("!!!!"); err == nil {
		t.Error("decodeBackup accepted non-alphabet junk instead of rejecting it")
	}
	if got, err := decodeBackup("   "); err != nil || got != "" {
		t.Errorf("decodeBackup(whitespace) = (%q, %v), want an empty value and no error", got, err)
	}
}

// A genuinely absent backup must not look like a failure, or the user gets told
// to retry something that is working fine.
func TestAbsentBackupIsNotUnreadable(t *testing.T) {
	for _, p := range []platform.Platform{platform.MacOS, platform.Linux} {
		t.Run(p.String(), func(t *testing.T) {
			s, _ := newTestStore(t, p)
			got, unreadable := s.ReadAccount("9", "missing@example.com")
			if got != "" {
				t.Errorf("value = %q, want empty", got)
			}
			if unreadable {
				t.Error("an absent backup was reported as unreadable")
			}
		})
	}
}

// The .enc is the only backend off macOS and wins over the Keychain on macOS, so
// its own read failure must reach the verdict on every platform.
func TestUnreadableEncIsNotAbsent(t *testing.T) {
	for _, p := range []platform.Platform{platform.MacOS, platform.Linux} {
		t.Run(p.String(), func(t *testing.T) {
			s, _ := newTestStore(t, p)
			writeEnc(t, s, "1", "a@example.com", "unreachable")
			testutil.MakeUnreadable(t, s.backupEncPath("1", "a@example.com"))

			got, unreadable := s.ReadAccount("1", "a@example.com")
			if got != "" {
				t.Errorf("value = %q, want empty", got)
			}
			if !unreadable {
				t.Error("an unreadable .enc was reported as a genuinely absent backup")
			}
		})
	}
}

// An unsearchable credentials directory must not be byte-identical to a
// genuinely absent backup.
func TestUnsearchableCredentialsDirIsUnreadable(t *testing.T) {
	s, _ := newTestStore(t, platform.Linux)
	writeEnc(t, s, "1", "a@example.com", "unreachable")
	testutil.MakeUnreadable(t, s.CredentialsDir())

	if _, unreadable := s.ReadAccount("1", "a@example.com"); !unreadable {
		t.Error("an unsearchable credentials directory read as a genuinely absent backup")
	}
}

// A Keychain that cannot answer means an empty read proves nothing — the
// consume gate must not treat it as an empty slot and POST a spent grant.
func TestKeychainFailureMakesTheReadUnreadable(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	fake.failGet = true

	got, unreadable := s.ReadAccount("1", "a@example.com")
	if got != "" {
		t.Errorf("value = %q, want empty", got)
	}
	if !unreadable {
		t.Error("a failed Keychain read was reported as a genuinely absent backup")
	}
}

// Off macOS there is no Keychain to consult at all.
func TestNonMacOSNeverConsultsTheKeychain(t *testing.T) {
	for _, p := range []platform.Platform{platform.Linux, platform.WSL, platform.Windows} {
		t.Run(p.String(), func(t *testing.T) {
			s, fake := newTestStore(t, p)
			fake.items[BackupService+"\x00"+backupUsername("1", "a@example.com")] = "should-not-be-read"

			got, _ := s.ReadAccount("1", "a@example.com")
			if got != "" {
				t.Errorf("value = %q, want empty; the Keychain must not be consulted off macOS", got)
			}
			if fake.getCalls != 0 {
				t.Error("the Keychain was consulted off macOS")
			}
		})
	}
}

// ---------------------------------------------------------------- Writes

func TestWriteAccountRoundTrip(t *testing.T) {
	for _, p := range []platform.Platform{platform.MacOS, platform.Linux, platform.Windows} {
		t.Run(p.String(), func(t *testing.T) {
			s, _ := newTestStore(t, p)
			if err := s.WriteAccount("2", "b@example.com", "stored"); err != nil {
				t.Fatalf("WriteAccount: %v", err)
			}
			got, unreadable := s.ReadAccount("2", "b@example.com")
			if got != "stored" || unreadable {
				t.Errorf("read back (%q, unreadable=%v), want (\"stored\", false)", got, unreadable)
			}
		})
	}
}

func TestNonMacOSWritesTheFile(t *testing.T) {
	s, fake := newTestStore(t, platform.Linux)
	if err := s.WriteAccount("1", "a@example.com", "value"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.backupEncPath("1", "a@example.com")); err != nil {
		t.Errorf(".enc was not written off macOS: %v", err)
	}
	if len(fake.items) != 0 {
		t.Error("the Keychain was written off macOS")
	}
}

// Reads are .enc-wins, so a leftover .enc would shadow the fresh Keychain copy
// forever. Clearing it is correctness-critical, not best-effort.
func TestKeychainWriteReconcilesTheEncAway(t *testing.T) {
	s, _ := newTestStore(t, platform.MacOS)
	writeEnc(t, s, "1", "a@example.com", "stale-fallback")

	if err := s.WriteAccount("1", "a@example.com", "fresh"); err != nil {
		t.Fatalf("WriteAccount: %v", err)
	}
	if _, err := os.Stat(s.backupEncPath("1", "a@example.com")); err == nil {
		t.Error("the stale .enc survived a successful Keychain write")
	}
	if got, _ := s.ReadAccount("1", "a@example.com"); got != "fresh" {
		t.Errorf("read back %q, want the freshly written value", got)
	}
}

// When the Keychain cannot be written the store falls back to a file, and the
// value must still be recoverable.
func TestKeychainWriteFailureFallsBackToTheFile(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	fake.failSet = true

	if err := s.WriteAccount("1", "a@example.com", "fallback-value"); err != nil {
		t.Fatalf("WriteAccount: %v", err)
	}
	if _, err := os.Stat(s.backupEncPath("1", "a@example.com")); err != nil {
		t.Errorf(".enc fallback was not written: %v", err)
	}
	// The failed write also flipped routing, which is what keeps the rest of
	// the invocation on one backend.
	if !s.usesFileBackupBackend() {
		t.Error("routing did not drop to file mode after a failed Keychain write")
	}
}

func TestBackupFileMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not meaningful on Windows")
	}
	s, _ := newTestStore(t, platform.Linux)
	if err := s.WriteAccount("1", "a@example.com", "secret"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.backupEncPath("1", "a@example.com"))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf(".enc mode = %#o, want 0600", got)
	}
}

// ---------------------------------------------------------------- Deletes

// Both backends are cleared regardless of which one this platform writes to: a
// slot may hold an .enc from a period when the Keychain was unusable, and
// leaving either behind would let a removed account come back.
func TestDeleteAccountClearsBothBackends(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	key := BackupService + "\x00" + backupUsername("1", "a@example.com")
	fake.items[key] = "in-keychain"
	writeEnc(t, s, "1", "a@example.com", "in-file")

	if err := s.DeleteAccount("1", "a@example.com"); err != nil {
		t.Fatalf("DeleteAccount: %v", err)
	}
	if _, err := os.Stat(s.backupEncPath("1", "a@example.com")); err == nil {
		t.Error(".enc survived the delete")
	}
	if _, ok := fake.items[key]; ok {
		t.Error("the Keychain item survived the delete")
	}
	if got, unreadable := s.ReadAccount("1", "a@example.com"); got != "" || unreadable {
		t.Errorf("after delete, read = (%q, %v), want a clean miss", got, unreadable)
	}
}

func TestDeleteAccountIsIdempotent(t *testing.T) {
	s, _ := newTestStore(t, platform.MacOS)
	for i := range 2 {
		if err := s.DeleteAccount("1", "a@example.com"); err != nil {
			t.Fatalf("DeleteAccount call %d: %v", i+1, err)
		}
	}
}

// ---------------------------------------------------------------- Naming

// These names are the on-disk and in-Keychain contract with the Python
// implementation, which must keep reading the same slots during the migration.
func TestBackupNaming(t *testing.T) {
	s, _ := newTestStore(t, platform.Linux)
	if got, want := filepath.Base(s.backupEncPath("2", "user@example.com")),
		".creds-2-user@example.com.enc"; got != want {
		t.Errorf("enc filename = %q, want %q", got, want)
	}
	if got, want := backupUsername("2", "user@example.com"), "account-2-user@example.com"; got != want {
		t.Errorf("keychain account name = %q, want %q", got, want)
	}
}

// runnerFunc adapts a function to the keychain.Runner interface, for tests that
// need per-call behaviour rather than the stateful fake above.
type runnerFunc func(context.Context, []string, string) (keychain.Result, error)

func (f runnerFunc) Run(ctx context.Context, args []string, stdin string) (keychain.Result, error) {
	return f(ctx, args, stdin)
}
