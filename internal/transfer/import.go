package transfer

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"regexp"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/swap"
	"github.com/d0lim/aaswap/internal/usagestore"
)

// emailPattern is what an imported address must look like.
//
// The address flows into a filename, so it is constrained BEFORE any path is
// built from it: an envelope is a file from another machine, and a slash or a
// parent reference in an address would write wherever it pointed.
var emailPattern = regexp.MustCompile(`^[^\s/\\:*?"<>|]+@[^\s/\\:*?"<>|]+\.[^\s/\\:*?"<>|]+$`)

// ImportOutcome is what happened to one account.
type ImportOutcome string

const (
	// Imported means the account was new here.
	Imported ImportOutcome = "imported"
	// Overwrote means an existing account was replaced because --force said so.
	Overwrote ImportOutcome = "overwrote"
	// Replaced means an existing account was replaced WITHOUT --force, because
	// its stored credential was quarantined as dead. A plain import heals that:
	// the quarantine normally postdates the slot's last credential write, so
	// the import is newer than what failed.
	Replaced ImportOutcome = "replaced"
	// Skipped means an existing healthy account was left alone.
	Skipped ImportOutcome = "skipped"
)

// ImportedAccount reports one account's fate.
type ImportedAccount struct {
	Name    string
	Email   string
	Outcome ImportOutcome
	// Notes carry anything the user needs to know about this account —
	// a dropped alias, a lifted strike, a live session still on old
	// credentials.
	Notes []string
}

// ImportResult reports the whole import.
type ImportResult struct {
	Accounts []ImportedAccount
	// ActiveSlot is where the envelope's active account landed here, empty when
	// the envelope named none or it was skipped.
	ActiveSlot string
}

// Import restores accounts from an envelope.
//
// Two passes, and the split is the point: EVERY account is validated before
// ANY is written. A malformed account late in the file must not leave the
// earlier ones half-imported, and validation failures say something about the
// file while write failures say something about the machine.
func Import(s *swap.Switcher, data []byte, force bool) (ImportResult, error) {
	envelope, err := parseEnvelope(data)
	if err != nil {
		return ImportResult{}, err
	}

	roster, err := s.RosterOrEmpty()
	if err != nil {
		return ImportResult{}, err
	}
	entries, err := validateAll(envelope, roster)
	if err != nil {
		return ImportResult{}, err
	}

	var result ImportResult
	for _, entry := range entries {
		// Re-read the roster each time, so per-account writes see the ones
		// before them and two accounts cannot be allocated the same slot.
		roster, err := s.RosterOrEmpty()
		if err != nil {
			return result, err
		}
		account, err := importOne(s, roster, entry, force)
		if err != nil {
			return result, err
		}
		result.Accounts = append(result.Accounts, account)
		if envelope.ActiveAccount != "" && entry.Name == envelope.ActiveAccount {
			result.ActiveSlot = account.Name
		}
	}
	return result, nil
}

// parseEnvelope decodes and sanity-checks the file itself.
func parseEnvelope(data []byte) (Envelope, error) {
	var envelope Envelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return Envelope{}, fmt.Errorf("%w: the export file is not valid JSON: %w",
			apperr.ErrTransfer, err)
	}
	if envelope.Version != FormatVersion {
		return Envelope{}, fmt.Errorf("%w: unsupported export version %d (this build reads "+
			"version %d)", apperr.ErrTransfer, envelope.Version, FormatVersion)
	}
	if envelope.Encrypted {
		return Envelope{}, fmt.Errorf("%w: this export is marked encrypted. Decrypt it "+
			"before importing — for example: gpg -d accounts.gpg | aaswap import -",
			apperr.ErrTransfer)
	}
	if len(envelope.Accounts) == 0 {
		return Envelope{}, fmt.Errorf("%w: the export file has no accounts to import",
			apperr.ErrTransfer)
	}
	return envelope, nil
}

// validated is one account, checked and normalized.
type validated struct {
	Name             string
	Email            string
	UUID             string
	OrganizationUUID string
	OrganizationName string
	Added            string
	Kind             swap.Kind
	Credentials      string
	Config           string
}

