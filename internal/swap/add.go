package swap

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/usagestore"
)

// AddRequest is one capture of the machine's live login into the store.
type AddRequest struct {
	// Name pins the account's handle. Empty derives one from the address,
	// suffixed when something already holds it.
	//
	// One field where there were two. A slot number and an alias were both
	// ways of saying "this account", and keeping them apart meant every
	// operation had to decide which one it was addressed by.
	Name string
	// AssumeYes skips the confirmation for taking a name something else holds.
	// Callers with their own confirmation UI set it after confirming.
	AssumeYes bool
	// Confirm asks whether to overwrite. Nil means refuse, which is the safe
	// answer for a caller that cannot ask.
	Confirm func(prompt string) bool
}

// AddOutcome reports what an add did.
type AddOutcome struct {
	Name  string
	Email string
	// Tag is the account's organization context, for display.
	Tag string
	// Refreshed marks an in-place credential refresh of an already-registered
	// account rather than a new registration.
	Refreshed bool
	// RenamedFrom names the handle this account previously had, when the add
	// renamed it.
	RenamedFrom string
	// Displaced names what the name previously held, when one was overwritten.
	Displaced string
	// Unverified carries the ownership check's reason for not completing.
	Unverified string
	// Cancelled marks a confirmation the user declined. Nothing was changed.
	Cancelled bool
}

// Add captures the machine's live Claude Code login into a managed slot.
//
// The order is the whole design: everything destructive happens AFTER the
// replacement credential and config are in memory and verified. A capture that
// fails halfway must leave the roster exactly as it found it, because the
// alternative is a slot that names an account whose credential is nowhere.
func (s *Switcher) Add(ctx context.Context, req AddRequest) (AddOutcome, error) {
	var name string
	if req.Name != "" {
		normalized, err := NormalizeName(req.Name)
		if err != nil {
			return AddOutcome{}, err
		}
		name = normalized
	}

	identity, ok := s.LiveIdentity()
	if !ok {
		return AddOutcome{}, s.noLiveAccount()
	}

	var outcome AddOutcome
	err := s.withLock(func() error {
		roster, err := s.RosterOrEmpty()
		if err != nil {
			return err
		}
		outcome, err = s.add(ctx, roster, req, name, identity)
		return err
	})
	return outcome, err
}

func (s *Switcher) add(ctx context.Context, roster *Roster, req AddRequest, name string, identity LiveIdentity) (AddOutcome, error) {
	existing, registered := roster.FindName(identity.Identity())

	// No name given and the account is already registered: refresh in place.
	// The alternative — inventing a second name — would leave two entries
	// holding one account, and the older one holding a credential the server
	// has retired.
	if name == "" && registered {
		return s.refreshInPlace(ctx, roster, existing, identity)
	}

	num, plan, err := s.planName(roster, req, name, identity, existing, registered)
	if err != nil || plan.cancelled {
		return AddOutcome{Cancelled: plan.cancelled}, err
	}

	captured, err := s.captureVerified(ctx, identity)
	if err != nil {
		return AddOutcome{}, err
	}
	config, err := s.ReadLiveConfig()
	if err != nil {
		return AddOutcome{}, err
	}
	// Last, because it licenses the bytes just read. Ahead of the read, a
	// /login landing in between would store a config the check never saw.
	if err := s.RejectIdentityDrift(identity); err != nil {
		return AddOutcome{}, err
	}

	// Everything below is destructive, and everything it needs is in memory.
	if plan.displace != "" {
		if err := s.forget(roster, plan.displace); err != nil {
			return AddOutcome{}, err
		}
	}
	if plan.migrateFrom != "" {
		if err := s.forget(roster, plan.migrateFrom); err != nil {
			return AddOutcome{}, err
		}
	}

	if err := s.storeCapture(num, identity, captured.Credentials, config); err != nil {
		return AddOutcome{}, err
	}

	roster.Insert(num, &Account{
		Email:            identity.Email,
		UUID:             identity.AccountUUID,
		OrganizationUUID: identity.OrganizationUUID,
		OrganizationName: identity.OrganizationName,
		Added:            Timestamp(s.now()),
		// Load-bearing for a provider with no address to store: the digest is
		// then the only thing identifying this account, and an empty one would
		// compare equal to every other identityless row.
		Fingerprint: identity.Fingerprint,
	})
	roster.SetActive(num)
	if err := s.WriteRoster(roster); err != nil {
		return AddOutcome{}, err
	}

	return AddOutcome{
		Name: num, Email: identity.Email, Tag: identity.DisplayTag(),
		RenamedFrom: plan.migrateFrom, Displaced: plan.displace,
		Unverified: captured.Unverified,
	}, nil
}

