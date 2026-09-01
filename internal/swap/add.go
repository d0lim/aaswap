package swap

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/d0lim/aaswap/internal/apperr"
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
		return AddOutcome{}, fmt.Errorf("%w: no active Claude account found. Log in first",
			apperr.ErrConfig)
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

// ReadLiveConfig reads Claude Code's config verbatim, for storage alongside the
// credential.
//
// Read as bytes rather than parsed and re-serialized: this is the user's file,
// and a slot must restore exactly what was captured.
func (s *Switcher) ReadLiveConfig() (string, error) {
	data, err := os.ReadFile(s.Paths.GlobalConfigPath())
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: Claude's config file was not found", apperr.ErrConfig)
		}
		return "", fmt.Errorf("%w: reading Claude's config: %w", apperr.ErrConfig, err)
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
	credentials, unreadable := s.Creds.ReadAccount(from, email)
	if unreadable {
		return fmt.Errorf("%w: %s's stored credential could not be read, so it "+
			"cannot be moved; nothing was changed", apperr.ErrCredentialRead, from)
	}
	if credentials != "" {
		if err := s.Creds.WriteAccount(to, email, credentials); err != nil {
			return err
		}
		s.BackupWritten(to, email)
	}
	if config := s.ReadAccountConfig(from, email); config != "" {
		if err := s.WriteAccountConfig(to, email, config); err != nil {
			return err
		}
	}
	return nil
}

// dropStored removes material the roster no longer names. Best effort: it runs
// after the roster has been published, so a failure costs disk rather than
// correctness.
func (s *Switcher) dropStored(name, email string) {
	if err := s.deleteAccountFiles(name, email); err != nil {
		slog.Debug("could not remove the material left behind by a rename",
			"name", name, "error", err)
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
