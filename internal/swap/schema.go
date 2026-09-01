package swap

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/d0lim/aaswap/internal/apperr"
)

// SchemaVersion is the account table's current format.
//
// Version 1 held one provider's accounts, keyed by slot number, at the top
// level. Version 2 nests every provider under its own section and keys accounts
// by name, because a slot number cannot say which provider it belongs to and
// one active pointer cannot express two tools being logged in at once.
const SchemaVersion = 2

// The providers this build can manage.
//
// A store may hold sections for providers not listed here — a newer release's
// — and those round-trip untouched. This list is what a person may ADDRESS.
const (
	// ProviderClaude is the provider every v1 store's accounts belong to: it
	// is the only tool that implementation could manage.
	ProviderClaude = "claude"
	// ProviderCodex is the OpenAI Codex CLI.
	ProviderCodex = "codex"
)

// Providers lists every addressable provider, in the order they are shown.
var Providers = []string{ProviderClaude, ProviderCodex}

// KnownProvider reports whether a name is one this build can manage.
func KnownProvider(name string) bool {
	return slices.Contains(Providers, name)
}

// File is sequence.json in full.
//
// Providers this build does not implement are read and rewritten verbatim. Two
// releases share a machine during any rollout, and a section dropped on a
// rewrite is a set of accounts whose credentials are still on disk with nothing
// naming them.
type File struct {
	SchemaVersion int                `json:"schemaVersion"`
	LastUpdated   string             `json:"lastUpdated,omitzero"`
	Providers     map[string]*Roster `json:"providers"`

	Extra map[string]jsontext.Value `json:",embed"`
}

// Rename is one account's move from a v1 slot number to a v2 name.
//
// Both ends are carried because the credential that has to move is filed under
// the old pair and lands under the new one, and the migration that moves it
// runs after the roster has already been rewritten in memory.
type Rename struct {
	Number string
	Email  string
	Name   string
}

// legacyFile is sequence.json as version 1 wrote it.
type legacyFile struct {
	ActiveAccountNumber *int                `json:"activeAccountNumber"`
	LastUpdated         string              `json:"lastUpdated,omitzero"`
	Sequence            []int               `json:"sequence"`
	Accounts            map[string]*Account `json:"accounts"`
}

// ParseFile reads sequence.json in either schema, upgrading version 1 in
// memory and reporting what the upgrade would have to move on disk.
//
// Nothing is written here. The caller applies the renames against the
// credential store first and publishes the file second, so a crash before the
// publish leaves version 1 intact rather than a roster naming credentials that
// have not moved.
//
// Refuses rather than guesses. A file that cannot be read is not an empty
// store: reading a torn one as "no accounts" is what let a later write rebuild
// the roster from nothing, taking a live credential backup with it.
func ParseFile(data []byte, provider string, now time.Time) (*File, []Rename, error) {
	var probe map[string]jsontext.Value
	if err := json.Unmarshal(data, &probe); err != nil || probe == nil {
		return nil, nil, fmt.Errorf("%w: the account table could not be parsed as a "+
			"JSON object (%v); repair or move it, then retry — refusing to overwrite "+
			"it unread", apperr.ErrConfig, err)
	}

	// Version 2 announces itself. Anything else is version 1, including a file
	// with no version at all, which is what every store written before this
	// release looks like.
	if _, current := probe["providers"]; current {
		var file *File
		if err := json.Unmarshal(data, &file); err != nil || file == nil {
			return nil, nil, fmt.Errorf("%w: the account table names providers but "+
				"could not be read (%v); repair or move it, then retry",
				apperr.ErrConfig, err)
		}
		file.normalize(now)
		return file, nil, nil
	}

	var legacy *legacyFile
	if err := json.Unmarshal(data, &legacy); err != nil || legacy == nil {
		return nil, nil, fmt.Errorf("%w: the account table could not be read (%v); "+
			"repair or move it, then retry", apperr.ErrConfig, err)
	}
	return upgrade(legacy, provider, now)
}

