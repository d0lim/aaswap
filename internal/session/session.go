// Package session runs one Claude Code instance as a chosen account without
// touching the machine's default login.
//
// It works by pointing Claude Code at a per-account profile directory through
// CLAUDE_CONFIG_DIR. Everything Claude Code keeps — its credential, its
// identity, its history — lives inside that directory, so several accounts can
// run side by side in different terminals and none of them disturbs the login
// `aaswap switch` manages.
//
// # aaswap never writes Claude Code's hashed Keychain item
//
// On macOS, Claude Code derives a per-profile Keychain service name by hashing
// the CLAUDE_CONFIG_DIR value it was given, and once it has written there, that
// item shadows the plaintext seed. aaswap SEEDS a profile by writing the
// plaintext file and DELETES a stale item before seeding, but it never writes
// the hashed item: writing it would make aaswap the item's creator, and macOS
// then prompts the user for permission every time the aaswap binary changes.
//
// # A profile holds the newest generation
//
// Once a session has run, the profile — not the backup store — has the freshest
// token for that account: Claude Code rotates in place and nothing syncs back.
// Reads of a profile's credential are therefore authoritative and strictly
// read-only.
package session

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/fsutil"
	"github.com/d0lim/aaswap/internal/platform"
	"github.com/d0lim/aaswap/internal/provider"
	"golang.org/x/text/unicode/norm"
)

// Files a profile carries beyond Claude Code's own.
const (
	// StaleMarkerSuffix names the deferred-invalidation marker.
	//
	// Written as a SIBLING of the profile directory, not a child. One of its
	// two writers is the fallback for an invalidation that just failed, and the
	// faults it exists for — an unwritable profile, a read-only mount — are
	// faults on that very directory.
	StaleMarkerSuffix = ".aaswap-stale-credentials"

	// ShareManifest records which entries in a profile aaswap created, so
	// turning sharing off only ever removes aaswap's own links and never the
	// user's files.
	ShareManifest = ".aaswap-shared.json"

	// MCPMirrorMarker records that this profile's MCP servers are aaswap-
	// mirrored. It gates both the one-time migration stash and the removal on
	// --no-share, so a profile's own pre-existing definitions are never
	// silently destroyed.
	MCPMirrorMarker = ".aaswap-mcp-mirror-v1"

	// MCPDisplacedStash holds session-local MCP definitions displaced by the
	// first mirror. Write-once: they land here instead of vanishing.
	MCPDisplacedStash = ".aaswap-mcp-displaced.json"
)

// SharedItems are mirrored from the default profile when sharing is on.
//
// Deliberately excludes everything account- or instance-scoped: plugins,
// sessions, editor state, the config file and the credential. Conversation
// history is per-account by default and moves under an explicit opt-in.
var SharedItems = []string{
	"settings.json",
	"keybindings.json",
	"CLAUDE.md",
	"skills",
	"commands",
	"agents",
}

// HistoryItems are linked additionally when history sharing is asked for.
var HistoryItems = []string{
	"projects",
	"history.jsonl",
}

