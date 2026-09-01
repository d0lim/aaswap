package swap

import (
	"encoding/json/jsontext"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/fsutil"
)

// TimestampLayout is how the roster stamps times: UTC, second resolution, no
// offset. Shared with the Python implementation, which writes and parses the
// same spelling.
const TimestampLayout = "2006-01-02T15:04:05Z"

// Timestamp renders a time the way the roster stores it.
func Timestamp(t time.Time) string { return t.UTC().Format(TimestampLayout) }

// Kind is how a slot authenticates.
type Kind string

const (
	// KindOAuth is a normal Claude login. The default for any record that does
	// not say otherwise, including every record written before kinds existed.
	KindOAuth Kind = "oauth"
	// KindAPIKey is a slot holding a managed sk-ant-api key, which sits on a
	// different auth axis and reports no usage of its own.
	KindAPIKey Kind = "api_key"
)

// Account is one managed slot's record.
//
// Extra collects members this version does not know. They round-trip verbatim
// so a roster written by a newer release survives being read and rewritten by
// an older one — which happens routinely while two implementations share a
// machine.
type Account struct {
	Email            string `json:"email"`
	UUID             string `json:"uuid,omitzero"`
	OrganizationUUID string `json:"organizationUuid,omitzero"`
	OrganizationName string `json:"organizationName,omitzero"`
	Added            string `json:"added,omitzero"`
	Kind             Kind   `json:"kind,omitzero"`
	// Disabled excludes the account from rotation while leaving it
	// switchable by hand. Absent rather than false when off, matching how the
	// record is written elsewhere.
	Disabled bool `json:"disabled,omitzero"`

	// Fingerprint digests the credential last stored for this account.
	//
	// Written for every account and load-bearing for one kind: a provider
	// whose token format nobody has parsed has no address, and this is then
	// the only thing that identifies it. For the rest it records the
	// generation, which is what tells a re-login outside aaswap from the
	// credential aaswap itself put there.
	Fingerprint string `json:"fingerprint,omitzero"`

	Extra map[string]jsontext.Value `json:",embed"`
}

// AuthKind reports how the slot authenticates, defaulting to OAuth.
//
// Anything other than the API-key marker reads as OAuth, including a value from
// a future release: treating an unrecognized kind as an API key would suppress
// the usage the slot actually reports.
func (a *Account) AuthKind() Kind {
	if a != nil && a.Kind == KindAPIKey {
		return KindAPIKey
	}
	return KindOAuth
}

// Identity is the composite that names an account.
//
// Email alone is not identity: the same address legitimately exists across a
// personal account and one or more organizations, and those are different
// accounts with different quotas.
type Identity struct {
	Email            string
	OrganizationUUID string
	// Fingerprint identifies an account that has no address to be identified
	// by. Set only when Email is empty — see LiveIdentity.Identity.
	Fingerprint string
}

// Identity returns the slot's account identity.
func (a *Account) Identity() Identity {
	if a == nil {
		return Identity{}
	}
	if a.Email == "" {
		return Identity{Fingerprint: a.Fingerprint}
	}
	return Identity{Email: a.Email, OrganizationUUID: a.OrganizationUUID}
}

// Roster is one provider's accounts: which exist, their order, and which one
// is live.
//
// One per provider, because the active account is per provider. Two tools being
// logged in at once is the ordinary case, not an edge one, and a single active
// pointer cannot say it.
type Roster struct {
	// Active is the name last activated, empty when none is.
	Active string `json:"active,omitzero"`
	// Order is the display and rotation order.
	Order []string `json:"order"`
	// Accounts is keyed by name. The key IS the account's handle — there is no
	// separate alias field, so a name and the thing it names cannot drift.
	Accounts map[string]*Account `json:"accounts"`

	Extra map[string]jsontext.Value `json:",embed"`
}

// newRoster returns an empty roster.
func newRoster() *Roster {
	return &Roster{Order: []string{}, Accounts: map[string]*Account{}}
}

// Names returns every account name, in the roster's own order, with any account
// missing from the order appended.
//
// The order list and the account map can disagree — a table edited by hand, or
// a write interrupted between the two — and an account that exists must never
// become invisible just because the ordering list forgot it.
func (r *Roster) Names() []string {
	if r == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, name := range r.Order {
		if _, ok := r.Accounts[name]; ok && !seen[name] {
			out = append(out, name)
			seen[name] = true
		}
	}
	for _, name := range sortedNames(r.Accounts) {
		if !seen[name] {
			out = append(out, name)
		}
	}
	return out
}

// sortedNames orders account names alphabetically.
func sortedNames[T any](accounts map[string]T) []string {
	return slices.Sorted(maps.Keys(accounts))
}

// sortedSlots orders v1 slot keys numerically, so "10" follows "9". Used only
// when reading a version 1 table.
func sortedSlots[T any](accounts map[string]T) []string {
	nums := slices.Collect(maps.Keys(accounts))
	slices.SortFunc(nums, func(a, b string) int {
		ai, aErr := strconv.Atoi(a)
		bi, bErr := strconv.Atoi(b)
		if aErr == nil && bErr == nil {
			return ai - bi
		}
		return cmpStrings(a, b)
	})
	return nums
}

