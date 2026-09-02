package swap

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
)

// namePattern is what a name may contain once normalized.
//
// Deliberately narrower than "what a filesystem accepts": a name is typed at a
// shell prompt, stored as a path component, and used as a Keychain account, and
// the intersection of what all three handle without quoting is small.
var namePattern = regexp.MustCompile(`^[a-z0-9_.-]+$`)

// allDots matches a name made of nothing but dots.
var allDots = regexp.MustCompile(`^\.+$`)

// DefaultName is the name an account falls back to when its address carries
// nothing usable. Collisions on it are resolved like any other.
const DefaultName = "account"

// NormalizeName validates and canonicalizes an account name.
//
// A name is the account's handle, and it is also a path component — it names
// the slot's credential file and its Keychain item. So this refuses three
// things the old alias rules allowed, each harmless as a label and not as a
// filename: "." and ".." and any other all-dots run, which address a directory
// rather than a file in it.
//
// A LEADING dot is fine. ".ssh" is a hidden file, not an escape, and someone
// who wants their account named that has a reason.
func NormalizeName(name string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(name))
	switch {
	case normalized == "":
		return "", fmt.Errorf("%w: a name cannot be empty", apperr.ErrValidation)
	case allDots.MatchString(normalized):
		// Both a path hazard and meaningless as a handle.
		return "", fmt.Errorf("%w: %q is not a name — it addresses a directory",
			apperr.ErrValidation, name)
	case isDigits(normalized):
		// Slot numbers are gone, but every shell history on the machine still
		// has `switch 2` in it meaning something else. Refusing keeps a name
		// from silently inheriting an old command's meaning.
		return "", fmt.Errorf("%w: %q cannot be purely numeric", apperr.ErrValidation, name)
	case strings.HasPrefix(normalized, "-"):
		return "", fmt.Errorf("%w: %q cannot start with '-' (it would be read as "+
			"a command flag)", apperr.ErrValidation, name)
	case !namePattern.MatchString(normalized):
		return "", fmt.Errorf("%w: %q may only contain letters, digits, '-', '_' "+
			"and '.'", apperr.ErrValidation, name)
	}
	return normalized, nil
}

// NameForEmail derives a default account name from an address.
//
// The local part, because it is the half a person recognizes: "work" out of
// work@example.com. Everything the name rules forbid is stripped rather than
// refused — this runs during a migration where refusing means an account with
// no handle at all, and an ugly name beats an unreachable account.
//
// The result is always a legal name. Callers may rely on that.
func NameForEmail(email string) string {
	local, _, _ := strings.Cut(email, "@")
	// Plus-addressing names one inbox, not one account.
	local, _, _ = strings.Cut(local, "+")
	local = strings.ToLower(strings.TrimSpace(local))

	var b strings.Builder
	for _, r := range local {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-")

	switch {
	case name == "" || allDots.MatchString(name):
		return DefaultName
	case isDigits(name):
		// A numeric local part is legal in an address and refused as a name.
		// Prefixing keeps the address recognizable in the handle.
		return DefaultName + "-" + name
	case strings.HasPrefix(name, "-"):
		return DefaultName + "-" + strings.TrimPrefix(name, "-")
	}
	return name
}

// uniqueName returns base, or base-2, base-3… when it is already taken.
//
// The suffix starts at 2 because the unsuffixed name IS the first one: "work"
// and "work-2" read as a pair, where "work-1" and "work-2" invite the question
// of where "work" went.
func uniqueName(base string, taken map[string]bool) string {
	if !taken[base] {
		return base
	}
	for n := 2; ; n++ {
		candidate := base + "-" + strconv.Itoa(n)
		if !taken[candidate] {
			return candidate
		}
	}
}
