package swap

import (
	"context"
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"log/slog"
	"os"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/fsutil"
	"github.com/d0lim/aaswap/internal/lockfile"
	"github.com/d0lim/aaswap/internal/pollpolicy"
	"github.com/d0lim/aaswap/internal/usagestore"
)

// AccountRef names one side of a switch.
//
// Number is empty for a live login aaswap does not manage, which is a real state
// and not an error: the machine can be logged into an account that was never
// added.
type AccountRef struct {
	Name  string
	Email string
}

// SwitchOutcome is what a switch did, captured under the lock so a caller never
// has to reconstruct the departing side after the mutation.
type SwitchOutcome struct {
	// From is the account left, or nil on a machine that had no live login.
	From *AccountRef
	To   AccountRef
	// Warnings are conditions the user must see but which did not stop the
	// switch.
	Warnings []string
	// Activated marks the direct path — no live login to back up first.
	Activated bool
}

// SwitchRequest is one activation.
type SwitchRequest struct {
	// Target is the slot to activate.
	Target string
	// Force routes through the direct activation path even when a managed live
	// login exists: the stored backup is written over the live credential
	// without backing the live one up first. For recovering from an import
	// whose live login is stale.
	Force bool
	// Condemned reports whether an earlier probe in this process already proved
	// a lineage foreign. Optional.
	Condemned func(fingerprint string) bool
}

// Switch activates a managed account.
//
// The shape of this operation is dictated by one rule: no network I/O while a
// lock is held. So ownership is resolved first, without any lock; then aaswap's
// store lock AND Claude Code's own advisory locks are taken for the whole
// mutation, rollback included.
//
// Claude Code's locks matter because its token refresh runs under them and
// re-reads credentials there. Holding them means a mid-refresh Claude Code
// either finishes before the swap — so the backup captures the rotated token —
// or re-checks after it and aborts. The config lock likewise keeps the identity
// splice from interleaving with Claude Code's own config writes.
func (s *Switcher) Switch(ctx context.Context, req SwitchRequest) (SwitchOutcome, error) {
	roster, err := s.RosterOrEmpty()
	if err != nil {
		return SwitchOutcome{}, err
	}
	if _, ok := roster.Accounts[req.Target]; !ok {
		return SwitchOutcome{}, fmt.Errorf("%w: account %s does not exist",
			apperr.ErrAccountNotFound, req.Target)
	}

	// Before the locks, because it may reach the network. Forced activation
	// never backs up the live credential, so it needs no lookup at all.
	var provenance Provenance
	if !req.Force {
		provenance = s.PrefetchProvenance(ctx, roster)
	}

	var outcome SwitchOutcome
	err = s.withLock(func() error {
		return s.withClaudeLocks(func() error {
			roster, err := s.RosterOrEmpty()
			if err != nil {
				return err
			}
			outcome, err = s.performSwitch(roster, req, provenance)
			return err
		})
	})
	if err != nil {
		return SwitchOutcome{}, err
	}

	// The locks are released: safe to touch the usage store, whose own lock a
	// caller may re-enter.
	s.replanNewActive(outcome.To.Name, roster)
	return outcome, nil
}

// withClaudeLocks holds Claude Code's advisory locks for the duration.
func (s *Switcher) withClaudeLocks(fn func() error) error {
	opts := lockfile.ProperOptions{}
	return lockfile.WithClaudeCredentials(s.Paths, opts, func() error {
		return lockfile.WithClaudeConfig(s.Paths, opts, fn)
	})
}

func (s *Switcher) performSwitch(roster *Roster, req SwitchRequest, provenance Provenance) (SwitchOutcome, error) {
	target := roster.Accounts[req.Target]
	outcome := SwitchOutcome{To: AccountRef{Name: req.Target, Email: target.Email}}

	live, hasLive := s.LiveIdentity()
	currentSlot := ""
	if hasLive {
		if num, managed := roster.FindName(live.Identity()); managed {
			currentSlot = num
		}
	}

	// The direct path: nothing to back up. Either the machine has no live login
	// at all, or it has one aaswap does not manage, or --force asked for the
	// overwrite. Backing up here would write a backup for a slot that does not
	// exist, or — under force — poison a slot with the very credential the user
	// called stale.
	if req.Force || !hasLive || currentSlot == "" {
		return s.activateDirect(roster, req, live, hasLive, currentSlot, outcome)
	}
	return s.switchFrom(roster, req, live, currentSlot, provenance, outcome)
}

