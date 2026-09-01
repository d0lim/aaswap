package credstore

import (
	"context"
	json "encoding/json/v2"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/realiti4/claude-swap/internal/keychain"
	"github.com/realiti4/claude-swap/internal/platform"
	"github.com/realiti4/claude-swap/internal/testutil"
)

func init() {
	// Collapse the transient-contention backoff; the retry *shape* is what the
	// tests check, not the wall clock it costs.
	activeReadRetryDelay = time.Millisecond
}

// seedOAuthKeychain puts a credential in the item the active profile reads.
func seedOAuthKeychain(t *testing.T, s *Store, fake *fakeKeychain, value string) {
	t.Helper()
	services := ActiveOAuthKeychainServices(s.paths)
	fake.items[services[0]+"\x00"+keychain.AccountName()] = value
}

func writeCredentialsFile(t *testing.T, s *Store, content string) {
	t.Helper()
	path := s.paths.CredentialsPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func writeGlobalConfig(t *testing.T, s *Store, cfg map[string]any) {
	t.Helper()
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.paths.GlobalConfigPath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

const oauthJSON = `{"claudeAiOauth":{"accessToken":"tok"}}`

// ---------------------------------------------------------------- Happy paths

func TestReadActiveFromTheKeychain(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	seedOAuthKeychain(t, s, fake, oauthJSON)

	got := s.ReadActive()
	if got.Value != oauthJSON {
		t.Errorf("Value = %q, want the Keychain credential", got.Value)
	}
	if got.Degraded || got.KeychainUnavailable || got.FileReadFailed {
		t.Errorf("a clean Keychain read reported problems: %+v", got)
	}
}

func TestReadActiveFromTheFile(t *testing.T) {
	for _, p := range []platform.Platform{platform.Linux, platform.WSL, platform.Windows} {
		t.Run(p.String(), func(t *testing.T) {
			s, _ := newTestStore(t, p)
			writeCredentialsFile(t, s, oauthJSON)

			got := s.ReadActive()
			if got.Value != oauthJSON {
				t.Errorf("Value = %q, want the file's contents", got.Value)
			}
			if got.Degraded {
				t.Error("a plain file read off macOS was marked degraded")
			}
		})
	}
}

// Nothing stored anywhere is a clean, unremarkable answer — not a failure.
func TestReadActiveFindsNothing(t *testing.T) {
	s, _ := newTestStore(t, platform.MacOS)

	got := s.ReadActive()
	if got.Value != "" {
		t.Errorf("Value = %q, want empty", got.Value)
	}
	if got.KeychainUnavailable || got.Degraded || got.FileReadFailed {
		t.Errorf("an empty store reported problems: %+v", got)
	}
}

// ---------------------------------------------------------------- Degradation

// The file may hold a stale generation once the Keychain read has failed, since
// Claude Code writes rotations Keychain-only on macOS. The credential is still
// served — but marked, so consume paths refuse to spend its refresh token.
func TestAFailedKeychainReadCoveredByTheFileIsDegraded(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	fake.failGet = true
	writeCredentialsFile(t, s, oauthJSON)

	got := s.ReadActive()
	if got.Value != oauthJSON {
		t.Errorf("Value = %q, want the file to cover the failed Keychain read", got.Value)
	}
	if !got.Degraded {
		t.Error("Degraded = false; the served bytes may be a superseded generation")
	}
	// Covered, so the slot is not "unavailable" — there is something to serve.
	if got.KeychainUnavailable {
		t.Error("KeychainUnavailable = true even though the file covered the read")
	}
}

// A merely unreadable slot must not render as "no credentials", which would
// nudge the user into a re-login that cannot help.
func TestAnUncoveredKeychainFailureIsUnavailable(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	fake.failGet = true

	got := s.ReadActive()
	if got.Value != "" {
		t.Errorf("Value = %q, want empty", got.Value)
	}
	if !got.KeychainUnavailable {
		t.Error("KeychainUnavailable = false for a failed, uncovered Keychain read")
	}
	if !got.Degraded {
		t.Error("Degraded = false for a failed Keychain read")
	}
}

// A denied Keychain AND an unreadable fallback file is the most unreadable
// state there is; reporting it as "no credentials" sent users to a re-login.
func TestADeniedKeychainAndUnreadableFileStayUnavailable(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	fake.failGet = true
	writeCredentialsFile(t, s, oauthJSON)
	testutil.MakeUnreadable(t, s.paths.CredentialsPath())

	got := s.ReadActive()
	if !got.FileReadFailed {
		t.Error("FileReadFailed = false for an unreadable credentials file")
	}
	if !got.KeychainUnavailable {
		t.Error("KeychainUnavailable was forced false; this is the most unreadable state there is")
	}
}

// ---------------------------------------------------------------- Retry

// A locked or contended login Keychain can fail one call transiently — just
// after wake, or under contention with Claude Code's statusline polling the same
// item — and a second attempt usually succeeds.
func TestATransientKeychainFailureIsRetried(t *testing.T) {
	s, _ := newTestStore(t, platform.MacOS)
	calls := 0
	flaky := runnerFunc(func(_ context.Context, args []string, _ string) (keychain.Result, error) {
		if args[0] != "find-generic-password" {
			return keychain.Result{ExitCode: 0}, nil
		}
		calls++
		if calls == 1 {
			return keychain.Result{ExitCode: 45, Stderr: "locked"}, nil
		}
		return keychain.Result{ExitCode: 0, Stdout: oauthJSON + "\n"}, nil
	})
	s.kc = keychain.NewWithRunner(flaky, 0)

	got := s.ReadActive()
	if got.Value != oauthJSON {
		t.Errorf("Value = %q, want the retry to have succeeded", got.Value)
	}
	if got.Degraded {
		t.Error("Degraded = true even though the retry succeeded")
	}
	if calls != 2 {
		t.Errorf("made %d attempts, want exactly 2", calls)
	}
}

// A genuinely absent item is an answer, not a transient failure, so retrying it
// would only cost latency on every empty slot.
func TestAnAbsentItemIsNotRetried(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)

	s.ReadActive()
	// Counted per service: a full ReadActive of an empty store legitimately
	// probes two different items — the OAuth credential and the managed key.
	// What must not happen is the same item being asked twice.
	if got := fake.getCallsByService[ClaudeOAuthService]; got != 1 {
		t.Errorf("the absent OAuth item was probed %d times, want 1", got)
	}
	if got := fake.getCallsByService[ClaudeManagedKeyService]; got != 1 {
		t.Errorf("the absent managed-key item was probed %d times, want 1", got)
	}
}

// ---------------------------------------------------------------- OAuth first

// Trying OAuth fully first is what stops a macOS OAuth login whose Keychain item
// is missing, but whose file fallback exists, from being misread as an API key.
func TestOAuthIsTriedFullyBeforeTheManagedKey(t *testing.T) {
	s, _ := newTestStore(t, platform.MacOS)
	writeCredentialsFile(t, s, oauthJSON)
	writeGlobalConfig(t, s, map[string]any{"primaryApiKey": "sk-ant-api03-should-not-win"})

	got := s.ReadActive()
	if got.Value != oauthJSON {
		t.Errorf("Value = %q, want the OAuth file to win over the managed key", got.Value)
	}
	if LooksLikeAPIKey(got.Value) {
		t.Error("an OAuth login was misread as an API key")
	}
}

func TestReadsTheManagedKeyWhenThereIsNoOAuth(t *testing.T) {
	s, _ := newTestStore(t, platform.Linux)
	writeGlobalConfig(t, s, map[string]any{"primaryApiKey": "sk-ant-api03-the-key"})

	got := s.ReadActive()
	if got.Value != "sk-ant-api03-the-key" {
		t.Errorf("Value = %q, want the managed key", got.Value)
	}
	if !LooksLikeAPIKey(got.Value) {
		t.Error("the managed key was not recognized as one")
	}
}

// The managed-key Keychain item is default-profile-only: Claude Code's service
// name for it under a custom profile is not pinned anywhere, and guessing is the
// thing the OAuth half can avoid and this half cannot.
func TestTheManagedKeyKeychainIsDefaultProfileOnly(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	s.paths.ConfigDir = t.TempDir()
	fake.items[ClaudeManagedKeyService+"\x00"+keychain.AccountName()] = "sk-ant-api03-other-profile"
	writeGlobalConfig(t, s, map[string]any{"primaryApiKey": "sk-ant-api03-own-profile"})

	got := s.ReadActive()
	if got.Value != "sk-ant-api03-own-profile" {
		t.Errorf("Value = %q, want this profile's own primaryApiKey", got.Value)
	}
}

// ---------------------------------------------------------------- Profile scoping

// Reading the fixed service name from a custom profile would return a
// credential belonging to a different account, while the identity read one
// layer up reports the custom profile's — a silent mispairing.
func TestACustomProfileReadsItsOwnItem(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	custom := t.TempDir()
	s.paths.ConfigDir = custom

	fake.items[ClaudeOAuthService+"\x00"+keychain.AccountName()] = `{"claudeAiOauth":{"accessToken":"DEFAULT-PROFILE"}}`
	fake.items[KeychainServiceName(custom)+"\x00"+keychain.AccountName()] = `{"claudeAiOauth":{"accessToken":"CUSTOM-PROFILE"}}`

	got := s.ReadActive()
	if got.Value != `{"claudeAiOauth":{"accessToken":"CUSTOM-PROFILE"}}` {
		t.Errorf("Value = %q, want the custom profile's own item", got.Value)
	}
}

// Skipping the Keychain under a custom profile would leave macOS mostly blind:
// Claude Code writes rotations Keychain-only there, so the plaintext file often
// does not exist and a logged-in profile would render as "no credentials".
func TestACustomProfileStillReadsTheKeychain(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	custom := t.TempDir()
	s.paths.ConfigDir = custom
	fake.items[KeychainServiceName(custom)+"\x00"+keychain.AccountName()] = oauthJSON

	if got := s.ReadActive(); got.Value != oauthJSON {
		t.Errorf("Value = %q, want the custom profile's Keychain credential", got.Value)
	}
}

// ---------------------------------------------------------------- Global config

func TestReadGlobalConfig(t *testing.T) {
	t.Run("absent is a clean miss", func(t *testing.T) {
		s, _ := newTestStore(t, platform.Linux)
		if _, ok := s.readGlobalConfig(); ok {
			t.Error("readGlobalConfig reported success with no file present")
		}
	})

	t.Run("parses an object", func(t *testing.T) {
		s, _ := newTestStore(t, platform.Linux)
		writeGlobalConfig(t, s, map[string]any{"oauthAccount": map[string]any{"emailAddress": "a@example.com"}})
		cfg, ok := s.readGlobalConfig()
		if !ok {
			t.Fatal("readGlobalConfig reported failure for a valid object")
		}
		if _, present := cfg["oauthAccount"]; !present {
			t.Errorf("cfg = %v, want oauthAccount preserved", cfg)
		}
	})

	t.Run("a non-object is a miss", func(t *testing.T) {
		s, _ := newTestStore(t, platform.Linux)
		if err := os.WriteFile(s.paths.GlobalConfigPath(), []byte(`["a"]`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, ok := s.readGlobalConfig(); ok {
			t.Error("readGlobalConfig accepted a JSON array")
		}
	})
}

// ---------------------------------------------------------------- Keychain cleanup

// Claude Code reads the Keychain before the plaintext file, so a stale item
// would resurrect the old account (#30337). The return value is what
// pinFileMode needs: proof that nothing can shadow the file.
func TestDeleteActiveKeychainEntry(t *testing.T) {
	t.Run("reports success when the item is gone", func(t *testing.T) {
		s, fake := newTestStore(t, platform.MacOS)
		fake.items[ClaudeOAuthService+"\x00"+keychain.AccountName()] = oauthJSON

		if !s.deleteActiveKeychainEntry() {
			t.Error("deleteActiveKeychainEntry = false for a successful delete")
		}
		if _, ok := fake.items[ClaudeOAuthService+"\x00"+keychain.AccountName()]; ok {
			t.Error("the item survived the delete")
		}
	})

	t.Run("an already-absent item still counts as cleared", func(t *testing.T) {
		s, _ := newTestStore(t, platform.MacOS)
		if !s.deleteActiveKeychainEntry() {
			t.Error("deleteActiveKeychainEntry = false for an already-absent item")
		}
	})

	// Best-effort: a down Keychain cannot be cleaned now, and that is the
	// documented recovery residual the pin has to record.
	t.Run("a failed delete reports that a residual may remain", func(t *testing.T) {
		s, fake := newTestStore(t, platform.MacOS)
		fake.failDelete = true
		if s.deleteActiveKeychainEntry() {
			t.Error("deleteActiveKeychainEntry = true even though the delete failed")
		}
	})

	t.Run("off macOS there is no item to shadow the file", func(t *testing.T) {
		s, _ := newTestStore(t, platform.Linux)
		if !s.deleteActiveKeychainEntry() {
			t.Error("deleteActiveKeychainEntry = false off macOS")
		}
	})
}

// ~/.claude belongs to Claude Code; narrowing its mode is not ccswap's call, and
// the parent of ~/.claude.json is the user's home directory.
func TestActiveCredentialsFileDoesNotHardenItsDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX file modes are not meaningful on Windows")
	}
	s, _ := newTestStore(t, platform.Linux)
	dir := filepath.Dir(s.paths.CredentialsPath())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := s.writeActiveCredentialsFile(oauthJSON); err != nil {
		t.Fatalf("writeActiveCredentialsFile: %v", err)
	}

	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("directory mode = %#o, want the original 0755 left alone", got)
	}
	fileInfo, err := os.Stat(s.paths.CredentialsPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Errorf("credentials file mode = %#o, want 0600", got)
	}
}
