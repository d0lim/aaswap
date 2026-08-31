package credstore

import (
	json "encoding/json/v2"
	"reflect"
	"strings"
	"testing"
)

func TestLooksLikeAPIKey(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"a managed key", "sk-ant-api03-abc123", true},
		{"surrounded by whitespace", "  sk-ant-api03-abc\n", true},
		{"empty", "", false},
		{"OAuth JSON", `{"claudeAiOauth":{"accessToken":"x"}}`, false},
		// The prefix check is strict precisely so a raw or garbled setup token
		// is never misclassified as an API key.
		{"a setup token", "sk-ant-oat01-abc123", false},
		{"JSON that happens to mention the prefix", `{"key":"sk-ant-api03-abc"}`, false},
		{"a near-miss prefix", "sk-ant-apX03-abc", false},
		{"plain garbage", "not a credential", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LooksLikeAPIKey(tt.in); got != tt.want {
				t.Errorf("LooksLikeAPIKey(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestSharedCredentialFields(t *testing.T) {
	t.Run("returns only the allowlisted keys", func(t *testing.T) {
		in := `{
			"claudeAiOauth": {"accessToken": "live"},
			"trustedDeviceToken": "device-token",
			"mcpOAuth": {"server": "tok"},
			"pluginSecrets": {"p": "s"},
			"someFutureKey": "value"
		}`
		got, ok := SharedCredentialFields(in)
		if !ok {
			t.Fatal("SharedCredentialFields reported a non-object")
		}
		want := map[string]any{
			"mcpOAuth":      map[string]any{"server": "tok"},
			"pluginSecrets": map[string]any{"p": "s"},
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	// An empty map is authoritative, not "no answer": a key absent from it is
	// absent from the machine's current shared state, and must not be
	// resurrected from a slot's snapshot.
	t.Run("an object with no shared keys yields an empty map, not a miss", func(t *testing.T) {
		got, ok := SharedCredentialFields(`{"claudeAiOauth":{"accessToken":"x"}}`)
		if !ok {
			t.Fatal("SharedCredentialFields reported a non-object")
		}
		if got == nil {
			t.Fatal("got nil, want an empty map")
		}
		if len(got) != 0 {
			t.Errorf("got %v, want an empty map", got)
		}
	})

	t.Run("non-objects report a miss", func(t *testing.T) {
		for _, in := range []string{"", "sk-ant-api03-abc", "not json", "[1,2]", "null", `"a string"`} {
			if _, ok := SharedCredentialFields(in); ok {
				t.Errorf("SharedCredentialFields(%q) reported an object", in)
			}
		}
	})
}

func TestMergeSharedCredentialFields(t *testing.T) {
	// The allowlisted keys are wholly live-owned, presence and absence alike.
	t.Run("live shared fields replace the slot's copies", func(t *testing.T) {
		target := `{"claudeAiOauth":{"accessToken":"slot"},"mcpOAuth":{"stale":"yes"}}`
		shared := map[string]any{"mcpOAuth": map[string]any{"fresh": "yes"}}

		got := decode(t, MergeSharedCredentialFields(target, shared))
		if !reflect.DeepEqual(got["mcpOAuth"], map[string]any{"fresh": "yes"}) {
			t.Errorf("mcpOAuth = %v, want the live copy", got["mcpOAuth"])
		}
		if !reflect.DeepEqual(got["claudeAiOauth"], map[string]any{"accessToken": "slot"}) {
			t.Errorf("claudeAiOauth = %v, want the target's own login", got["claudeAiOauth"])
		}
	})

	// A shared key the machine no longer holds must not come back from the
	// slot's snapshot.
	t.Run("a shared key absent from the machine is dropped", func(t *testing.T) {
		target := `{"claudeAiOauth":{"accessToken":"slot"},"pluginSecrets":{"old":"secret"}}`

		got := decode(t, MergeSharedCredentialFields(target, map[string]any{}))
		if _, present := got["pluginSecrets"]; present {
			t.Error("pluginSecrets was resurrected from the slot's snapshot")
		}
	})

	// Account-scoped and unknown siblings stay with the slot: a stale restore
	// merely re-prompts for auth, while carrying a live account-bound field
	// across a switch would present one account's credential under another.
	t.Run("account-scoped and unknown fields stay slot-owned", func(t *testing.T) {
		target := `{"claudeAiOauth":{"accessToken":"slot"},"trustedDeviceToken":"slot-device","futureKey":42}`

		got := decode(t, MergeSharedCredentialFields(target, map[string]any{}))
		if got["trustedDeviceToken"] != "slot-device" {
			t.Errorf("trustedDeviceToken = %v, want the slot's own", got["trustedDeviceToken"])
		}
		if got["futureKey"] != float64(42) {
			t.Errorf("futureKey = %v, want it passed through untouched", got["futureKey"])
		}
	})

	// Managed API keys and opaque legacy shapes must stay activatable verbatim.
	t.Run("non-login credentials pass through unchanged", func(t *testing.T) {
		for _, in := range []string{
			"sk-ant-api03-abc",
			"not json",
			"",
			`{"somethingElse":1}`, // an object, but not a Claude login
		} {
			if got := MergeSharedCredentialFields(in, map[string]any{"mcpOAuth": "x"}); got != in {
				t.Errorf("MergeSharedCredentialFields(%q) = %q, want it unchanged", in, got)
			}
		}
	})

	t.Run("output is deterministic", func(t *testing.T) {
		target := `{"claudeAiOauth":{"a":1},"z":1,"m":2,"b":3}`
		shared := map[string]any{"mcpOAuth": "x", "pluginSecrets": "y"}
		first := MergeSharedCredentialFields(target, shared)
		for range 5 {
			if got := MergeSharedCredentialFields(target, shared); got != first {
				t.Fatalf("output varies between calls:\n%s\n%s", got, first)
			}
		}
	})
}

func TestApprovedForm(t *testing.T) {
	// Mirrors Claude Code's normalizeApiKeyForConfig (apiKey.slice(-20)).
	// Storing anything else makes its "is this key approved?" check miss and
	// re-prompt the user.
	tests := []struct {
		in, want string
	}{
		{"sk-ant-api03-" + strings.Repeat("x", 30), strings.Repeat("x", 20)},
		{"  sk-ant-api03-abcdefghijklmnopqrst  ", "abcdefghijklmnopqrst"},
		{"short", "short"},
		{strings.Repeat("y", 20), strings.Repeat("y", 20)},
		{"", ""},
	}
	for _, tt := range tests {
		if got := ApprovedForm(tt.in); got != tt.want {
			t.Errorf("ApprovedForm(%q) = %q, want %q", tt.in, got, tt.want)
		}
		if got := ApprovedForm(tt.in); len(got) > 20 {
			t.Errorf("ApprovedForm(%q) returned %d chars, want at most 20", tt.in, len(got))
		}
	}
}

// The service names are a compatibility contract: Claude Code reads and writes
// the same items, so a changed name silently stops the two seeing each other.
func TestServiceNames(t *testing.T) {
	if ClaudeOAuthService != "Claude Code-credentials" {
		t.Errorf("ClaudeOAuthService = %q", ClaudeOAuthService)
	}
	// Deliberately without the -credentials suffix: Claude Code resolves the
	// managed key on a separate auth axis.
	if ClaudeManagedKeyService != "Claude Code" {
		t.Errorf("ClaudeManagedKeyService = %q", ClaudeManagedKeyService)
	}
	// Distinct from the legacy keyring service so old and new items coexist
	// during migration.
	if BackupService != "claude-swap" {
		t.Errorf("BackupService = %q", BackupService)
	}
}

func decode(t *testing.T, s string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("Unmarshal(%q): %v", s, err)
	}
	return out
}
