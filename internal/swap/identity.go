package swap

import (
	json "encoding/json/v2"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/credstore"
	providerpkg "github.com/d0lim/aaswap/internal/provider"
)

// LiveIdentity is who the machine is currently logged in as, read from Claude
// Code's own config.
type LiveIdentity struct {
	Email            string
	OrganizationUUID string
	OrganizationName string
	AccountUUID      string
}

// Identity narrows a live identity to the composite that names an account.
func (l LiveIdentity) Identity() Identity {
	return Identity{Email: l.Email, OrganizationUUID: l.OrganizationUUID}
}

// DisplayTag is how an account's org context reads to a person.
func (l LiveIdentity) DisplayTag() string { return displayTag(l.OrganizationName) }

func displayTag(orgName string) string {
	if orgName != "" {
		return orgName
	}
	return "personal"
}

// DisplayTag is the account record's org context.
func (a *Account) DisplayTag() string {
	if a == nil {
		return "personal"
	}
	return displayTag(a.OrganizationName)
}

// LiveIdentity reads the active account's identity from Claude Code's config.
//
// Every field comes from ONE read. Reading the config twice — once for the
// identity and once near the write — lets a /login landing in between pair one
// account's token with another's metadata, which is precisely the failure the
// ownership guards exist to close.
//
// Reports false when there is no active login: no config file, no parseable
// config, or no email in it. An account with no email address is not an
// identity, and treating it as one would let every comparison against it
// succeed vacuously.
func (s *Switcher) LiveIdentity() (LiveIdentity, bool) {
	if s.provider() == ProviderCodex {
		return s.codexLiveIdentity()
	}
	return s.claudeLiveIdentity()
}

// codexLiveIdentity reads the identity out of Codex's credential file.
//
// One file, not two. Codex has no config beside the credential to read an
// address from — the id_token inside the credential IS the identity document —
// so the read that answers "who is logged in" and the read that answers "what
// is the token" are the same read. That is not a smaller version of Claude's
// shape; it is a different one, which is why this cannot be a path swap.
func (s *Switcher) codexLiveIdentity() (LiveIdentity, bool) {
	data, err := os.ReadFile(s.Paths.CodexAuthPath())
	if err != nil {
		return LiveIdentity{}, false
	}
	identity, ok := providerpkg.CodexIdentity(string(data))
	if !ok {
		return LiveIdentity{}, false
	}
	return LiveIdentity{
		Email:            identity.Email,
		OrganizationUUID: identity.OrganizationUUID,
		OrganizationName: identity.OrganizationName,
		AccountUUID:      identity.AccountUUID,
	}, true
}

// claudeLiveIdentity reads Claude Code's config.
func (s *Switcher) claudeLiveIdentity() (LiveIdentity, bool) {
	data, ok := readObjectLenient(s.Paths.GlobalConfigPath())
	if !ok {
		return LiveIdentity{}, false
	}
	raw, ok := data["oauthAccount"]
	if !ok {
		return LiveIdentity{}, false
	}
	var account struct {
		EmailAddress     string `json:"emailAddress"`
		OrganizationUUID string `json:"organizationUuid"`
		OrganizationName string `json:"organizationName"`
		AccountUUID      string `json:"accountUuid"`
	}
	if err := json.Unmarshal(raw, &account); err != nil || account.EmailAddress == "" {
		return LiveIdentity{}, false
	}
	return LiveIdentity{
		Email:            account.EmailAddress,
		OrganizationUUID: account.OrganizationUUID,
		OrganizationName: account.OrganizationName,
		AccountUUID:      account.AccountUUID,
	}, true
}

// LiveIdentityMatches reports whether the live config names this identity right
// now.
//
// The under-lock re-check every locked credential path shares: a switch or a
// /login landing between a caller's pre-lock read and its lock acquisition
// changes this, and a mismatch means the live store is no longer the caller's
// account — so nothing there is theirs to adopt, consume or overwrite.
//
// The organization is compared too. Two managed slots may share an email across
// orgs, and matching on the address alone would let one account's transaction
// operate on another's credential.
func (s *Switcher) LiveIdentityMatches(identity Identity) bool {
	live, ok := s.LiveIdentity()
	return ok && live.Identity() == identity
}

// RejectIdentityDrift refuses when the active identity moved while a credential
// was being verified.
//
// A capture verifies a credential over the network and then reads the config
// again for the bytes it stores. A /login landing in that window puts one
// account's identity on another's credential — labelled as one account,
// containing another.
//
// The comparison is against the WHOLE triple that was read, never a rebuild
// from its parts: a rebuild describes no real account, so the guard would never
// match and would refuse every time rather than only on a race.
func (s *Switcher) RejectIdentityDrift(verified LiveIdentity) error {
	now, ok := s.LiveIdentity()
	if ok && now == verified {
		return nil
	}
	current := "unknown"
	if ok && now.Email != "" {
		current = now.Email
	}
	return fmt.Errorf("%w: the active account changed while %s was being verified "+
		"(now %s). Nothing was changed. Re-run when no other login is in flight",
		apperr.ErrConfig, verified.Email, current)
}

