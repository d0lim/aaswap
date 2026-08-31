package swap

import (
	"context"
	"fmt"
	"os"

	"github.com/realiti4/claude-swap/internal/apperr"
	"github.com/realiti4/claude-swap/internal/usagestore"
)

// AddRequest is one capture of the machine's live login into a slot.
type AddRequest struct {
	// Slot pins the destination. Zero auto-assigns the next number.
	Slot int
	// Alias names the slot. Empty leaves any existing alias alone — which is
	// what makes re-running `cswap add` on a registered account a credential
	// refresh rather than a rename.
	Alias string
	// AssumeYes skips the confirmation for overwriting an occupied slot.
	// Callers with their own confirmation UI set it after confirming.
	AssumeYes bool
	// Confirm asks whether to overwrite an occupied slot. Nil means refuse,
	// which is the safe answer for a caller that cannot ask.
	Confirm func(prompt string) bool
}

// AddOutcome reports what an add did.
type AddOutcome struct {
	Number string
	Email  string
	// Tag is the account's organization context, for display.
	Tag string
	// Refreshed marks an in-place credential refresh of an already-registered
	// account rather than a new registration.
	Refreshed bool
	// MovedFrom names the slot this account previously occupied, when the add
	// relocated it.
	MovedFrom string
	// Displaced names the slot's previous occupant, when one was overwritten.
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
	var alias string
	if req.Alias != "" {
		normalized, err := NormalizeAlias(req.Alias)
		if err != nil {
			return AddOutcome{}, err
		}
		alias = normalized
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
		outcome, err = s.add(ctx, roster, req, alias, identity)
		return err
	})
	return outcome, err
}

func (s *Switcher) add(ctx context.Context, roster *Roster, req AddRequest, alias string, identity LiveIdentity) (AddOutcome, error) {
	existing, registered := roster.FindSlot(identity.Identity())

	// No slot named and the account is already registered: refresh in place.
	// The alternative — assigning a new number — would leave two slots holding
	// one account, and the older one holding a credential the server has
	// retired.
	if req.Slot == 0 && registered {
		return s.refreshInPlace(ctx, roster, existing, alias, identity)
	}

	num, plan, err := s.planSlot(roster, req, alias, identity, existing, registered)
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
		Alias:            plan.alias,
	}, s.now())
	roster.SetActive(num, s.now())
	if err := s.WriteRoster(roster); err != nil {
		return AddOutcome{}, err
	}

	return AddOutcome{
		Number: num, Email: identity.Email, Tag: identity.DisplayTag(),
		MovedFrom: plan.migrateFrom, Displaced: plan.displace,
		Unverified: captured.Unverified,
	}, nil
}

// refreshInPlace re-captures the credential for an account already in the
// roster, leaving its slot, added date and — unless one was given — its alias
// alone.
func (s *Switcher) refreshInPlace(ctx context.Context, roster *Roster, num, alias string, identity LiveIdentity) (AddOutcome, error) {
	account := roster.Accounts[num]
	if alias != "" {
		if conflict, inUse := AliasInUse(roster, alias, num); inUse {
			return AddOutcome{}, fmt.Errorf("%w: alias %q is already used by account %s",
				apperr.ErrValidation, alias, conflict)
		}
	}

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

	if err := s.storeCapture(num, identity, captured.Credentials, config); err != nil {
		return AddOutcome{}, err
	}
	if alias != "" {
		account.Alias = alias
	}
	roster.SetActive(num, s.now())
	if err := s.WriteRoster(roster); err != nil {
		return AddOutcome{}, err
	}

	return AddOutcome{
		Number: num, Email: identity.Email, Refreshed: true,
		Tag: displayTag(account.OrganizationName), Unverified: captured.Unverified,
	}, nil
}

// slotPlan is what an add decided about placement before anything destructive
// ran.
type slotPlan struct {
	alias       string
	displace    string
	migrateFrom string
	cancelled   bool
}

// planSlot decides where the capture lands and collects every confirmation,
// before a single destructive step.
func (s *Switcher) planSlot(roster *Roster, req AddRequest, alias string, identity LiveIdentity, existing string, registered bool) (string, slotPlan, error) {
	plan := slotPlan{alias: alias}

	if req.Slot == 0 {
		num := fmt.Sprint(roster.NextNumber())
		if alias != "" {
			if conflict, inUse := AliasInUse(roster, alias, num); inUse {
				return "", plan, fmt.Errorf("%w: alias %q is already used by account %s",
					apperr.ErrValidation, alias, conflict)
			}
		}
		return num, plan, nil
	}

	if req.Slot < 1 {
		return "", plan, fmt.Errorf("%w: a slot number must be 1 or greater", apperr.ErrConfig)
	}
	num := fmt.Sprint(req.Slot)

	if registered && existing != num {
		plan.migrateFrom = existing
	}

	if occupant, taken := roster.Accounts[num]; taken {
		if occupant.Identity() == identity.Identity() {
			// The same account, re-captured into the slot it already holds.
			// Its alias carries forward unless one was given.
			if alias == "" {
				plan.alias = occupant.Alias
			}
		} else {
			prompt := fmt.Sprintf("Slot %s is occupied by %s [%s]. Overwrite it?",
				num, occupant.Email, occupant.DisplayTag())
			if !req.AssumeYes {
				if req.Confirm == nil || !req.Confirm(prompt) {
					plan.cancelled = true
					return "", plan, nil
				}
			}
			plan.displace = num
		}
	}
	if plan.migrateFrom != "" && plan.alias == "" {
		// The account is moving slots; its alias moves with it.
		plan.alias = roster.Accounts[plan.migrateFrom].Alias
	}

	if plan.alias != "" {
		if conflict, inUse := AliasInUse(roster, plan.alias, num); inUse && conflict != plan.migrateFrom {
			return "", plan, fmt.Errorf("%w: alias %q is already used by account %s",
				apperr.ErrValidation, plan.alias, conflict)
		}
	}
	return num, plan, nil
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
	if err := s.WriteAccountConfig(num, identity.Email, config); err != nil {
		return err
	}
	return s.Usage.ClearDeadToken([]string{num}, map[string]usagestore.Identity{
		num: {Email: identity.Email, OrganizationUUID: identity.OrganizationUUID},
	})
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
	roster.Remove(num, s.now())
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