// SlugifyEmail makes an address safe as a directory name.
//
// Uniqueness comes from the slot-number prefix on the directory, so this only
// has to be SAFE — including on filesystems that forbid characters POSIX
// allows — not injective.
func SlugifyEmail(email string) string {
	normalized := norm.NFC.String(email)
	var b strings.Builder
	for _, r := range normalized {
		switch {
		case r < 128 && (isAlphanumeric(r) || r == '.' || r == '_' || r == '-'):
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func isAlphanumeric(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// DirFor is an account's profile directory, for one provider.
//
// The profile contains the tool's own per-process records, so a full path looks
// like <backup>/sessions/2-user_example.com/sessions/1234.json. That nesting is
// intentional: the inner one is the tool's.
//
// Scoped by provider because a profile is a whole synthetic HOME. One address at
// two tools is the ordinary case for one person, and an unscoped path gave both
// the same directory — two live credentials and two sets of shared links under
// one manifest. The declarations overlap where it matters: Claude and Codex both
// call `skills` and `history.jsonl` shareable, and `sessions` is Claude Code's
// per-process record directory while Codex declares it as history pointing at
// the rollout files aaswap reads its quota from.
//
// Claude keeps the unsuffixed path. Profiles outlive a change here — they hold a
// copy of a credential and a manifest of links aaswap created — and moving them
// would orphan both with nothing left to clean them up.
func DirFor(backupRoot, provider, accountNum, email string) string {
	parts := []string{backupRoot, "sessions"}
	if provider != "" && provider != claudeProvider {
		parts = append(parts, provider)
	}
	return filepath.Join(append(parts, accountNum+"-"+SlugifyEmail(email))...)
}

// claudeProvider owns the unsuffixed path. A profile directory always carries an
// address, so <account>-<email> can never collide with a bare provider name.
const claudeProvider = "claude"

// StaleMarkerFor is where a profile's stale marker is written.
func StaleMarkerFor(sessionDir string) string {
	return sessionDir + StaleMarkerSuffix
}

// AdoptLegacyMarker renames a marker written under the old command name to its
// current spelling.
//
// The command was renamed from ccswap to aaswap, and every marker in this file
// is named after it. Profiles outlive the rename, so ignoring the old spelling
// would not merely lose a file — it would change behavior: a stale profile
// would stop announcing itself and keep serving a superseded credential, an
// orphaned share manifest would leave aaswap's own links behind on --no-share,
// and an unseen mirror marker would re-run the one-time MCP migration against
// servers that were already mirrored.
//
// One predecessor, not a chain. ccswap is the only spelling that ever shipped a
// profile; the cswap era was the Python implementation, which wrote none of
// these markers on a machine this binary will ever meet.
//
// Renaming rather than reading both names on: the profile converges on one
// spelling, so the compatibility shim stays here instead of spreading into
// every reader. Best effort — a failure leaves the profile exactly as it was,
// which is the same state a pre-rename aaswap would find.
func AdoptLegacyMarker(path string) {
	if _, err := os.Lstat(path); err == nil {
		return // already current; nothing to adopt
	}
	dir, base := filepath.Split(path)
	suffix, ok := strings.CutPrefix(base, ".aaswap-")
	if !ok {
		return
	}
	legacy := filepath.Join(dir, ".ccswap-"+suffix)
	if _, err := os.Lstat(legacy); err != nil {
		return
	}
	if err := os.Rename(legacy, path); err != nil {
		slog.Warn("could not adopt a session marker written under the old command name",
			"from", legacy, "to", path, "error", err)
	}
}

// IsStale reports whether a profile was marked for re-seeding.
//
// The marker is set when a slot's stored credential changes while a session is
// live: aaswap never pulls credentials out from under a running Claude Code, so
// the invalidation is deferred to the next launch that finds the profile quiet.
func IsStale(sessionDir string) bool {
	AdoptLegacyMarker(StaleMarkerFor(sessionDir))
	_, err := os.Lstat(StaleMarkerFor(sessionDir))
	return err == nil
}

// MarkStale defers a profile's invalidation to its next quiet launch.
func MarkStale(sessionDir string) error {
	path := StaleMarkerFor(sessionDir)
	// Adopt first: writing the current name while an old one survives would
	// leave the profile carrying both.
	AdoptLegacyMarker(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("%w: marking a session profile stale: %w", apperr.ErrSession, err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return fmt.Errorf("%w: marking a session profile stale: %w", apperr.ErrSession, err)
	}
	return nil
}

// ClearStale removes the marker, reporting whether there was one.
func ClearStale(sessionDir string) bool {
	AdoptLegacyMarker(StaleMarkerFor(sessionDir))
	err := os.Remove(StaleMarkerFor(sessionDir))
	return err == nil
}

// Manager runs sessions for one backup root.
//
// Its collaborators are fields, so a test drives the whole surface without a
// real Claude Code, a real Keychain, or a real process.
type Manager struct {
	BackupRoot string
	Platform   platform.Platform
	Creds      *credstore.Store

	// Keychain deletes a profile's hashed item before seeding. Nil skips it,
	// which is right off macOS and in a test.
	// Profiles is how the provider's tool keeps a credential inside a profile
	// directory. Nil disables every profile-credential operation, which is a
	// supported state: a caller that never seeds needs none of them.
	Profiles provider.ProfileStore

	// Probe answers whether a profile can authenticate. Nil makes every probe
	// report Unknown, which callers resolve from local artifacts rather than by
	// destroying anything.
	Probe Prober

	// Spec is the provider whose profiles this manager keeps: which file holds
	// the credential, whether there is a config beside it, what a profile
	// mirrors, and whether running sessions can be detected at all.
	//
	// The zero value resolves to Claude's declaration, which is what every
	// caller predating providers meant.
	Spec provider.Spec

	// ConfigsDir is where captured account configs live.
	//
	// Supplied rather than derived from BackupRoot: the switcher scopes it by
	// provider, and a second derivation here silently read the pre-provider
	// location — so every Claude session refused to seed with "no stored
	// config backup" while the config sat one directory away.
	ConfigsDir string

	// Now is the clock.
	Now func() time.Time
}

// Dir is a slot's profile directory.
func (m *Manager) Dir(accountNum, email string) string {
	return DirFor(m.BackupRoot, m.spec().Name, accountNum, email)
}

// spec is this manager's provider declaration, defaulting to Claude's.
func (m *Manager) spec() provider.Spec {
	if m.Spec.Name == "" {
		return provider.MustLookup(provider.Claude)
	}
	return m.Spec
}

// secretName is the file a profile keeps its credential in, relative to the
// profile directory.
func (m *Manager) secretName() string {
	if secrets := m.spec().SecretFiles(); len(secrets) > 0 {
		return secrets[0].Path
	}
	return claudeProfileSecret
}

// configName is the account-scoped config beside the credential, empty for a
// provider whose credential is the whole story.
func (m *Manager) configName() string {
	if file, ok := m.spec().ConfigFile(); ok {
		return file.Path
	}
	return ""
}

// LivePIDs lists the processes running against a slot's profile.
//
// Empty for a provider that declares no way to detect them — which is NOT the
// same as "nothing is running", and callers must not read it as such. Ask
// CanDetectSessions before drawing a conclusion from an empty list.
func (m *Manager) LivePIDs(accountNum, email string) []int {
	liveness := m.liveness()
	if liveness == nil {
		return nil
	}
	pids, _ := liveness.PIDs(m.Dir(accountNum, email))
	return pids
}

// liveness is this provider's process detector, nil when it declares none.
func (m *Manager) liveness() provider.Liveness {
	if session := m.spec().Session; session != nil {
		return session.Liveness
	}
	return nil
}

// CanProbe reports whether the provider's own tool can be asked who a profile
// is logged in as.
//
// Only Claude Code publishes such a command, so for every other provider a
// verdict of Unknown means "nothing could be asked" rather than "the question
// went unanswered" — two different things to tell a user, and the second one
// names a binary they do not have.
func (m *Manager) CanProbe() bool { return m.Probe != nil }

// CanDetectSessions reports whether this provider can be asked what is running.
//
// The distinction the whole reseeding rule turns on: a provider that cannot be
// asked never gets an affirmative "nothing is running", so aaswap never
// refreshes a profile's credential out from under a session it cannot see.
func (m *Manager) CanDetectSessions() bool { return m.liveness() != nil }

// claudeProfileSecret is the fallback profile credential name.
const claudeProfileSecret = ".credentials.json"

// Quiescent reports that nothing is running against a slot's profile AND every
// record was readable.
//
// False for a provider with no way to detect processes. That is the safe
// direction and it is deliberate: proving a profile idle takes evidence, and
// having none is not evidence of absence. It costs a reseed that could have
// been done; the alternative costs a running agent its credential.
func (m *Manager) Quiescent(accountNum, email string) bool {
	liveness := m.liveness()
	if liveness == nil {
		return false
	}
	pids, complete := liveness.PIDs(m.Dir(accountNum, email))
	return complete && len(pids) == 0
}

// ReadCredentials reads a profile's CURRENT credential, or empty when there is
// none readable.
//
// Once a session has run, this — not the backup — is the newest generation of
// the account's token family. Strictly read-only: rotating what is here would
// log the next launch out the same way.
func (m *Manager) ReadCredentials(sessionDir string) string {
	if m.Profiles == nil {
		return ""
	}
	return m.Profiles.Read(sessionDir)
}

// MayHaveCredentialMaterial reports whether a profile's credential is anything
// but definitely absent.
//
// An EXISTENCE test, not a read, and false only when every store is POSITIVELY
// empty. An unreadable Keychain is indeterminate and leans present: the profile
// may hold the freshest generation of the account's token family, and
// re-seeding over it would begin by deleting that item.
func (m *Manager) MayHaveCredentialMaterial(sessionDir string) bool {
	if m.Profiles == nil {
		return false
	}
	return m.Profiles.MayHold(sessionDir)
}

// Identity is the account a profile is logged in as.
type Identity struct {
	Email            string
	OrganizationUUID string
}

// ReadIdentity reads the account a profile is currently logged in as.
//
// Claude Code records it in the profile's config and rewrites it on every
// login, so this reflects the profile's CURRENT identity — which an in-session
// /login can re-point at a different account than the slot the profile was made
// for.
func (m *Manager) ReadIdentity(sessionDir string) (Identity, bool) {
	if name := m.configName(); name != "" {
		return m.readIdentityFromConfig(filepath.Join(sessionDir, name))
	}
	// No config beside the credential: the credential is the identity
	// document, so the provider's own resolver reads it.
	spec := m.spec()
	data, err := os.ReadFile(filepath.Join(sessionDir, m.secretName()))
	if err != nil {
		return Identity{}, false
	}
	identity, ok := spec.Resolve(map[string]string{m.secretName(): string(data)})
	if !ok {
		return Identity{}, false
	}
	return Identity{Email: identity.Email, OrganizationUUID: identity.OrganizationUUID}, true
}

func (m *Manager) readIdentityFromConfig(path string) (Identity, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, false
	}
	var config struct {
		OAuthAccount *struct {
			EmailAddress     string `json:"emailAddress"`
			OrganizationUUID string `json:"organizationUuid"`
		} `json:"oauthAccount"`
	}
	if err := json.Unmarshal(data, &config); err != nil || config.OAuthAccount == nil {
		return Identity{}, false
	}
	if config.OAuthAccount.EmailAddress == "" {
		return Identity{}, false
	}
	return Identity{
		Email:            config.OAuthAccount.EmailAddress,
		OrganizationUUID: config.OAuthAccount.OrganizationUUID,
	}, true
}

// IdentityDrifted reports whether a profile is logged in as a DIFFERENT account
// than its slot.
//
// An in-session /login — after the slot's account hit its rate limit mid-session,
// say — re-points the profile's credential at another account while the
// directory keeps claiming the original slot.
//
// An unreadable identity is NOT drift. Missing metadata degrades to trusting the
// profile, whose token family is normally the slot's freshest, rather than
// abandoning it over a broken config file. The organization is compared only
// when both sides state one, so a renamed field degrades to an address check
// instead of producing false drift.
func (m *Manager) IdentityDrifted(sessionDir string, want Identity) bool {
	got, ok := m.ReadIdentity(sessionDir)
	if !ok {
		return false
	}
	if got.Email != want.Email {
		return true
	}
	return got.OrganizationUUID != "" && want.OrganizationUUID != "" &&
		got.OrganizationUUID != want.OrganizationUUID
}

// ClearProfileCredential removes whatever would shadow a freshly seeded
// credential, best effort.
//
// Needed before seeding and on removal. What it clears is the provider's
// business — for Claude Code on macOS it is a Keychain item named after the
// directory, which cannot be recomputed once the directory is gone.
func (m *Manager) ClearProfileCredential(sessionDir string) {
	if m.Profiles == nil {
		return
	}
	m.Profiles.Clear(sessionDir)
}

// Bootstrap seeds a profile from a slot's stored credential and config.
//
// The caller holds the store lock. No network happens here: any refresh runs
// before the lock is taken, and this reads whatever that left behind.
func (m *Manager) Bootstrap(sessionDir, accountNum, email string) error {
	// Claude Code reads the Keychain before the file, so a stale item from an
	// earlier profile at this path would shadow the seed.
	m.ClearProfileCredential(sessionDir)

	credentials, unreadable := m.Creds.ReadAccount(accountNum, email)
	if credentials == "" {
		if unreadable {
			return fmt.Errorf("%w: account %s's backup is in the macOS Keychain but it is "+
				"unreadable right now (locked, or no GUI session). Retry from a GUI "+
				"terminal; do not re-add", apperr.ErrSession, accountNum)
		}
		return fmt.Errorf("%w: account %s has no stored credentials. Log in as it, then "+
			"run: aaswap login --capture --name %s",
			apperr.ErrSession, accountNum, accountNum)
	}

	// Read AFTER the credential and BEFORE any write, but only for a provider
	// that has a config to seed. Demanding a stored config from a provider
	// whose credential is the whole login would refuse every session it could
	// otherwise run.
	configName := m.configName()
	var identity, theme jsontext.Value
	if configName != "" {
		var err error
		if identity, theme, err = m.storedIdentity(accountNum, email); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return fmt.Errorf("%w: creating the session profile: %w", apperr.ErrSession, err)
	}
	if err := os.Chmod(sessionDir, 0o700); err != nil {
		slog.Warn("could not restrict the session profile's permissions",
			"profile", sessionDir, "error", err)
	}

	if err := os.WriteFile(filepath.Join(sessionDir, m.secretName()),
		[]byte(credentials), 0o600); err != nil {
		return fmt.Errorf("%w: seeding the session credential: %w", apperr.ErrSession, err)
	}

	if configName == "" {
		// No account-scoped config to write. The credential carries the
		// identity, and everything else in the home belongs to the machine —
		// writing an identity block into it would be writing into the user's
		// own settings.
		slog.Info("bootstrapped a session profile", "account", accountNum, "profile", sessionDir)
		return nil
	}

	// Merged into any existing config, so re-seeding preserves the profile's
	// own projects and history.
	configPath := filepath.Join(sessionDir, configName)
	config := map[string]jsontext.Value{}
	if data, err := os.ReadFile(configPath); err == nil {
		if err := json.Unmarshal(data, &config); err != nil || config == nil {
			config = map[string]jsontext.Value{}
		}
	}
	config["oauthAccount"] = identity
	config["hasCompletedOnboarding"] = jsontext.Value("true")
	if _, present := config["theme"]; !present {
		// Load-bearing: Claude Code shows onboarding when the theme is unset,
		// and a fresh profile that walks the user through setup is not the
		// session they asked for.
		config["theme"] = theme
	}
	data, err := json.Marshal(config, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("%w: encoding the session config: %w", apperr.ErrSession, err)
	}
	if err := os.WriteFile(configPath, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("%w: writing the session config: %w", apperr.ErrSession, err)
	}

	slog.Info("bootstrapped a session profile", "account", accountNum, "profile", sessionDir)
	return nil
}

// configsDir is where captured configs live, falling back to the pre-provider
// location for a caller that did not say.
func (m *Manager) configsDir() string {
	if m.ConfigsDir != "" {
		return m.ConfigsDir
	}
	return filepath.Join(m.BackupRoot, "configs")
}

// storedIdentity reads a slot's captured identity block and theme.
func (m *Manager) storedIdentity(accountNum, email string) (identity, theme jsontext.Value, err error) {
	path := filepath.Join(m.configsDir(),
		fmt.Sprintf(".claude-config-%s-%s.json", accountNum, email))
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		return nil, nil, fmt.Errorf("%w: account %s has no stored config backup. Log in "+
			"as it, then run: aaswap login --capture --name %s",
			apperr.ErrSession, accountNum, accountNum)
	}
	var stored map[string]jsontext.Value
	if err := json.Unmarshal(data, &stored); err != nil || stored == nil {
		return nil, nil, fmt.Errorf("%w: account %s's stored config could not be parsed",
			apperr.ErrSession, accountNum)
	}
	account, present := stored["oauthAccount"]
	if !present || string(account) == "null" {
		return nil, nil, fmt.Errorf("%w: account %s's stored config carries no account "+
			"identity. Log in as it, then run: aaswap login --capture --name %s",
			apperr.ErrSession, accountNum, accountNum)
	}
	theme = stored["theme"]
	if len(theme) == 0 || string(theme) == "null" {
		theme = jsontext.Value(`"dark"`)
	}
	return account, theme, nil
}

// ProfileSuperseded reports that a profile PROVABLY holds a different
// credential generation than the slot's backup.
//
// Compared by lineage fingerprint, because that is what identifies a
// generation: a profile seeded before a rotation is the same ACCOUNT holding
// the predecessor token — well-formed, unexpired, and rejected by the server on
// its first refresh. Identity checks cannot see that.
//
// False whenever the question cannot be answered: no profile credential, or an
// absent or unreadable backup. Each of those is "unknown", and the caller
// spends this answer on discarding a working profile — so unknown has to mean
// leave it alone. Unreadable is not absent, the same rule every stored-credential
// read in this codebase follows, and it binds harder here: one momentary
// Keychain lock would otherwise re-bootstrap every profile on the machine.
func (m *Manager) ProfileSuperseded(sessionDir, accountNum, email string) bool {
	profile := m.ReadCredentials(sessionDir)
	if profile == "" {
		return false
	}
	backup, unreadable := m.Creds.ReadAccount(accountNum, email)
	if unreadable || backup == "" {
		return false
	}
	return claudeapi.Fingerprint(profile) != claudeapi.Fingerprint(backup)
}

// Remove deletes a profile entirely.
//
// The Keychain item goes first: once the directory is gone its hashed service
// name cannot be recomputed, and the item would linger forever.
func (m *Manager) Remove(sessionDir string) error {
	m.ClearProfileCredential(sessionDir)
	if err := os.RemoveAll(sessionDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%w: removing the session profile: %w", apperr.ErrSession, err)
	}
	// A sibling, so removing the directory did not take it.
	ClearStale(sessionDir)
	return nil
}

// Environment builds the environment a session runs in.
//
// The provider's auth overrides are DROPPED rather than passed through: any of
// them would override the account inside the tool, making `aaswap run work`
// silently run as something else. The list is the declaration's, not a fixed
// one: Claude's three variables mean nothing to Codex, and OPENAI_API_KEY —
// which Codex prefers over its ChatGPT login — was passing straight through.
func Environment(base []string, sessionDir string, spec provider.Spec) (env []string, scrubbed []string) {
	return provider.Environment(base, sessionDir, spec)
}

// writeJSONPrivate writes an owner-only JSON file.
func writeJSONPrivate(path string, value any) error {
	data, err := json.Marshal(value, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return fmt.Errorf("%w: encoding %s: %w", apperr.ErrSession, path, err)
	}
	return fsutil.WriteFileAtomic(path, append(data, '\n'))
}
