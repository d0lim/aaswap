package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// digestLength is how much of the hash becomes a name.
//
// Eight hex characters is 32 bits. A person has a handful of accounts per
// provider, so the collision risk is negligible, and the string stays short
// enough to type — which matters, because this IS the account's name until
// somebody renames it.
const digestLength = 8

// digestPrefix keeps a digest from being read as something else.
//
// Two rules a name must satisfy make a bare hex prefix unsafe: a name may not
// be purely numeric, and a purely numeric name would be read as a legacy slot
// number. Roughly one digest in 10^-1.6 million is all digits — rare enough to
// never see in testing and certain to happen to somebody.
const digestPrefix = "a"

// Digest is the fingerprint of a provider's stored secret.
//
// Taken over the SECRET files only, in declared order. A machine-scoped config
// is excluded deliberately: it changes when someone edits their model choice,
// and letting that move the digest would silently rename the account.
//
// Empty when there is no secret to fingerprint. Hashing the empty string would
// give every logged-out provider the same answer, and that answer would then
// compare equal to itself and look like a real identity.
func (s Spec) Digest(files map[string]string) string {
	sum := sha256.New()
	any := false
	for _, file := range s.SecretFiles() {
		content, ok := files[file.Path]
		if !ok || strings.TrimSpace(content) == "" {
			continue
		}
		any = true
		// The path goes into the hash too. Without it, moving content between
		// two secret files would leave the digest unchanged.
		sum.Write([]byte(file.Path))
		sum.Write([]byte{0})
		sum.Write([]byte(content))
		sum.Write([]byte{0})
	}
	if !any {
		return ""
	}
	return digestPrefix + hex.EncodeToString(sum.Sum(nil))[:digestLength]
}

// Resolve is who a credential belongs to, by the best means this provider
// declares.
//
// The ladder, in order: the declared parser, then the digest. A parser that
// declines does not sink the account — an API-key install has no address in it,
// and refusing would make a perfectly usable login unmanageable.
//
// The fingerprint is always filled in, even when an address was parsed. It
// identifies the credential GENERATION rather than the account, which is what
// answers "has someone logged in outside aaswap since we last looked".
//
// Reports false only when there is no secret at all, which is what "nobody is
// logged in" looks like at every tier.
func (s Spec) Resolve(files map[string]string) (Identity, bool) {
	fingerprint := s.Digest(files)
	if fingerprint == "" {
		return Identity{}, false
	}
	if s.Identity != nil {
		if identity, ok := s.Identity.Identify(files); ok {
			identity.Fingerprint = fingerprint
			return identity, true
		}
	}
	return Identity{Fingerprint: fingerprint}, true
}
