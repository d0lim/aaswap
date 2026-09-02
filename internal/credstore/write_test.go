package credstore

import (
	json "encoding/json/v2"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/keychain"
	"github.com/d0lim/aaswap/internal/platform"
)

func readGlobalConfigForTest(t *testing.T, s *Store) map[string]any {
	t.Helper()
	b, err := os.ReadFile(s.paths.GlobalConfigPath())
	if err != nil {
		t.Fatalf("read global config: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("parse global config: %v", err)
	}
	return cfg
}

const apiKey = "sk-ant-api03-0123456789abcdefghijklmnop"

// ---------------------------------------------------------------- OAuth writes

func TestWriteOAuthRoundTrips(t *testing.T) {
	for _, p := range []platform.Platform{platform.MacOS, platform.Linux, platform.Windows} {
		t.Run(p.String(), func(t *testing.T) {
			s, _ := newTestStore(t, p)
			if err := s.WriteActive(oauthJSON); err != nil {
				t.Fatalf("WriteActive: %v", err)
			}
			if got := s.ReadActive(); got.Value != oauthJSON {
				t.Errorf("read back %q, want the written credential", got.Value)
			}
		})
	}
}

func TestOAuthWriteRecordsItsBackend(t *testing.T) {
	t.Run("macOS records the keychain", func(t *testing.T) {
		s, _ := newTestStore(t, platform.MacOS)
		if err := s.WriteActive(oauthJSON); err != nil {
			t.Fatal(err)
		}
		if got := s.LastActiveBackend(); got != "keychain" {
			t.Errorf("LastActiveBackend = %q, want \"keychain\"", got)
		}
	})

	t.Run("off macOS records the file", func(t *testing.T) {
		s, _ := newTestStore(t, platform.Linux)
		if err := s.WriteActive(oauthJSON); err != nil {
			t.Fatal(err)
		}
		if got := s.LastActiveBackend(); got != "file" {
			t.Errorf("LastActiveBackend = %q, want \"file\"", got)
		}
	})
}

// Claude Code reads the Keychain before the plaintext file, so a stale item
// would resurrect the old account over the file aaswap just wrote (#30337).
func TestAFileFallbackClearsTheStaleKeychainItem(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	fake.items[ClaudeOAuthService+"\x00"+keychain.AccountName()] = `{"claudeAiOauth":{"accessToken":"OLD"}}`
	fake.failSet = true

	if err := s.WriteActive(oauthJSON); err != nil {
		t.Fatalf("WriteActive: %v", err)
	}
	if _, ok := fake.items[ClaudeOAuthService+"\x00"+keychain.AccountName()]; ok {
		t.Error("the stale Keychain item survived a file fallback")
	}
	if got := s.LastActiveBackend(); got != "file" {
		t.Errorf("LastActiveBackend = %q, want \"file\"", got)
	}
}

// A write that falls back must pin file mode, so a later cooldown re-probe
// cannot read a residual Keychain item and resurrect the wrong account.
func TestAFileFallbackPinsFileMode(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	fake.failSet = true
	if err := s.WriteActive(oauthJSON); err != nil {
		t.Fatal(err)
	}

	clock := &fakeClock{t: time.Now().Add(10 * RecheckCooldown)}
	s.cap.now = clock.Now
	if s.cap.useKeychain() {
		t.Error("a pinned file mode re-probed the Keychain after the cooldown window")
	}
}

// Rewrite-when-present, never create (#86): a running session invalidates its
// memoized token only when the file's mtime changes or the file is absent.
func TestAKeychainWriteRefreshesAnExistingShadowFile(t *testing.T) {
	s, _ := newTestStore(t, platform.MacOS)
	writeCredentialsFile(t, s, `{"claudeAiOauth":{"accessToken":"OLD"}}`)

	if err := s.WriteActive(oauthJSON); err != nil {
		t.Fatalf("WriteActive: %v", err)
	}
	b, err := os.ReadFile(s.paths.CredentialsPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != oauthJSON {
		t.Errorf("shadow file = %q, want it refreshed with the fresh credential", b)
	}
}

// Keychain-only users keep their fileless posture and never gain a plaintext
// credential on disk; their absent-file path already hot-reloads via the
// Keychain TTL.
func TestAKeychainWriteNeverCreatesAShadowFile(t *testing.T) {
	s, _ := newTestStore(t, platform.MacOS)

	if err := s.WriteActive(oauthJSON); err != nil {
		t.Fatalf("WriteActive: %v", err)
	}
	if _, err := os.Stat(s.paths.CredentialsPath()); err == nil {
		t.Error("a Keychain-only write created a plaintext credentials file")
	}
}

// ---------------------------------------------------------------- API key writes

func TestWriteManagedKeyRoundTrips(t *testing.T) {
	s, _ := newTestStore(t, platform.Linux)
	if err := s.WriteActive(apiKey); err != nil {
		t.Fatalf("WriteActive: %v", err)
	}
	if got := s.ReadActive(); got.Value != apiKey {
		t.Errorf("read back %q, want the API key", got.Value)
	}
}

// Skipping the approved list makes Claude Code re-prompt the user to approve
// the key they just set.
func TestWriteManagedKeyRecordsTheApprovedForm(t *testing.T) {
	s, _ := newTestStore(t, platform.Linux)
	if err := s.WriteActive(apiKey); err != nil {
		t.Fatal(err)
	}

	cfg := readGlobalConfigForTest(t, s)
	responses, ok := cfg["customApiKeyResponses"].(map[string]any)
	if !ok {
		t.Fatalf("customApiKeyResponses = %v, want an object", cfg["customApiKeyResponses"])
	}
	approved, ok := responses["approved"].([]any)
	if !ok || len(approved) != 1 || approved[0] != ApprovedForm(apiKey) {
		t.Errorf("approved = %v, want exactly the key's approved form %q", approved, ApprovedForm(apiKey))
	}
	if _, present := responses["rejected"]; !present {
		t.Error("rejected list was not initialized")
	}
}

func TestWriteManagedKeyDoesNotDuplicateApprovals(t *testing.T) {
	s, _ := newTestStore(t, platform.Linux)
	for range 3 {
		if err := s.WriteActive(apiKey); err != nil {
			t.Fatal(err)
		}
	}
	responses := readGlobalConfigForTest(t, s)["customApiKeyResponses"].(map[string]any)
	if approved := responses["approved"].([]any); len(approved) != 1 {
		t.Errorf("approved = %v, want one entry after three identical writes", approved)
	}
}

// On a Keychain success the key must stay out of the plaintext config.
func TestAKeychainManagedWriteKeepsTheKeyOutOfTheConfig(t *testing.T) {
	s, _ := newTestStore(t, platform.MacOS)
	if err := s.WriteActive(apiKey); err != nil {
		t.Fatal(err)
	}
	if got, present := readGlobalConfigForTest(t, s)["primaryApiKey"]; present {
		t.Errorf("primaryApiKey = %v, want it absent when the Keychain holds the key", got)
	}
}

func TestAFailedManagedKeychainWriteFallsBackToTheConfig(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	fake.failSet = true

	if err := s.WriteActive(apiKey); err != nil {
		t.Fatalf("WriteActive: %v", err)
	}
	if got := readGlobalConfigForTest(t, s)["primaryApiKey"]; got != apiKey {
		t.Errorf("primaryApiKey = %v, want the key written to the config", got)
	}
}

// ---------------------------------------------------------------- Mutual exclusion

// Activating one auth axis clears the other, or a stale credential shadows the
// switch and Claude Code serves the wrong account.
func TestActivatingAnAPIKeyClearsOAuth(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	fake.items[ClaudeOAuthService+"\x00"+keychain.AccountName()] = oauthJSON
	writeCredentialsFile(t, s, oauthJSON)

	if err := s.WriteActive(apiKey); err != nil {
		t.Fatalf("WriteActive: %v", err)
	}
	if _, ok := fake.items[ClaudeOAuthService+"\x00"+keychain.AccountName()]; ok {
		t.Error("the OAuth Keychain item survived an API-key activation")
	}
	if _, err := os.Stat(s.paths.CredentialsPath()); err == nil {
		t.Error("the OAuth credentials file survived an API-key activation")
	}
}

// A stale primaryApiKey surviving alongside a freshly activated OAuth
// credential is a live cross-account key that bills per token while it lies.
func TestActivatingOAuthClearsTheManagedKey(t *testing.T) {
	s, fake := newTestStore(t, platform.MacOS)
	writeGlobalConfig(t, s, map[string]any{"primaryApiKey": apiKey, "oauthAccount": map[string]any{"emailAddress": "a@example.com"}})
	fake.items[ClaudeManagedKeyService+"\x00"+keychain.AccountName()] = apiKey

	if err := s.WriteActive(oauthJSON); err != nil {
		t.Fatalf("WriteActive: %v", err)
	}
	cfg := readGlobalConfigForTest(t, s)
	if got, present := cfg["primaryApiKey"]; present {
		t.Errorf("primaryApiKey = %v, want it cleared", got)
	}
	if _, ok := fake.items[ClaudeManagedKeyService+"\x00"+keychain.AccountName()]; ok {
		t.Error("the managed-key Keychain item survived an OAuth activation")
	}
	// Everything else in the config must survive.
	if _, present := cfg["oauthAccount"]; !present {
		t.Error("oauthAccount was lost while clearing the managed key")
	}
}

// removeApiKey does not clear the approved list either, and removing it would
// force recovering the approved form from the Keychain for no benefit.
func TestClearingTheManagedKeyLeavesTheApprovedList(t *testing.T) {
	s, _ := newTestStore(t, platform.Linux)
	if err := s.WriteActive(apiKey); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteActive(oauthJSON); err != nil {
		t.Fatal(err)
	}

	responses, ok := readGlobalConfigForTest(t, s)["customApiKeyResponses"].(map[string]any)
	if !ok {
		t.Fatal("customApiKeyResponses was removed along with the key")
	}
	if approved := responses["approved"].([]any); len(approved) != 1 {
		t.Errorf("approved = %v, want the entry preserved", approved)
	}
}

// ---------------------------------------------------------------- Global config

// Every key aaswap does not own must survive a write. Losing oauthAccount,
// projects or mcpServers would take the user's whole Claude Code state with it.
func TestUpdateGlobalConfigPreservesForeignKeys(t *testing.T) {
	s, _ := newTestStore(t, platform.Linux)
	writeGlobalConfig(t, s, map[string]any{
		"oauthAccount": map[string]any{"emailAddress": "a@example.com"},
		"projects":     map[string]any{"/tmp/x": map[string]any{"allowedTools": []any{}}},
		"mcpServers":   map[string]any{"srv": map[string]any{"command": "x"}},
	})

	if err := s.WriteActive(apiKey); err != nil {
		t.Fatalf("WriteActive: %v", err)
	}
	cfg := readGlobalConfigForTest(t, s)
	for _, key := range []string{"oauthAccount", "projects", "mcpServers"} {
		if _, present := cfg[key]; !present {
			t.Errorf("%s was lost", key)
		}
	}
}

// An unreadable config is refused rather than treated as absent: an atomic
// replace would otherwise write a near-empty object over a file it never read.
// Measured against a torn config — a valid prefix with a truncated tail, which
// is what a crash mid-write leaves — oauthAccount, projects and mcpServers were
// all gone.
func TestUpdateGlobalConfigRefusesToOverwriteAnUnreadableFile(t *testing.T) {
	s, _ := newTestStore(t, platform.Linux)
	const torn = `{"oauthAccount": {"emailAddress": "a@example.com"}, "projects": {`
	if err := os.WriteFile(s.paths.GlobalConfigPath(), []byte(torn), 0o600); err != nil {
		t.Fatal(err)
	}

	err := s.updateGlobalConfig(func(cfg map[string]any) { cfg["primaryApiKey"] = apiKey })
	if !errors.Is(err, apperr.ErrCredentialWrite) {
		t.Fatalf("updateGlobalConfig = %v, want it to refuse with a write error", err)
	}
	b, readErr := os.ReadFile(s.paths.GlobalConfigPath())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(b) != torn {
		t.Errorf("the torn config was rewritten as %q", b)
	}
}

// An absent config has nothing to preserve and is a genuine start.
func TestUpdateGlobalConfigCreatesAnAbsentFile(t *testing.T) {
	s, _ := newTestStore(t, platform.Linux)
	if err := s.updateGlobalConfig(func(cfg map[string]any) { cfg["primaryApiKey"] = apiKey }); err != nil {
		t.Fatalf("updateGlobalConfig: %v", err)
	}
	if got := readGlobalConfigForTest(t, s)["primaryApiKey"]; got != apiKey {
		t.Errorf("primaryApiKey = %v, want the written key", got)
	}
}

// The parent of ~/.claude.json is the user's home directory; hardening it would
// silently change the permissions of everything the user keeps there.
func TestGlobalConfigWriteDoesNotHardenTheHomeDirectory(t *testing.T) {
	if platform.Detect().IsWindows() {
		t.Skip("POSIX file modes are not meaningful on Windows")
	}
	s, _ := newTestStore(t, platform.Linux)
	home := filepath.Dir(s.paths.GlobalConfigPath())
	if err := os.Chmod(home, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := s.updateGlobalConfig(func(cfg map[string]any) { cfg["primaryApiKey"] = apiKey }); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(home)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("home directory mode = %#o, want the original 0755 left alone", got)
	}
}
