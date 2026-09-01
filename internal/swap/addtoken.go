package swap

import (
	"fmt"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	providerpkg "github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/usagestore"
)

// AddTokenRequest registers a raw token as an account.
type AddTokenRequest struct {
	// Token is a managed API key or an OAuth setup token. The kind is detected
	// from the value; nothing is sent anywhere to find out.
	Token string
	// Email labels the account. Empty synthesizes a placeholder, because these
	// tokens carry no address of their own and making the user invent one is
	// noise.
	Email string
	// Name pins the account's handle. Empty derives one: from the address when
	// one was given, and from the token's kind when one was not.
	Name string
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
	// Asked of the declaration before anything is read or written. A token's
	// FORMAT is the one part of a login aaswap has to understand — recognising
	// it, and writing it in the shape the tool reads — and there is no generic
	// version of that to fall back on. Storing Claude's shape for another
	// provider produces an account whose activation destroys a working login.
	source := s.spec().Token
	if source == nil {
		return AddOutcome{}, fmt.Errorf("%w: %s",
			apperr.ErrValidation, s.spec().Why(providerpkg.CapToken))
	}

	token := strings.TrimSpace(req.Token)
	if token == "" {
		return AddOutcome{}, fmt.Errorf("%w: the token cannot be empty", apperr.ErrValidation)
	}
	isAPIKey := source.APIKey(token)

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

		name := req.Name
		if name != "" {
			normalized, err := NormalizeName(name)
			if err != nil {
				return err
			}
			name = normalized
		}

		email := req.Email
		if email == "" {
			// No address to derive from, so the kind supplies the handle and
			// the handle supplies the address. Deriving in that order — rather
			// than numbering the label as slots used to — keeps the two from
			// disagreeing, because one is built out of the other.
			if name == "" {
				label := "setup-token"
				if isAPIKey {
					label = "api-key"
				}
				name = uniqueName(label, roster.TakenNames())
			}
			email = name + "@token.local"
		} else if name == "" {
			name = roster.NameFor(email)
		}

		// Identity is the (email, organization) composite alone, so an API-key
		// and an OAuth account sharing an address could not be told apart at
		// switch time. Refuse rather than silently convert one into the other.
		if err := s.RejectCrossKindCollision(roster, email, isAPIKey); err != nil {
			return err
		}

		credentials, config, err := source.Material(token, email)
		if err != nil {
			return fmt.Errorf("%w: %w", apperr.ErrCredentialWrite, err)
		}

		identity := Identity{Email: email}
		if existing, registered := roster.FindName(identity); registered && req.Name == "" {
			// Refresh in place: a new token for an account already here.
			if err := s.storeToken(roster, existing, email, credentials, config, isAPIKey); err != nil {
				return err
			}
			outcome = AddOutcome{
				Name: existing, Email: email, Tag: "personal", Refreshed: true,
			}
			return s.WriteRoster(roster)
		}

		num, plan, err := s.planTokenName(roster, req, name, identity)
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

		record := &Account{Email: email, Added: Timestamp(s.now())}
		if isAPIKey {
			record.Kind = KindAPIKey
		}
		roster.Insert(num, record)
		outcome = AddOutcome{
			Name: num, Email: email, Tag: "personal",
			RenamedFrom: plan.migrateFrom, Displaced: plan.displace,
		}
		return s.WriteRoster(roster)
	})
	return outcome, err
}

// planTokenName decides what a token account is called and collects any
// confirmation.
func (s *Switcher) planTokenName(roster *Roster, req AddTokenRequest, name string, identity Identity) (string, namePlan, error) {
	var plan namePlan

	if existing, registered := roster.FindName(identity); registered && existing != name {
		plan.migrateFrom = existing
	}
	if occupant, taken := roster.Accounts[name]; taken && occupant.Identity() != identity {
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
	return name, plan, nil
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