// refreshInPlace re-captures the credential for an account already in the
// store, leaving its name and added date alone.
func (s *Switcher) refreshInPlace(ctx context.Context, roster *Roster, name string, identity LiveIdentity) (AddOutcome, error) {
	account := roster.Accounts[name]

	captured, err := s.captureVerified(ctx, identity)
	if err != nil {
		return AddOutcome{}, err
	}
	config, err := s.ReadLiveConfig()
	if err != nil {
		return AddOutcome{}, err
	}
	if err := s.RejectIdentityDrift(identity); err != nil {
		return AddOutcome{}, err
	}

	if err := s.storeCapture(name, identity, captured.Credentials, config); err != nil {
		return AddOutcome{}, err
	}
	// The stored generation moved, so the record of which one it is has to move
	// with it — otherwise the next read compares the new credential against the
	// digest of the one it replaced and reports a login that never happened.
	account.Fingerprint = identity.Fingerprint
	roster.SetActive(name)
	if err := s.WriteRoster(roster); err != nil {
		return AddOutcome{}, err
	}

	return AddOutcome{
		Name: name, Email: identity.Email, Refreshed: true,
		Tag: displayTag(account.OrganizationName), Unverified: captured.Unverified,
	}, nil
}

// namePlan is what an add decided about placement before anything destructive
// ran.
type namePlan struct {
	displace    string
	migrateFrom string
	cancelled   bool
}

// planName decides what the capture is called and collects every confirmation,
// before a single destructive step.
//
// One decision where there used to be two. A slot number said where an account
// lived and an alias said what it was called, and every path had to keep them
// agreeing; now the name is both, so the only question left is whether
// something already holds it.
func (s *Switcher) planName(roster *Roster, req AddRequest, name string, identity LiveIdentity, existing string, registered bool) (string, namePlan, error) {
	var plan namePlan

	if name == "" {
		return roster.NameFor(identity.Email), plan, nil
	}

	if registered && existing != name {
		plan.migrateFrom = existing
	}

	if occupant, taken := roster.Accounts[name]; taken {
		if occupant.Identity() != identity.Identity() {
			prompt := fmt.Sprintf("%q is %s [%s]. Overwrite it?",
				name, occupant.Email, occupant.DisplayTag())
			if !req.AssumeYes {
				if req.Confirm == nil || !req.Confirm(prompt) {
					plan.cancelled = true
					return "", plan, nil
				}
			}
			plan.displace = name
		}
	}
	return name, plan, nil
}

// captureVerified reads the live credential and checks it belongs to the
// identity naming it.
func (s *Switcher) captureVerified(ctx context.Context, identity LiveIdentity) (CaptureResult, error) {
	credentials, err := s.ReadCaptureCredentials()
	if err != nil {
		return CaptureResult{}, err
	}
	if credentials == "" {
		return CaptureResult{}, fmt.Errorf("%w: no credentials found for the current account",
			apperr.ErrCredentialRead)
	}
	if err := RejectLiveAPIKeyCapture(credentials); err != nil {
		return CaptureResult{}, err
	}
	captured, err := s.VerifyCredentialOwnership(ctx, credentials, identity)
	if err != nil {
		return CaptureResult{}, err
	}
	if err := s.RejectCredentialDrift(captured.Credentials); err != nil {
		return CaptureResult{}, err
	}
	return captured, nil
}

// noLiveAccount explains a live login that resolved to no account.
//
// Two states look the same to the resolver and need opposite advice. Nothing
// is logged in: log in. Something is, but it carries no account — a Codex
// install signed in with an API key has an auth.json and no id_token in it —
// and telling that person to "log in first" tells someone who is logged in to
// log in. The credential file's presence is what tells the two apart.
func (s *Switcher) noLiveAccount() error {
	spec := s.spec()
	// Only where the credential IS the identity document. Where the identity
	// lives in a config beside the credential — Claude — a credential with no
	// identity is a config that has not been written yet, and "log in first"
	// is exactly right.
	if _, separateConfig := spec.ConfigFile(); !separateConfig {
		live := s.readLiveFiles(spec)
		for _, secret := range spec.SecretFiles() {
			if strings.TrimSpace(live[secret.Path]) != "" {
				return fmt.Errorf("%w: %s is logged in, but the login carries no account "+
					"identity — an API key rather than an account, most likely. aaswap "+
					"manages accounts, so there is nothing here to store",
					apperr.ErrConfig, spec.DisplayName())
			}
		}
	}
	return fmt.Errorf("%w: no active %s account found. Log in first",
		apperr.ErrConfig, spec.DisplayName())
}

