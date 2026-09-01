package swap

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/d0lim/ccswap/internal/apperr"
	"github.com/d0lim/ccswap/internal/fsutil"
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
	Alias            string `json:"alias,omitzero"`
	Kind             Kind   `json:"kind,omitzero"`
	// Disabled excludes the slot from automatic selection while leaving it
	// switchable by hand. Absent rather than false when off, matching how the
	// record is written elsewhere.
	Disabled bool `json:"disabled,omitzero"`

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
}

// Identity returns the slot's account identity.
func (a *Account) Identity() Identity {
	if a == nil {
		return Identity{}
	}
	return Identity{Email: a.Email, OrganizationUUID: a.OrganizationUUID}
}

// Roster is sequence.json: which slots exist, their order, and which one is
// active.
type Roster struct {
	// ActiveAccountNumber is the slot last activated, or nil when none is.
	ActiveAccountNumber *int   `json:"activeAccountNumber"`
	LastUpdated         string `json:"lastUpdated,omitzero"`
	// Sequence is the display and rotation order, held as ints because that is
	// what is on disk.
	Sequence []int `json:"sequence"`
	// Accounts is keyed by slot number as a string, again matching the file.
	Accounts map[string]*Account `json:"accounts"`

	Extra map[string]jsontext.Value `json:",embed"`
}

// newRoster returns an empty roster.
func newRoster(now time.Time) *Roster {
	return &Roster{
		LastUpdated: Timestamp(now),
		Sequence:    []int{},
		Accounts:    map[string]*Account{},
	}
}

// Numbers returns every slot number, in the roster's own order, with any slot
// missing from the sequence appended.
//
// The sequence and the account map can disagree — a roster edited by hand, or a
// write interrupted between the two — and a slot that exists must never become
// invisible just because the ordering list forgot it.
func (r *Roster) Numbers() []string {
	if r == nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	for _, n := range r.Sequence {
		num := strconv.Itoa(n)
		if _, ok := r.Accounts[num]; ok && !seen[num] {
			out = append(out, num)
			seen[num] = true
		}
	}
	for _, num := range sortedSlots(r.Accounts) {
		if !seen[num] {
			out = append(out, num)
		}
	}
	return out
}

// sortedSlots orders slot keys numerically, so "10" follows "9".
func sortedSlots[T any](accounts map[string]T) []string {
	nums := slices.Collect(maps.Keys(accounts))
	slices.SortFunc(nums, func(a, b string) int {
		ai, aErr := strconv.Atoi(a)
		bi, bErr := strconv.Atoi(b)
		if aErr == nil && bErr == nil {
			return ai - bi
		}
		// A non-numeric key is not something ccswap writes, but it must still
		// order deterministically rather than crash the listing.
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

// FindSlot returns the slot holding the given identity.
func (r *Roster) FindSlot(identity Identity) (string, bool) {
	if r == nil {
		return "", false
	}
	for _, num := range r.Numbers() {
		if r.Accounts[num].Identity() == identity {
			return num, true
		}
	}
	return "", false
}

// NextNumber is the lowest slot number above every existing one.
//
// Deliberately not the lowest FREE number: reusing a number a user just removed
// would make "account 3" mean a different account than it did a minute ago, in
// a shell history full of `ccswap switch 3`.
func (r *Roster) NextNumber() int {
	highest := 0
	if r != nil {
		for num := range r.Accounts {
			if n, err := strconv.Atoi(num); err == nil {
				highest = max(highest, n)
			}
		}
	}
	return highest + 1
}

// Insert places an account at a slot, keeping the sequence ordered.
func (r *Roster) Insert(num string, account *Account, now time.Time) {
	if r.Accounts == nil {
		r.Accounts = map[string]*Account{}
	}
	r.Accounts[num] = account
	if n, err := strconv.Atoi(num); err == nil && !slices.Contains(r.Sequence, n) {
		r.Sequence = append(r.Sequence, n)
		slices.Sort(r.Sequence)
	}
	r.LastUpdated = Timestamp(now)
}

// Remove drops a slot from both the account map and the sequence.
func (r *Roster) Remove(num string, now time.Time) {
	delete(r.Accounts, num)
	if n, err := strconv.Atoi(num); err == nil {
		r.Sequence = slices.DeleteFunc(r.Sequence, func(s int) bool { return s == n })
	}
	if r.ActiveAccountNumber != nil && strconv.Itoa(*r.ActiveAccountNumber) == num {
		// The active pointer must not outlive the slot it names, or the next
		// read reports an account that no longer exists.
		r.ActiveAccountNumber = nil
	}
	r.LastUpdated = Timestamp(now)
}

// SetActive records which slot was last activated.
func (r *Roster) SetActive(num string, now time.Time) {
	if n, err := strconv.Atoi(num); err == nil {
		r.ActiveAccountNumber = &n
	}
	r.LastUpdated = Timestamp(now)
}

// Active returns the slot number last activated.
func (r *Roster) Active() (string, bool) {
	if r == nil || r.ActiveAccountNumber == nil {
		return "", false
	}
	num := strconv.Itoa(*r.ActiveAccountNumber)
	if _, ok := r.Accounts[num]; !ok {
		// The pointer names a slot that is gone. Reporting it would send every
		// caller looking for an account that does not exist.
		return "", false
	}
	return num, true
}

// ReadRoster loads sequence.json, reporting false when it does not exist yet.
//
// An existing but unreadable roster is an ERROR, never an empty one. Reading a
// torn file as "no accounts" is what let a subsequent write rebuild the roster
// from nothing, taking a live credential backup with it.
func (s *Switcher) ReadRoster() (*Roster, bool, error) {
	path := s.RosterPath()
	text, err := fsutil.ReadText(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("%w: %s exists but could not be read (%w); "+
			"fix what is blocking the read, then retry", apperr.ErrConfig, path, err)
	}

	// Decoded through a pointer so a file holding literal `null` comes back nil
	// rather than as a zero Roster. Null is not an empty roster: it is a file
	// that says nothing, and reading it as "no accounts" is the exact mistake
	// this reader exists to prevent.
	var roster *Roster
	if err := json.Unmarshal([]byte(text), &roster); err != nil || roster == nil {
		return nil, false, fmt.Errorf("%w: %s exists but could not be parsed as an "+
			"account roster (%v); repair or move it, then retry — refusing to "+
			"overwrite it unread", apperr.ErrConfig, path, err)
	}
	if roster.Accounts == nil {
		roster.Accounts = map[string]*Account{}
	}
	return roster, true, nil
}

// RosterOrEmpty loads the roster, returning an empty one when the file does not
// exist yet. An unreadable file is still an error.
func (s *Switcher) RosterOrEmpty() (*Roster, error) {
	roster, ok, err := s.ReadRoster()
	if err != nil {
		return nil, err
	}
	if !ok {
		return newRoster(s.now()), nil
	}
	return roster, nil
}

// WriteRoster publishes the roster atomically.
func (s *Switcher) WriteRoster(roster *Roster) error {
	if roster.Sequence == nil {
		// An explicit empty array, not null: the Python reader indexes it.
		roster.Sequence = []int{}
	}
	if roster.Accounts == nil {
		roster.Accounts = map[string]*Account{}
	}
	return writeJSON(s.RosterPath(), roster)
}