// activateDirect writes a slot's stored credential over the live one without
// backing the live one up into a slot first.
func (s *Switcher) activateDirect(roster *Roster, req SwitchRequest, live LiveIdentity, hasLive bool, currentSlot string, outcome SwitchOutcome) (SwitchOutcome, error) {
	outcome.Activated = true
	switch {
	case !hasLive:
		outcome.From = nil
	case currentSlot == "":
		// An unmanaged live account: a real departure, with no slot to name.
		outcome.From = &AccountRef{Email: live.Email}
	default:
		outcome.From = &AccountRef{Name: currentSlot, Email: live.Email}
	}

	target := roster.Accounts[req.Target]
	targetCreds, err := s.readTargetCredentials(req.Target, target.Email)
	if err != nil {
		return SwitchOutcome{}, err
	}
	targetOAuth, targetConfig, err := s.readTargetConfig(req.Target, target.Email)
	if err != nil {
		return SwitchOutcome{}, err
	}

	// Snapshot the live state first, config identity or not: a wiped or
	// half-written config can orphan a live credential whose machine-shared
	// state still has to reach the composed credential below — and the rollback
	// needs it if activation fails partway. An UNREADABLE snapshot aborts
	// rather than overwriting state that would then have no safety copy.
	active := s.Creds.ReadActive()
	if active.FileReadFailed || active.Degraded {
		return SwitchOutcome{}, fmt.Errorf("%w: cannot snapshot the live credentials "+
			"before activation", apperr.ErrCredentialRead)
	}
	rollbackCreds := active.Value
	rollbackConfig, hadConfig := readFileIfExists(s.Paths.GlobalConfigPath())
	if _, hasConfig := s.spec().ConfigFile(); !hasConfig {
		// Nothing to roll back: the switch below writes no config at all.
		rollbackConfig, hadConfig = "", false
	}

	// This path skips the backup step, so the live credential it replaces would
	// otherwise have no surviving copy anywhere. Stash it. For an unmanaged or
	// config-orphaned login the stash is the ONLY copy; under force it guards
	// against the "stale" live login actually being the fresher generation.
	if rollbackCreds != "" && rollbackCreds != targetCreds {
		if _, err := s.stashLive(rollbackCreds, "displaced-live-login", displacedSlotLabel(currentSlot), nil); err != nil {
			if !req.Force {
				return SwitchOutcome{}, fmt.Errorf("%w: could not preserve the live "+
					"credential before activation (the safety copy failed: %w); aborting "+
					"rather than destroying it", apperr.ErrSwitch, err)
			}
			outcome.Warnings = append(outcome.Warnings, fmt.Sprintf(
				"Could not preserve the replaced live credential (the safety copy failed: %v) "+
					"— proceeding because --force explicitly rewrites the live login.", err))
		}
	}

	rollback := &switchRollback{
		configPath:     s.Paths.GlobalConfigPath(),
		originalConfig: rollbackConfig,
		hadConfig:      hadConfig,
		originalCreds:  rollbackCreds,
		store:          s,
	}

	if err := s.Creds.WriteActive(prepareForActivation(targetCreds, rollbackCreds)); err != nil {
		return SwitchOutcome{}, err
	}
	rollback.credsWritten = true

	warnings, err := s.spliceLiveConfig(targetOAuth, targetConfig)
	outcome.Warnings = append(outcome.Warnings, warnings...)
	if err != nil {
		rollback.run()
		return SwitchOutcome{}, err
	}
	rollback.configWritten = true

	roster.SetActive(req.Target)
	if err := s.WriteRoster(roster); err != nil {
		rollback.run()
		return SwitchOutcome{}, err
	}
	return outcome, nil
}

