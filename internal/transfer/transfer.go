// Package transfer moves accounts between machines through a portable JSON
// envelope.
//
// # No encryption
//
// The envelope carries live OAuth refresh tokens in the clear. That is
// deliberate rather than an omission: an encryption scheme built in here would
// have to invent key management, and users already have tools that do it
// properly —
//
//	aaswap export - | gpg -c > accounts.gpg
//	gpg -d accounts.gpg | aaswap import -
//
// The envelope records that it is unencrypted, and import refuses one that
// claims otherwise rather than handing ciphertext to a JSON parser.
//
// # Slim by default
//
// A default export carries only what a switch consumes: each account's own
// login and its identity block. The siblings are either machine-shared state
// the destination replaces at activation anyway, or device-bound and
// meaningless off-device — secret surface with no cross-machine value. A full
// export keeps everything, for backing up a machine to itself.
package transfer

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/swap"
)

// FormatVersion is the envelope's version. Import refuses anything else rather
// than guessing at a shape it does not know.
const FormatVersion = 1

// Envelope is the whole export.
type Envelope struct {
	Version    int    `json:"version"`
	ExportedAt string `json:"exportedAt"`
	// ExportedFrom names the source platform, for diagnosing a transfer that
	// behaves differently at the far end.
	ExportedFrom string `json:"exportedFrom"`
	SwapVersion  string `json:"swapVersion,omitzero"`
	// Encrypted is always false here. It exists so a user who wrapped the
	// envelope in their own encryption can mark it, and so import refuses
	// ciphertext with an explanation rather than a parse error.
	Encrypted bool `json:"encrypted"`
	// ActiveAccount is carried only when that account is actually in the
	// payload, so an import never points at an account that is not there.
	ActiveAccount string            `json:"activeAccount,omitzero"`
	Accounts      []ExportedAccount `json:"accounts"`
}

// ExportedAccount is one account in an envelope.
type ExportedAccount struct {
	Name             string `json:"name"`
	Email            string `json:"email"`
	UUID             string `json:"uuid"`
	OrganizationUUID string `json:"organizationUuid"`
	OrganizationName string `json:"organizationName"`
	Added            string `json:"added"`
	// Fingerprint digests the credential this account was stored with.
	//
	// Load-bearing for a provider whose token format nobody has parsed: it has
	// no address, and this is then the only thing identifying the account. For
	// the rest it is a note about which generation was exported. Additive, so
	// an older archive without it still imports.
	Fingerprint string `json:"fingerprint,omitzero"`
	// Kind marks an API-key account, whose credential is a raw string rather
	// than an object.
	Kind string `json:"kind,omitzero"`

	// Credentials is a JSON object for an OAuth account and a JSON string for
	// an API-key one, so it stays raw here and is interpreted by kind.
	Credentials jsontext.Value `json:"credentials"`
	Config      jsontext.Value `json:"config"`
}

// slimConfig reduces a captured config to what a switch actually consumes.
//
// Only the identity block is read back at activation, and stripping the rest
// keeps a transfer small while keeping the source machine's identifiers — its
// user id, its absolute paths, its cached flags — out of the destination.
//
// hasIdentity is false for a provider with no account-scoped config, where
// there is no identity block to keep and its absence is not a defect: the
// credential carries whatever identity the account has. Demanding one refused
// every export such a provider could otherwise produce.
func slimConfig(config jsontext.Value, label string, hasIdentity bool) (jsontext.Value, error) {
	var parsed map[string]jsontext.Value
	if err := json.Unmarshal(config, &parsed); err != nil || parsed == nil {
		return nil, fmt.Errorf("%w: the %s is not a JSON object", apperr.ErrTransfer, label)
	}
	account, present := parsed["oauthAccount"]
	if !present || string(account) == "null" {
		if !hasIdentity {
			return marshalObject(map[string]jsontext.Value{})
		}
		return nil, fmt.Errorf("%w: the %s carries no account identity — it cannot be "+
			"exported", apperr.ErrTransfer, label)
	}
	return marshalObject(map[string]jsontext.Value{"oauthAccount": account})
}