// ReadLiveConfig reads Claude Code's config verbatim, for storage alongside the
// credential.
//
// Read as bytes rather than parsed and re-serialized: this is the user's file,
// and a slot must restore exactly what was captured.
func (s *Switcher) ReadLiveConfig() (string, error) {
	spec := s.spec()
	config, ok := s.configFileFor(spec)
	if !ok {
		// This provider declares no account-scoped config to capture. Its
		// credential carries whatever identity it has, and anything else in its
		// home is machine-scoped — swapping that would carry one account's
		// model choice onto another.
		//
		// Empty, and the layers above ask the declaration rather than reading a
		// placeholder. This used to return "{}" so that "has a stored config"
		// would keep standing in for "is switchable" — which meant the answer
		// was carried by a file written on every capture and read by nothing, and
		// anything that failed to write it produced accounts that reported as
		// unswitchable and refused to export.
		return "", nil
	}
	data, err := os.ReadFile(config)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s's config file was not found",
				apperr.ErrConfig, spec.Name)
		}
		return "", fmt.Errorf("%w: reading %s's config: %w",
			apperr.ErrConfig, spec.Name, err)
	}
	return string(data), nil
}

// storeCapture writes a slot's credential and config, and lifts any dead-token
// quarantine, since the credential it was issued against has just been
// replaced.
func (s *Switcher) storeCapture(num string, identity LiveIdentity, credentials, config string) error {
	if err := s.Creds.WriteAccount(num, identity.Email, credentials); err != nil {
		return err
	}
	s.BackupWritten(num, identity.Email)
	if err := s.WriteAccountConfig(num, identity.Email, config); err != nil {
		return err
	}
	return s.Usage.ClearDeadToken([]string{num}, map[string]usagestore.Identity{
		num: {Email: identity.Email, OrganizationUUID: identity.OrganizationUUID},
	})
}

// copyStored duplicates an account's stored material under a new name.
//
// A copy, not a move. Until the roster names the new location the old one is
// still the truth, and a move would leave a window where neither is.
func (s *Switcher) copyStored(from, to, email string) error {
	return s.copyStoredFrom(s.Creds, s.Creds, s.ConfigsDir(), from, to, email)
}

// copyStoredFrom is copyStored between two given stores, so the upgrade can read
// from the pre-provider layout and write where the accounts belong.
//
// Both ends are explicit because the upgrade's are neither of this switcher's:
// it reads the unscoped layout, and it writes Claude's — a version 1 store holds
// only Claude accounts, whichever provider the command that triggered it
// addressed.
func (s *Switcher) copyStoredFrom(source, target *credstore.Store, configsDir, from, to, email string) error {
	credentials, unreadable := source.ReadAccount(from, email)
	if unreadable {
		return fmt.Errorf("%w: %s's stored credential could not be read, so it "+
			"cannot be moved; nothing was changed", apperr.ErrCredentialRead, from)
	}
	if credentials != "" {
		if err := target.WriteAccount(to, email, credentials); err != nil {
			return err
		}
		s.BackupWritten(to, email)
	}
	config := s.readConfigIn(configsDir, from, email)
	if config == "" && source != target {
		// The upgrade's source: the config sits in the pre-provider layout too.
		config = s.readLegacyConfig(from, email)
	}
	if config != "" {
		if err := s.writeConfigIn(configsDir, to, email, config); err != nil {
			return err
		}
	}
	return nil
}

// dropStored removes material the roster no longer names. Best effort: it runs
// after the roster has been published, so a failure costs disk rather than
// correctness.
func (s *Switcher) dropStored(name, email string) {
	s.dropStoredFrom(s.Creds, name, email, false)
}

// dropStoredFrom is dropStored against a given store, so the upgrade can clear
// the pre-provider layout it read from.
func (s *Switcher) dropStoredFrom(store *credstore.Store, name, email string, legacyConfig bool) {
	if err := store.DeleteAccount(name, email); err != nil {
		slog.Debug("could not remove a superseded credential", "name", name, "error", err)
	}
	path := s.ConfigBackupPath(name, email)
	if legacyConfig {
		path = filepath.Join(s.legacyConfigsDir(), filepath.Base(path))
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		slog.Debug("could not remove a superseded config", "name", name, "error", err)
	}
}

// forget removes a slot's stored material and its roster record.
func (s *Switcher) forget(roster *Roster, num string) error {
	account, ok := roster.Accounts[num]
	if !ok {
		return nil
	}
	if err := s.deleteAccountFiles(num, account.Email); err != nil {
		return err
	}
	roster.Remove(num)
	return nil
}

// deleteAccountFiles removes a slot's credential backup and config backup.
func (s *Switcher) deleteAccountFiles(num, email string) error {
	if err := s.Creds.DeleteAccount(num, email); err != nil {
		return err
	}
	if err := os.Remove(s.ConfigBackupPath(num, email)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("%w: removing the stored config for account %s: %w",
			apperr.ErrConfig, num, err)
	}
	return nil
}