// switchFrom is the ordinary switch: back up the departing account, then
// activate the target.
func (s *Switcher) switchFrom(roster *Roster, req SwitchRequest, live LiveIdentity, currentSlot string, provenance Provenance, outcome SwitchOutcome) (SwitchOutcome, error) {
	outcome.From = &AccountRef{Name: currentSlot, Email: live.Email}

	active := s.Creds.ReadActive()
	if active.FileReadFailed || active.Degraded {
		return SwitchOutcome{}, fmt.Errorf("%w: failed to read the current credentials",
			apperr.ErrCredentialRead)
	}
	originalCreds := active.Value
	if originalCreds == "" {
		// An empty read — a Keychain timeout answers "" rather than failing —
		// must NOT be written over the departing account's backup: that would
		// destroy its stored credential. Fail the switch instead; the backup
		// stays intact and a retry costs nothing once the Keychain settles.
		return SwitchOutcome{}, fmt.Errorf("%w: the current account's credential reads "+
			"as empty (an unreadable Keychain?); refusing to overwrite its backup",
			apperr.ErrCredentialRead)
	}
	spec := s.spec()
	_, hasAccountConfig := spec.ConfigFile()
	originalConfig, hadConfig := readFileIfExists(s.Paths.GlobalConfigPath())
	switch {
	case !hasAccountConfig:
		// This provider keeps no account-scoped config, so there is none to
		// read, back up, splice or roll back. The credential is the whole
		// login.
		originalConfig, hadConfig = "", false
	case !hadConfig:
		return SwitchOutcome{}, fmt.Errorf("%w: %s's config file was not found",
			apperr.ErrConfig, spec.Name)
	}

	rollback := &switchRollback{
		configPath:     s.Paths.GlobalConfigPath(),
		originalConfig: originalConfig,
		hadConfig:      hadConfig,
		originalCreds:  originalCreds,
		store:          s,
	}

	// Step 1 — back up the departing account, but only what is actually its.
	warnings, err := s.backUpOutgoing(roster, currentSlot, live.Email, originalCreds, originalConfig, provenance, req.Condemned)
	outcome.Warnings = append(outcome.Warnings, warnings...)
	if err != nil {
		return SwitchOutcome{}, err
	}

	// Step 2 — read the target. Before anything is written, so a target with no
	// usable backup fails while the live login is still intact.
	target := roster.Accounts[req.Target]
	targetCreds, err := s.readTargetCredentials(req.Target, target.Email)
	if err != nil {
		return SwitchOutcome{}, err
	}
	targetOAuth, targetConfig, err := s.readTargetConfig(req.Target, target.Email)
	if err != nil {
		return SwitchOutcome{}, err
	}

	// Step 3 — activate.
	if err := s.Creds.WriteActive(prepareForActivation(targetCreds, originalCreds)); err != nil {
		rollback.run()
		return SwitchOutcome{}, wrapRolledBack(err)
	}
	rollback.credsWritten = true

	spliceWarnings, err := s.spliceLiveConfig(targetOAuth, targetConfig)
	outcome.Warnings = append(outcome.Warnings, spliceWarnings...)
	if err != nil {
		rollback.run()
		return SwitchOutcome{}, wrapRolledBack(err)
	}
	rollback.configWritten = true

	// Step 4 — record which slot is active.
	roster.SetActive(req.Target)
	if err := s.WriteRoster(roster); err != nil {
		rollback.run()
		return SwitchOutcome{}, wrapRolledBack(err)
	}
	return outcome, nil
}