// slimCredentials reduces a credential to the account's own login.
//
// The siblings of the login are either machine-shared integration state — owned
// by whichever machine the export lands on, and replaced from the live
// credential at activation anyway — or device-bound and meaningless elsewhere.
// A legacy shape with no login exports verbatim rather than being emptied.
func slimCredentials(credentials jsontext.Value) (jsontext.Value, error) {
	parsed, ok := decodeObject(credentials)
	if !ok {
		// Not an object at all — a legacy shape. Carried verbatim rather than
		// emptied: it is still the account's credential.
		return credentials, nil
	}
	login, present := parsed["claudeAiOauth"]
	if !present {
		return credentials, nil
	}
	return marshalObject(map[string]jsontext.Value{"claudeAiOauth": login})
}

// decodeObject decodes a JSON object, reporting false for anything else.
func decodeObject(raw jsontext.Value) (map[string]jsontext.Value, bool) {
	var out map[string]jsontext.Value
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return nil, false
	}
	return out, true
}

func marshalObject(value map[string]jsontext.Value) (jsontext.Value, error) {
	data, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return nil, fmt.Errorf("%w: encoding an export section: %w", apperr.ErrTransfer, err)
	}
	return data, nil
}

// ExportRequest selects what to export.
type ExportRequest struct {
	// Account limits the export to one slot. Empty exports every account.
	Account string
	// Full keeps the whole config and the whole credential per account — for
	// backing up a machine to itself, where the source-machine identifiers are
	// the point rather than a leak.
	Full bool
}

// ExportResult reports what an export produced.
type ExportResult struct {
	Envelope Envelope
	// Skipped names slots left out because they had no stored material, with
	// the reason. Only possible when exporting everything: naming one account
	// makes a missing backup a hard failure, because that is the account the
	// user asked for.
	Skipped []string
}

// Export builds an envelope from a switcher's accounts.
func Export(s *swap.Switcher, req ExportRequest, swapVersion string) (ExportResult, error) {
	roster, err := s.RosterOrEmpty()
	if err != nil {
		return ExportResult{}, err
	}
	if len(roster.Accounts) == 0 {
		return ExportResult{}, fmt.Errorf("%w: there are no accounts to export — run "+
			"`aaswap add` first", apperr.ErrTransfer)
	}

	explicit := req.Account != ""
	var numbers []string
	if explicit {
		num, ok, resolveErr := s.ResolveIdentifier(roster, req.Account)
		if resolveErr != nil {
			return ExportResult{}, resolveErr
		}
		if !ok {
			return ExportResult{}, fmt.Errorf("%w: account not found: %s",
				apperr.ErrTransfer, req.Account)
		}
		if _, exists := roster.Accounts[num]; !exists {
			return ExportResult{}, fmt.Errorf("%w: account not found: %s",
				apperr.ErrTransfer, req.Account)
		}
		numbers = []string{num}
	} else {
		numbers = roster.Names()
	}

	// The live store holds fresher tokens than a backup for whichever account
	// is active, so that one is exported from the live store.
	liveSlot := ""
	if live, ok := s.LiveIdentity(); ok {
		if num, managed := roster.FindName(live.Identity()); managed {
			liveSlot = num
		}
	}

	result := ExportResult{Envelope: Envelope{
		Version:      FormatVersion,
		ExportedAt:   swap.Timestamp(s.Now()),
		ExportedFrom: s.Paths.Platform.String(),
		SwapVersion:  swapVersion,
	}}

	for _, num := range numbers {
		account := roster.Accounts[num]
		entry, skipped, err := exportOne(s, num, account, num == liveSlot, req.Full, explicit)
		if err != nil {
			return ExportResult{}, err
		}
		if skipped != "" {
			result.Skipped = append(result.Skipped, skipped)
			continue
		}
		result.Envelope.Accounts = append(result.Envelope.Accounts, entry)
	}

	if len(result.Envelope.Accounts) == 0 {
		return ExportResult{}, fmt.Errorf("%w: no account could be exported — every "+
			"managed slot is missing its stored credential or config. Re-add one with: "+
			"aaswap add --slot <number>", apperr.ErrTransfer)
	}

	// Carried only when that slot is actually in the payload: pointing at an
	// account the import cannot find would be worse than saying nothing.
	if activeName, ok := roster.ActiveName(); ok {
		for _, entry := range result.Envelope.Accounts {
			if entry.Name == activeName {
				result.Envelope.ActiveAccount = activeName
				break
			}
		}
	}
	return result, nil
}