// RejectCredentialDrift refuses when the credential store rotated while the
// credential was being verified.
//
// The identity guard catches a /login, because that moves the config's account
// block. A plain refresh of the SAME account does not: the identity is
// unchanged and only the credential moved. Storing the pre-refresh bytes hands
// the slot a generation the server has already retired, so the slot's next
// refresh comes back invalid_grant.
//
// The comparison is on the LINEAGE fingerprint, which hashes the refresh token,
// so an access-token-only rotation compares equal on purpose. A difference
// therefore means the lineage advanced, not merely that some bytes changed.
//
// Unreadable is UNVERIFIABLE, not a refusal. This guard can only ever ADD a
// refusal, and a store that cannot be re-read is exactly the fail-open case the
// ownership guard already treats that way.
func (s *Switcher) RejectCredentialDrift(verified string) error {
	current := s.Creds.ReadActive()
	if current.Value == "" {
		return nil
	}
	before := claudeapi.Fingerprint(verified)
	after := claudeapi.Fingerprint(current.Value)
	if before == "" || after == "" || before == after {
		return nil
	}
	return fmt.Errorf("%w: the stored credential rotated while it was being verified. "+
		"Nothing was changed. Registering the pre-rotation generation would hand the "+
		"slot a credential the server has already retired. Re-run when no refresh is "+
		"in flight", apperr.ErrConfig)
}

// RejectLiveAPIKeyCapture refuses to snapshot a live managed key as an OAuth
// account.
//
// A capture stores the live credential under the config's OAuth identity. A raw
// sk-ant-api key captured that way becomes a kindless account, which corrupts
// every path that keys off the kind — the session guard, export, the cross-kind
// collision check — and points the user at the wrong recovery when it breaks.
func RejectLiveAPIKeyCapture(credentials string) error {
	if !credstore.LooksLikeAPIKey(credentials) {
		return nil
	}
	return fmt.Errorf("%w: the active login is an API-key account. Add it with "+
		"`aaswap add-token sk-ant-api...` instead", apperr.ErrValidation)
}

// RejectCrossKindCollision refuses to register a token whose identity already
// exists as the other kind.
//
// Identity is the (email, organization) composite alone, so two slots sharing an
// email across kinds could not be told apart at switch time. Threading the kind
// through the whole identity system would be the alternative; refusing the
// collision and asking for a distinct address is smaller and says what happened.
// The default token labels never collide, so this only ever fires on a forced
// address.
func (s *Switcher) RejectCrossKindCollision(roster *Roster, email string, isAPIKey bool) error {
	num, ok := roster.FindName(Identity{Email: email})
	if !ok {
		return nil
	}
	existing := roster.Accounts[num].AuthKind()
	incoming := KindOAuth
	if isAPIKey {
		incoming = KindAPIKey
	}
	if existing == incoming {
		return nil
	}
	return fmt.Errorf("%w: %q already exists as an %s account (slot %s); cannot add it "+
		"as an %s account. Pass a distinct --email",
		apperr.ErrValidation, email, kindLabel(existing), num, kindLabel(incoming))
}

func kindLabel(k Kind) string {
	if k == KindAPIKey {
		return "API-key"
	}
	return "OAuth"
}

// ResolveIdentifier maps what a user typed — a slot number, an alias, or an
// email — to a slot number.
//
// Precedence is number, then alias, then email. A number is taken literally
// even when no such slot exists, so "switch 9" reports "no account 9" rather
// than hunting for an alias named "9".
//
// An email matching several slots is an ERROR, not a guess. The same address
// across two organizations is two accounts with two quotas, and picking one
// would silently switch the user to an account they did not name.
func (s *Switcher) ResolveIdentifier(roster *Roster, identifier string) (string, bool, error) {
	if roster == nil || identifier == "" {
		return "", false, nil
	}
	// The name is the key, so an exact hit needs no search. Case-folded because
	// names are stored lowercased and a person typing one should not have to
	// remember that.
	//
	// A name and an address can never collide, so the order below is not a
	// precedence rule to reason about: NormalizeName refuses "@", and every
	// address has one.
	wanted := strings.ToLower(identifier)
	for name := range roster.Accounts {
		if name == wanted {
			return name, true, nil
		}
	}

	var matches []string
	for _, name := range roster.Names() {
		if strings.EqualFold(roster.Accounts[name].Email, identifier) {
			matches = append(matches, name)
		}
	}
	switch len(matches) {
	case 0:
		return "", false, nil
	case 1:
		return matches[0], true, nil
	}

	details := make([]string, len(matches))
	for i, name := range matches {
		details[i] = fmt.Sprintf("%s [%s]", name, roster.Accounts[name].DisplayTag())
	}
	return "", false, fmt.Errorf("%w: email %q is ambiguous — it matches accounts: %s. "+
		"Use the account name instead (for example, `aaswap switch %s`)",
		apperr.ErrConfig, identifier, strings.Join(details, ", "), matches[0])
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	_, err := strconv.Atoi(s)
	if err != nil {
		return false
	}
	// Atoi accepts a leading sign; a slot number never carries one.
	return strings.IndexFunc(s, func(r rune) bool { return r < '0' || r > '9' }) < 0
}