func cmpStrings(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// FindName returns the account holding the given identity.
func (r *Roster) FindName(identity Identity) (string, bool) {
	if r == nil {
		return "", false
	}
	for _, name := range r.Names() {
		if r.Accounts[name].Identity() == identity {
			return name, true
		}
	}
	return "", false
}

// TakenNames is every name already in use, for collision resolution.
func (r *Roster) TakenNames() map[string]bool {
	taken := map[string]bool{}
	if r == nil {
		return taken
	}
	for name := range r.Accounts {
		taken[name] = true
	}
	return taken
}

// NameFor picks the name a newly captured account will take: the address, with
// a suffix when something already holds it.
func (r *Roster) NameFor(email string) string {
	return uniqueName(NameForEmail(email), r.TakenNames())
}

// Insert places an account under a name, appending it to the order.
func (r *Roster) Insert(name string, account *Account) {
	if r.Accounts == nil {
		r.Accounts = map[string]*Account{}
	}
	r.Accounts[name] = account
	if !slices.Contains(r.Order, name) {
		r.Order = append(r.Order, name)
	}
}

// Remove drops an account from both the map and the order.
func (r *Roster) Remove(name string) {
	delete(r.Accounts, name)
	r.Order = slices.DeleteFunc(r.Order, func(n string) bool { return n == name })
	if r.Active == name {
		// The active pointer must not outlive the account it names, or the next
		// read reports an account that no longer exists.
		r.Active = ""
	}
}

// Rename moves an account to a new name, keeping its place in the order.
func (r *Roster) Rename(from, to string) {
	account, ok := r.Accounts[from]
	if !ok || from == to {
		return
	}
	delete(r.Accounts, from)
	r.Accounts[to] = account
	for i, name := range r.Order {
		if name == from {
			r.Order[i] = to
		}
	}
	if r.Active == from {
		r.Active = to
	}
}

// SetActive records which account was last activated.
func (r *Roster) SetActive(name string) { r.Active = name }

// ActiveName returns the account last activated.
func (r *Roster) ActiveName() (string, bool) {
	if r == nil || r.Active == "" {
		return "", false
	}
	if _, ok := r.Accounts[r.Active]; !ok {
		// The pointer names an account that is gone. Reporting it would send
		// every caller looking for something that does not exist.
		return "", false
	}
	return r.Active, true
}

// readStore loads sequence.json in whatever schema it is written in.
//
// An existing but unreadable table is an ERROR, never an empty one. Reading a
// torn file as "no accounts" is what let a subsequent write rebuild the table
// from nothing, taking a live credential backup with it.
func (s *Switcher) readStore() (file *File, found bool, renames []Rename, err error) {
	path := s.RosterPath()
	text, readErr := fsutil.ReadText(path)
	if readErr != nil {
		if errors.Is(readErr, fs.ErrNotExist) {
			return nil, false, nil, nil
		}
		return nil, false, nil, fmt.Errorf("%w: %s exists but could not be read (%w); "+
			"fix what is blocking the read, then retry", apperr.ErrConfig, path, readErr)
	}
	file, renames, err = ParseFile([]byte(text), s.provider(), s.now())
	if err != nil {
		return nil, false, nil, fmt.Errorf("%w (%s)", err, path)
	}
	return file, true, renames, nil
}

// StoreOrEmpty loads the whole table, returning an empty one when the file does
// not exist yet.
func (s *Switcher) StoreOrEmpty() (*File, []Rename, error) {
	file, found, renames, err := s.readStore()
	if err != nil {
		return nil, nil, err
	}
	if !found {
		file = &File{}
		file.normalize(s.now())
	}
	return file, renames, nil
}

// RosterOrEmpty loads this switcher's provider section.
func (s *Switcher) RosterOrEmpty() (*Roster, error) {
	file, _, err := s.StoreOrEmpty()
	if err != nil {
		return nil, err
	}
	return file.For(s.provider()), nil
}

// WriteRoster publishes one provider's section, leaving every other provider's
// untouched.
//
// The file is re-read to merge rather than held from the earlier load: another
// provider's section may have changed in between, and this must not be the
// write that drops it. Safe because every mutation runs under the store lock.
func (s *Switcher) WriteRoster(roster *Roster) error {
	file, _, err := s.StoreOrEmpty()
	if err != nil {
		return err
	}
	if roster.Order == nil {
		roster.Order = []string{}
	}
	if roster.Accounts == nil {
		roster.Accounts = map[string]*Account{}
	}
	file.Providers[s.provider()] = roster
	file.LastUpdated = Timestamp(s.now())
	file.SchemaVersion = SchemaVersion
	return writeJSON(s.RosterPath(), file)
}

// provider is the provider this switcher operates on, defaulting to Claude.
func (s *Switcher) provider() string {
	if s.Provider == "" {
		return ProviderClaude
	}
	return s.Provider
}