// backUpOutgoing files the departing account's live state — but only the parts
// that are actually its.
func (s *Switcher) backUpOutgoing(roster *Roster, slot, email, credentials, config string, provenance Provenance, condemned func(string) bool) ([]string, error) {
	kind, foreignSlot := s.ClassifyOutgoing(roster, slot, email, credentials, provenance, condemned)
	var warnings []string

	switch kind {
	case KindForeign, KindAlien, KindKnownForeign:
		// Positively not this slot's bytes: they go into no slot, and they are
		// never silently destroyed. The safety copy — which fails loudly,
		// aborting before the live store is touched — is the license to
		// proceed.
		var resolved *claudeapi.Identity
		if provenance.Live == credentials {
			resolved = provenance.Resolved
		}
		if _, err := s.stashLive(credentials, string(kind), slot, resolved); err != nil {
			return warnings, fmt.Errorf("%w: the live credential does not belong to "+
				"account %s and could not be preserved (%w); aborting rather than "+
				"destroying it", apperr.ErrSwitch, slot, err)
		}
		warnings = append(warnings, foreignWarning(kind, slot, foreignSlot))

	case KindForeignSynced:
		// Another managed account's bytes, and that slot already holds this
		// lineage. Nothing needs preserving, nothing may be written.
		warnings = append(warnings, fmt.Sprintf(
			"Credential ownership mismatch detected. The live credential already matches "+
				"account %s's stored backup, so nothing was written into account %s.",
			foreignSlot, slot))

	case KindWiped:
		// The blob carries nothing to preserve, and writing it would replace
		// the slot's only surviving refresh token with empty strings — the
		// exact destruction chain observed in the field. Config only.
		if err := s.WriteAccountConfig(slot, email, config); err != nil {
			return warnings, err
		}
		warnings = append(warnings, fmt.Sprintf(
			"The live credential's tokens were wiped (Claude Code clears them when a "+
				"refresh is rejected). Account %s's stored backup was kept. If the account "+
				"cannot authenticate after switching back, log in with Claude Code and run: "+
				"aaswap add", slot))

	case KindOwnBytes:
		// Untouched since aaswap wrote it. Refresh the config backup only.
		if err := s.WriteAccountConfig(slot, email, config); err != nil {
			return warnings, err
		}
		slog.Info("backed up the departing account (config only; the credential is unchanged)",
			"account", slot)

	default: // own-family, own-rotated, unresolved
		// Unresolved falls open on purpose: most such divergences are the
		// account's own rotation, and skipping the backup would leave the slot
		// holding a consumed token. Logged rather than warned — it is
		// indistinguishable from a legitimate rotation, so a warning would cry
		// wolf on every ordinary switch.
		if err := s.Creds.WriteAccount(slot, email, credentials); err != nil {
			return warnings, err
		}
		s.BackupWritten(slot, email)
		if err := s.WriteAccountConfig(slot, email, config); err != nil {
			return warnings, err
		}
		if kind == KindOwnRotated && provenance.Resolved != nil {
			// The lookup proved the identity; backfill a missing slot uuid
			// while the roster is being rewritten anyway.
			if account := roster.Accounts[slot]; account != nil && account.UUID == "" {
				account.UUID = provenance.Resolved.UUID
			}
		}
		slog.Info("backed up the departing account", "account", slot, "kind", kind)
	}
	return warnings, nil
}

func foreignWarning(kind OutgoingKind, slot, foreignSlot string) string {
	switch kind {
	case KindForeign:
		return fmt.Sprintf("Credential ownership mismatch detected. The live credential "+
			"was preserved and was not written into account %s. If account %s later cannot "+
			"authenticate, log in as it and run: aaswap add --slot %s", slot, foreignSlot, foreignSlot)
	case KindKnownForeign:
		return fmt.Sprintf("The live credential was previously identified as another "+
			"account's. It was preserved and not written into account %s. If the owning "+
			"account later cannot authenticate, log in as it and run: aaswap add", slot)
	default:
		return fmt.Sprintf("The live login does not match a managed account. It was "+
			"preserved and not written into account %s. If you need that account, log in "+
			"as it and run: aaswap add", slot)
	}
}

// readTargetCredentials reads the switch target's stored credential, or says
// exactly why it cannot.
//
// The two failures need different advice. "No stored credential" means re-add.
// "Stored in the Keychain but unreadable right now" means retry from a GUI
// terminal — telling that user to re-add would burn the stored grant of a slot
// whose backup is merely behind a locked Keychain.
func (s *Switcher) readTargetCredentials(accountNum, email string) (string, error) {
	credentials, unreadable := s.Creds.ReadAccount(accountNum, email)
	if credentials != "" {
		return credentials, nil
	}
	if unreadable {
		return "", fmt.Errorf("%w: account %s's backup is in the macOS Keychain but it is "+
			"unreadable right now (locked, or no GUI session). Retry from a GUI terminal; "+
			"do not re-add", apperr.ErrSwitch, accountNum)
	}
	return "", fmt.Errorf("%w: account %s has no stored credentials. Re-add with: "+
		"aaswap add --slot %s", apperr.ErrSwitch, accountNum, accountNum)
}