// validateAll checks every account before any is written.
func validateAll(envelope Envelope, roster *swap.Roster) ([]validated, error) {
	// A name already used here belongs to the local account that has it.
	localNames := map[string]swap.Identity{}
	for _, name := range roster.Names() {
		localNames[name] = roster.Accounts[name].Identity()
	}

	seenIdentities := map[swap.Identity]bool{}
	seenNames := map[string]bool{}
	out := make([]validated, 0, len(envelope.Accounts))

	for _, raw := range envelope.Accounts {
		entry, err := validateOne(raw)
		if err != nil {
			return nil, err
		}

		identity := swap.Identity{Email: entry.Email, OrganizationUUID: entry.OrganizationUUID}
		if seenIdentities[identity] {
			return nil, fmt.Errorf("%w: the export names %s twice (organization %s)",
				apperr.ErrTransfer, entry.Email, orgLabel(entry.OrganizationUUID))
		}
		seenIdentities[identity] = true

		if entry.Name != "" {
			if seenNames[entry.Name] {
				return nil, fmt.Errorf("%w: the export uses the alias %q twice",
					apperr.ErrTransfer, entry.Name)
			}
			seenNames[entry.Name] = true
			// An alias already held by a DIFFERENT local account is dropped
			// rather than refused: the accounts are what the user asked to
			// import, and a name collision is not worth failing the transfer.
			if owner, taken := localNames[entry.Name]; taken && owner != identity {
				entry.Name = ""
			}
		}
		out = append(out, entry)
	}
	return out, nil
}

// validateOne checks one account's fields BEFORE any filename is built from
// them.
func validateOne(raw ExportedAccount) (validated, error) {
	if !emailPattern.MatchString(raw.Email) {
		return validated{}, fmt.Errorf("%w: an imported account has an invalid or missing "+
			"email address: %q", apperr.ErrTransfer, raw.Email)
	}

	entry := validated{
		Email:            raw.Email,
		UUID:             raw.UUID,
		OrganizationUUID: raw.OrganizationUUID,
		OrganizationName: raw.OrganizationName,
		Added:            raw.Added,
		Kind:             swap.KindOAuth,
	}

	// A name the local rules refuse is dropped rather than refused: an import
	// that fails over a label would strand the credential it carried.
	if raw.Name != "" {
		if name, err := swap.NormalizeName(raw.Name); err == nil {
			entry.Name = name
		}
	}
	if entry.Name == "" {
		entry.Name = swap.NameForEmail(raw.Email)
	}

	// The config must be an object carrying an identity, or a switch to this
	// account could never complete.
	var config map[string]jsontext.Value
	if err := json.Unmarshal(raw.Config, &config); err != nil || config == nil {
		return validated{}, fmt.Errorf("%w: the config for %s must be a JSON object",
			apperr.ErrTransfer, raw.Email)
	}
	if account, present := config["oauthAccount"]; !present || string(account) == "null" {
		return validated{}, fmt.Errorf("%w: the config for %s carries no account identity",
			apperr.ErrTransfer, raw.Email)
	}
	formatted, err := json.Marshal(config, jsontext.WithIndent("  "), json.Deterministic(true))
	if err != nil {
		return validated{}, fmt.Errorf("%w: encoding the config for %s: %w",
			apperr.ErrTransfer, raw.Email, err)
	}
	entry.Config = string(formatted)

	// An API-key account carries a raw string; an OAuth one carries an object.
	var asString string
	if err := json.Unmarshal(raw.Credentials, &asString); err == nil {
		if raw.Kind != string(swap.KindAPIKey) || !credstore.LooksLikeAPIKey(asString) {
			return validated{}, fmt.Errorf("%w: the credential for %s is a bare string but "+
				"is not a managed API key", apperr.ErrTransfer, raw.Email)
		}
		entry.Kind = swap.KindAPIKey
		entry.Credentials = strings.TrimSpace(asString)
		return entry, nil
	}
	if raw.Kind == string(swap.KindAPIKey) {
		return validated{}, fmt.Errorf("%w: the credential for %s is tagged as an API key "+
			"but is not a bare string", apperr.ErrTransfer, raw.Email)
	}

	var credentials map[string]jsontext.Value
	if err := json.Unmarshal(raw.Credentials, &credentials); err != nil || credentials == nil {
		return validated{}, fmt.Errorf("%w: the credential for %s must be a JSON object",
			apperr.ErrTransfer, raw.Email)
	}
	encoded, err := json.Marshal(credentials, json.Deterministic(true))
	if err != nil {
		return validated{}, fmt.Errorf("%w: encoding the credential for %s: %w",
			apperr.ErrTransfer, raw.Email, err)
	}
	entry.Credentials = string(encoded)
	return entry, nil
}