// upgrade turns a version 1 table into a version 2 one.
func upgrade(legacy *legacyFile, provider string, now time.Time) (*File, []Rename, error) {
	file := &File{
		SchemaVersion: SchemaVersion,
		LastUpdated:   legacy.LastUpdated,
		Providers:     map[string]*Roster{},
	}
	file.normalize(now)
	if len(legacy.Accounts) == 0 {
		return file, nil, nil
	}

	roster := newRoster()
	file.Providers[provider] = roster

	// Slot order decides every collision, so the walk has to be the v1 reader's
	// own order — sequence first, then whatever the sequence forgot. Ordering by
	// map iteration would hand the same store different names on each read, and
	// each read would then want to rename the credentials again.
	taken := map[string]bool{}
	var renames []Rename
	active := ""
	for _, num := range legacyOrder(legacy) {
		account := legacy.Accounts[num]
		if account == nil {
			continue
		}
		name := uniqueName(nameFromLegacy(account), taken)
		taken[name] = true

		// The alias is consumed by becoming the key. Left in the passthrough it
		// would round-trip forever and desynchronise the first time someone
		// renames the account.
		delete(account.Extra, "alias")

		roster.Accounts[name] = account
		roster.Order = append(roster.Order, name)
		renames = append(renames, Rename{Number: num, Email: account.Email, Name: name})

		if legacy.ActiveAccountNumber != nil && strconv.Itoa(*legacy.ActiveAccountNumber) == num {
			active = name
		}
	}
	roster.Active = active
	return file, renames, nil
}

// nameFromLegacy is the name a v1 slot takes.
//
// The alias when there is one and it is a legal name, because a person chose
// it. Otherwise the address. An alias the name rules refuse falls back rather
// than failing: this runs during a migration, and refusing would strand every
// account in the store over one bad label.
func nameFromLegacy(account *Account) string {
	if raw, ok := account.Extra["alias"]; ok {
		var alias string
		if err := json.Unmarshal(raw, &alias); err == nil {
			if name, err := NormalizeName(alias); err == nil {
				return name
			}
		}
	}
	return NameForEmail(account.Email)
}

// legacyOrder is the v1 reader's own ordering: the sequence, then any slot the
// sequence forgot, numerically.
func legacyOrder(legacy *legacyFile) []string {
	var out []string
	seen := map[string]bool{}
	for _, n := range legacy.Sequence {
		num := strconv.Itoa(n)
		if _, ok := legacy.Accounts[num]; ok && !seen[num] {
			out = append(out, num)
			seen[num] = true
		}
	}
	for _, num := range sortedSlots(legacy.Accounts) {
		if !seen[num] {
			out = append(out, num)
		}
	}
	return out
}

// normalize fills in what a reader may rely on being present.
func (f *File) normalize(now time.Time) {
	f.SchemaVersion = SchemaVersion
	if f.Providers == nil {
		f.Providers = map[string]*Roster{}
	}
	if f.LastUpdated == "" {
		f.LastUpdated = Timestamp(now)
	}
	for _, roster := range f.Providers {
		if roster == nil {
			continue
		}
		if roster.Accounts == nil {
			roster.Accounts = map[string]*Account{}
		}
		if roster.Order == nil {
			roster.Order = []string{}
		}
	}
}

// For returns a provider's roster, creating an empty one when this is the first
// account it will hold.
func (f *File) For(provider string) *Roster {
	if f.Providers == nil {
		f.Providers = map[string]*Roster{}
	}
	if roster, ok := f.Providers[provider]; ok && roster != nil {
		return roster
	}
	roster := newRoster()
	f.Providers[provider] = roster
	return roster
}

// ProviderNames lists every provider the file holds, in a stable order.
func (f *File) ProviderNames() []string {
	if f == nil {
		return nil
	}
	out := make([]string, 0, len(f.Providers))
	for name := range f.Providers {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}