// readTargetConfig reads the target slot's stored config and its identity
// block.
//
// Both are nil for a provider with no account-scoped config. That is not a
// missing backup: the credential is the whole login, and there is nothing to
// splice into anything. Demanding an identity block there refused every switch
// the provider could otherwise perform.
func (s *Switcher) readTargetConfig(accountNum, email string) (jsontext.Value, object, error) {
	if _, ok := s.spec().ConfigFile(); !ok {
		return nil, nil, nil
	}
	stored := s.ReadAccountConfig(accountNum, email)
	if stored == "" {
		return nil, nil, fmt.Errorf("%w: account %s has no stored config backup. Re-add "+
			"with: aaswap add --slot %s", apperr.ErrSwitch, accountNum, accountNum)
	}
	var config object
	if err := json.Unmarshal([]byte(stored), &config); err != nil {
		return nil, nil, fmt.Errorf("%w: account %s's stored config is not a JSON object: %w",
			apperr.ErrSwitch, accountNum, err)
	}
	if config == nil {
		// Valid JSON, but a literal null rather than an object.
		return nil, nil, fmt.Errorf("%w: account %s's stored config is not a JSON object",
			apperr.ErrSwitch, accountNum)
	}
	oauth, ok := config["oauthAccount"]
	if !ok || string(oauth) == "null" {
		return nil, nil, fmt.Errorf("%w: account %s's stored config carries no account "+
			"identity", apperr.ErrSwitch, accountNum)
	}
	return oauth, config, nil
}

// spliceLiveConfig puts the target's identity into the live config, preserving
// everything else in it.
//
// A no-op when the provider has no account-scoped config: see readTargetConfig.
//
// The live config holds the user's projects, MCP servers and settings. Only the
// identity block belongs to the account, so only that is replaced. When the
// live config cannot be read at all, its bytes are copied aside under a name
// the user is told about, and the target's stored config is written whole —
// replacing an unreadable config is what a freshly imported machine needs, and
// nothing local distinguishes that from a working install whose config just
// tore.
func (s *Switcher) spliceLiveConfig(targetOAuth jsontext.Value, targetConfig object) ([]string, error) {
	if targetOAuth == nil && targetConfig == nil {
		// No account-scoped config for this provider. Writing one would create
		// a file the tool never reads, in a home where a same-named file may
		// belong to something else entirely.
		return nil, nil
	}
	path := s.Paths.GlobalConfigPath()

	existing, present, err := readObject(path)
	if err == nil && present {
		// Present and readable — including a valid but EMPTY object, which
		// loses nothing by being spliced. Treating empty as unreadable would
		// tell the user their config "could not be parsed" when it parsed fine.
		existing["oauthAccount"] = targetOAuth
		return nil, s.writeLiveConfig(existing)
	}

	var warnings []string
	if err != nil {
		salvage, salvageErr := salvageUnreadable(path, s.now())
		if salvageErr != nil {
			return nil, salvageErr
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s could not be parsed — a copy was kept at %s",
			baseName(path), baseName(salvage)))
	}
	return warnings, s.writeLiveConfig(targetConfig)
}

// writeLiveConfig publishes the live config.
//
// Written through the FOREIGN writer: the config's parent directory is the
// user's home, and hardening it to owner-only would lock the user out of their
// own home directory.
func (s *Switcher) writeLiveConfig(config object) error {
	data, err := json.Marshal(config, jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Errorf("%w: encoding Claude's config: %w", apperr.ErrConfig, err)
	}
	return fsutil.WriteForeignFileAtomic(s.Paths.GlobalConfigPath(), append(data, '\n'))
}

// prepareForActivation composes the credential to activate from its two owners.
//
// The machine-shared OAuth integrations are frozen in the slot at backup time
// and may hold rotated-out tokens, while the live credential's copies are by
// definition the current generation — so for those keys the live credential
// wins, absence included. Every other field the destination slot stored travels
// with the slot: account-bound state, and anything aaswap does not recognize,
// must not leak across a switch.
func prepareForActivation(targetCredentials, liveCredentials string) string {
	shared, ok := credstore.SharedCredentialFields(liveCredentials)
	if !ok {
		// No live JSON credential to take shared fields from — a fresh machine,
		// or a managed API key is active. The stored blob activates unchanged.
		return targetCredentials
	}
	return credstore.MergeSharedCredentialFields(targetCredentials, shared)
}

// switchRollback undoes the steps a failed switch took, in reverse.
type switchRollback struct {
	store          *Switcher
	configPath     string
	originalConfig string
	hadConfig      bool
	originalCreds  string

	credsWritten  bool
	configWritten bool
}