// exportOne builds one account's entry, or explains why it was skipped.
func exportOne(s *swap.Switcher, num string, account *swap.Account, isLive, full, explicit bool) (ExportedAccount, string, error) {
	var credentials, config string

	if isLive {
		active := s.Creds.ReadActive()
		if active.Value == "" {
			return ExportedAccount{}, "", fmt.Errorf("%w: could not read the live "+
				"credential for the active account %s", apperr.ErrCredentialRead, account.Email)
		}
		credentials = active.Value
		liveConfig, err := s.ReadLiveConfig()
		if err != nil {
			return ExportedAccount{}, "", err
		}
		config = liveConfig
	} else {
		credentials, _ = s.Creds.ReadAccount(num, account.Email)
		config = s.ReadAccountConfig(num, account.Email)
		if credentials == "" || config == "" {
			if explicit {
				// The user named this account, so a missing backup is a
				// failure, not something to work around.
				if credentials == "" {
					return ExportedAccount{}, "", fmt.Errorf("%w: no stored credential for "+
						"account %s (%s)", apperr.ErrCredentialRead, num, account.Email)
				}
				return ExportedAccount{}, "", fmt.Errorf("%w: no stored config for account "+
					"%s (%s)", apperr.ErrConfig, num, account.Email)
			}
			// Exporting everything: one damaged slot must not poison the whole
			// backup.
			return ExportedAccount{}, fmt.Sprintf(
				"account %s (%s): no stored credential or config — re-add with: "+
					"aaswap add --name %s", num, account.Email, num), nil
		}
	}

	entry := ExportedAccount{
		Name:             num,
		Email:            account.Email,
		UUID:             account.UUID,
		OrganizationUUID: account.OrganizationUUID,
		OrganizationName: account.OrganizationName,
		Added:            account.Added,
		Fingerprint:      account.Fingerprint,
	}

	configValue := jsontext.Value(config)
	if !full {
		_, hasIdentity := s.Spec().ConfigFile()
		slim, err := slimConfig(configValue,
			fmt.Sprintf("config for %s", account.Email), hasIdentity)
		if err != nil {
			return ExportedAccount{}, "", err
		}
		configValue = slim
	} else if !configValue.IsValid() {
		return ExportedAccount{}, "", fmt.Errorf("%w: the config for %s is not valid JSON",
			apperr.ErrTransfer, account.Email)
	}
	entry.Config = configValue

	// An API-key account's credential is a raw key, not an object: carried as a
	// JSON string, and tagged, so the far end restores it as-is rather than
	// choking on a parse.
	if credstore.LooksLikeAPIKey(credentials) {
		entry.Kind = string(swap.KindAPIKey)
		encoded, err := json.Marshal(strings.TrimSpace(credentials))
		if err != nil {
			return ExportedAccount{}, "", fmt.Errorf("%w: encoding an API key: %w",
				apperr.ErrTransfer, err)
		}
		entry.Credentials = encoded
		return entry, "", nil
	}

	credentialValue := jsontext.Value(credentials)
	if !credentialValue.IsValid() {
		return ExportedAccount{}, "", fmt.Errorf("%w: the credential for %s is not valid JSON",
			apperr.ErrTransfer, account.Email)
	}
	if !full {
		slim, err := slimCredentials(credentialValue)
		if err != nil {
			return ExportedAccount{}, "", err
		}
		credentialValue = slim
	}
	entry.Credentials = credentialValue
	return entry, "", nil
}