// importOne writes a single validated account.
func importOne(s *swap.Switcher, roster *swap.Roster, entry validated, force bool) (ImportedAccount, error) {
	identity := swap.Identity{Email: entry.Email, OrganizationUUID: entry.OrganizationUUID}
	usageIdentity := usagestore.Identity{Email: entry.Email, OrganizationUUID: entry.OrganizationUUID}

	target := ""
	outcome := Imported
	var notes []string

	if existing, found := roster.FindName(identity); found {
		stored := s.Usage.Entries(map[string]usagestore.Identity{existing: usageIdentity}, nil)[existing]
		switch {
		case force:
			outcome = Overwrote
			if stored.AuthDeadStrikes > 0 {
				notes = append(notes, "cleared this slot's stored dead-token strike")
				if stored.StruckFingerprint != "" &&
					claudeapi.Fingerprint(entry.Credentials) == stored.StruckFingerprint {
					// Store-fact wording on purpose: the import rewrites the
					// backup, so for the active slot the next poll may still
					// exercise the live credential. Promise only what happened.
					notes = append(notes, "this import holds the same credential generation "+
						"the strike condemned; another permanent auth failure will "+
						"quarantine it again — recover with a newer export or a re-login")
				}
			}
		case stored.TokenDead(""):
			// The narrow heal: a plain import replaces a slot whose stored
			// credential is quarantined as dead. The verdict normally postdates
			// the slot's last credential write, so the import is newer than what
			// failed. A healthy slot still requires --force.
			outcome = Replaced
			notes = append(notes, "this slot was quarantined as needing a re-login; "+
				"the import replaced it")
		default:
			return ImportedAccount{
				Name: existing, Email: entry.Email, Outcome: Skipped,
				Notes: []string{"it already exists here — use --force to overwrite it"},
			}, nil
		}
		target = existing
	} else {
		// Prefer the name the export used, so a machine-to-machine transfer
		// keeps the handles people have in their shell history.
		target = entry.Name
		if _, taken := roster.Accounts[target]; taken {
			target = roster.NameFor(entry.Email)
		}
	}

	if err := s.Creds.WriteAccount(target, entry.Email, entry.Credentials); err != nil {
		return ImportedAccount{}, err
	}
	s.BackupWritten(target, entry.Email)
	if err := s.WriteAccountConfig(target, entry.Email, entry.Config); err != nil {
		return ImportedAccount{}, err
	}
	// Every import introduces credential material whose previous verdict is no
	// longer authoritative. Without this, re-importing an identity into a slot
	// that was quarantined would stay quarantined and never fetch to prove the
	// imported token good.
	if err := s.Usage.ClearDeadToken([]string{target},
		map[string]usagestore.Identity{target: usageIdentity}); err != nil {
		return ImportedAccount{}, err
	}

	added := entry.Added
	if added == "" {
		added = swap.Timestamp(s.Now())
	}
	record := &swap.Account{
		Email:            entry.Email,
		UUID:             entry.UUID,
		OrganizationUUID: entry.OrganizationUUID,
		OrganizationName: entry.OrganizationName,
		Added:            added,
	}
	if entry.Kind == swap.KindAPIKey {
		record.Kind = swap.KindAPIKey
	}
	roster.Insert(target, record)
	if err := s.WriteRoster(roster); err != nil {
		return ImportedAccount{}, err
	}

	return ImportedAccount{
		Name: target, Email: entry.Email, Outcome: outcome, Notes: notes,
	}, nil
}

func orgLabel(uuid string) string {
	if uuid == "" {
		return "personal"
	}
	return uuid
}