// run restores whatever was written.
//
// Failures here are logged, not returned: the caller is already reporting the
// original failure, and a rollback error would replace the cause with a
// symptom. The log line is what a user needs when manual recovery is required.
func (r *switchRollback) run() {
	if r.configWritten && r.hadConfig {
		if err := fsutil.WriteForeignFileAtomic(r.configPath, []byte(r.originalConfig)); err != nil {
			slog.Error("failed to roll back Claude's config after a failed switch", "error", err)
		}
	}
	if r.credsWritten && r.originalCreds != "" {
		if err := r.store.Creds.WriteActive(r.originalCreds); err != nil {
			slog.Error("failed to roll back the live credential after a failed switch", "error", err)
		}
	}
}

func wrapRolledBack(err error) error {
	return fmt.Errorf("%w: the switch failed and was rolled back: %w", apperr.ErrSwitch, err)
}

// stashLive preserves a live credential that belongs to no slot here.
func (s *Switcher) stashLive(credentials, reason, configSlot string, resolved *claudeapi.Identity) (string, error) {
	entry := credstore.StashEntry{
		Reason:      reason,
		ConfigSlot:  configSlot,
		Fingerprint: claudeapi.Fingerprint(credentials),
	}
	if mtime, ok := s.Creds.CredentialsMtime(); ok {
		entry.CredentialsMtime = mtime.Format(TimestampLayout)
	}
	if config, ok := readObjectLenient(s.Paths.GlobalConfigPath()); ok {
		if account, present := config["oauthAccount"]; present {
			entry.LiveOAuthAccount = account
		}
	}
	if resolved != nil {
		if encoded, err := json.Marshal(map[string]string{
			"uuid": resolved.UUID, "email": resolved.Email,
			"organizationUuid": resolved.OrganizationUUID,
		}, json.Deterministic(true)); err == nil {
			entry.ResolvedIdentity = encoded
		}
	}

	entryID, err := s.Creds.WriteUnclaimed(credentials, entry, s.now())
	if err != nil {
		return "", err
	}
	slog.Warn("the live credential does not belong to the slot the config names; it was "+
		"stashed. Something outside aaswap rewrote the live login after the last switch",
		"slot", configSlot, "reason", reason, "entry", entryID,
		"credentials_mtime", entry.CredentialsMtime)
	return entryID, nil
}

// replanNewActive pulls the just-activated account's poll plan to the active
// floor.
//
// Its stored plan was computed while it was an idle candidate and may wait ten
// minutes — far too slow for the account whose usage is about to move. The
// deadline anchors on the last measurement, so an already-old one comes due
// immediately and a never-measured account is left plan-less, blocking nothing.
// The poll is only ever pulled EARLIER, never pushed later.
//
// Best-effort by contract: the switch this rides on has already committed, so a
// hiccup here must never surface as a switch failure.
func (s *Switcher) replanNewActive(num string, roster *Roster) {
	account, ok := roster.Accounts[num]
	if !ok {
		return
	}
	identities := map[string]usagestore.Identity{
		num: {Email: account.Email, OrganizationUUID: account.OrganizationUUID},
	}
	entry, known := s.Usage.Entries(identities, nil)[num]
	if !known || entry.FetchedAt.IsZero() {
		return
	}

	now := s.now()
	nextPoll := entry.FetchedAt.Add(pollpolicy.MinInterval)
	if nextPoll.Before(now) {
		nextPoll = now
	}
	if !entry.NextPollAt.IsZero() && !entry.NextPollAt.After(nextPoll) {
		return
	}
	if err := s.Usage.SetPollPlan(map[string]pollpolicy.Plan{
		num: {NextPollAt: nextPoll, Interval: pollpolicy.MinInterval},
	}, identities); err != nil {
		slog.Warn("the post-switch poll re-plan failed (the switch itself succeeded)", "error", err)
	}
}

func readFileIfExists(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(data), true
}

func displacedSlotLabel(slot string) string {
	if slot == "" {
		return "unmanaged"
	}
	return slot
}

func baseName(path string) string {
	for i := len(path) - 1; i >= 0; i-- {
		if path[i] == '/' || path[i] == '\\' {
			return path[i+1:]
		}
	}
	return path
}
