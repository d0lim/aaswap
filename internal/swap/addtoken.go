package swap

import (
	json "encoding/json/v2"
	"fmt"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/usagestore"
)

// setupTokenScopes are what a setup token carries. Recorded so the stored
// credential has the shape Claude Code expects, even though nothing here
// verifies it.
var setupTokenScopes = []string{"user:inference"}

// AddTokenRequest registers a raw token as an account.
type AddTokenRequest struct {
	// Token is a managed API key or an OAuth setup token. The kind is detected
	// from the value; nothing is sent anywhere to find out.
	Token string
	// Email labels the account. Empty synthesizes a placeholder, because these
	// tokens carry no address of their own and making the user invent one is
	// noise.
	Email string
	// Slot pins the destination. Zero auto-assigns.
	Slot int
	// AssumeYes skips the confirmation for overwriting an occupied slot.
	AssumeYes bool
	// Confirm asks whether to overwrite. Nil means refuse.
	Confirm func(prompt string) bool
}

// AddToken registers a raw OAuth setup token or a managed API key as an
// account, with no network call at all.
//
// For a headless machine, or a token handed over from somewhere else: there is
// no prior Claude Code login on this machine to capture.
func (s *Switcher) AddToken(req AddTokenRequest) (AddOutcome, error) {
	token := strings.TrimSpace(req.Token)
	if token == "" {
		return AddOutcome{}, fmt.Errorf("%w: the token cannot be empty", apperr.ErrValidation)
	}
	isAPIKey := credstore.LooksLikeAPIKey(token)

	if req.Email != "" && !validEmail(req.Email) {
		return AddOutcome{}, fmt.Errorf("%w: %q is not a valid email address",
			apperr.ErrValidation, req.Email)
	}

	var outcome AddOutcome
	err := s.withLock(func() error {
		roster, err := s.RosterOrEmpty()
		if err != nil {
			return err
		}

		slot := req.Slot
		email := req.Email
		if email == "" {
			if slot == 0 {
				slot = roster.NextNumber()
			}
			// The slot number gives every synthesized account a unique key, and
			// the label says at a glance what kind of token it is.
			label := "setup-token"
			if isAPIKey {
				label = "api-key"
			}
			email = fmt.Sprintf("%s-%d@token.local", label, slot)
		}

		// Identity is the (email, organization) composite alone, so an API-key
		// and an OAuth account sharing an address could not be told apart at
		// switch time. Refuse rather than silently convert one into the other.
		if err := s.RejectCrossKindCollision(roster, email, isAPIKey); err != nil {
			return err
		}

		credentials, config, err := tokenMaterial(token, email, isAPIKey)
		if err != nil {
			return err
		}

		identity := Identity{Email: email}
		if existing, registered := roster.FindSlot(identity); registered && req.Slot == 0 {
			// Refresh in place: a new token for an account already here.
			if err := s.storeToken(roster, existing, email, credentials, config, isAPIKey); err != nil {
				return err
			}
			roster.LastUpdated = Timestamp(s.now())
			outcome = AddOutcome{
				Number: existing, Email: email, Tag: "personal", Refreshed: true,
			}
			return s.WriteRoster(roster)
		}

		num, plan, err := s.planTokenSlot(roster, req, slot, email, identity)
		if err != nil || plan.cancelled {
			outcome = AddOutcome{Cancelled: plan.cancelled}
			return err
		}

		if plan.displace != "" {
			if err := s.forget(roster, plan.displace); err != nil {
				return err
			}
		}
		if plan.migrateFrom != "" {
			if err := s.forget(roster, plan.migrateFrom); err != nil {
				return err
			}
		}
		if err := s.storeToken(roster, num, email, credentials, config, isAPIKey); err != nil {
			return err
		}

		record := &Account{Email: email, Added: Timestamp(s.now()), Alias: plan.alias}
		if isAPIKey {
			record.Kind = KindAPIKey
		}
		roster.Insert(num, record, s.now())
		outcome = AddOutcome{
			Number: num, Email: email, Tag: "personal",
			MovedFrom: plan.migrateFrom, Displaced: plan.displace,
		}
		return s.WriteRoster(roster)
	})
	return outcome, err
}

// planTokenSlot decides where a token lands and collects any confirmation.
func (s *Switcher) planTokenSlot(roster *Roster, req AddTokenRequest, slot int, email string, identity Identity) (string, slotPlan, error) {
	var plan slotPlan
	if slot == 0 {
		return fmt.Sprint(roster.NextNumber()), plan, nil
	}
	if slot < 1 {
		return "", plan, fmt.Errorf("%w: a slot number must be 1 or greater", apperr.ErrConfig)
	}
	num := fmt.Sprint(slot)

	if existing, registered := roster.FindSlot(identity); registered && existing != num {
		plan.migrateFrom = existing
		plan.alias = roster.Accounts[existing].Alias
	}
	if occupant, taken := roster.Accounts[num]; taken {
		if occupant.Identity() == identity {
			plan.alias = occupant.Alias
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
	return num, plan, nil
}

// storeToken writes a token account's material and lifts any quarantine.
func (s *Switcher) storeToken(roster *Roster, num, email, credentials, config string, isAPIKey bool) error {
	if err := s.Creds.WriteAccount(num, email, credentials); err != nil {
		return err
	}
	s.BackupWritten(num, email)
	if err := s.WriteAccountConfig(num, email, config); err != nil {
		return err
	}
	if account, exists := roster.Accounts[num]; exists {
		if isAPIKey {
			account.Kind = KindAPIKey
		} else {
			account.Kind = ""
		}
	}
	// A replaced credential makes any dead-token verdict obsolete; leaving it
	// would keep the account stuck at "re-login needed" and it would never
	// fetch to prove the new token good.
	return s.Usage.ClearDeadToken([]string{num},
		map[string]usagestore.Identity{num: {Email: email}})
}

// tokenMaterial builds the stored credential and config for a raw token.
//
// A managed key is stored RAW, because that is what Claude Code's API-key axis
// reads. A setup token is wrapped in the credential shape its OAuth axis reads.
// The synthesized config is the same either way: neither token carries real
// organization metadata.
func tokenMaterial(token, email string, isAPIKey bool) (credentials, config string, err error) {
	if isAPIKey {
		credentials = token
	} else {
		encoded, marshalErr := json.Marshal(map[string]any{
			"claudeAiOauth": map[string]any{
				"accessToken": token,
				"scopes":      setupTokenScopes,
			},
		}, json.Deterministic(true))
		if marshalErr != nil {
			return "", "", fmt.Errorf("%w: encoding the token credential: %w",
				apperr.ErrCredentialWrite, marshalErr)
		}
		credentials = string(encoded)
	}

	encodedConfig, err := json.Marshal(map[string]any{
		"oauthAccount": map[string]any{
			"emailAddress":     email,
			"accountUuid":      "",
			"organizationUuid": nil,
			"organizationName": nil,
		},
	}, json.Deterministic(true))
	if err != nil {
		return "", "", fmt.Errorf("%w: encoding the token config: %w", apperr.ErrConfig, err)
	}
	return credentials, string(encodedConfig), nil
}

// validEmail is the same shape the import path enforces: the address becomes
// part of a filename, so it is constrained before any path is built from it.
func validEmail(email string) bool {
	if strings.ContainsAny(email, " \t\n/\\:*?\"<>|") {
		return false
	}
	local, domain, found := strings.Cut(email, "@")
	return found && local != "" && strings.Contains(domain, ".") &&
		!strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}
