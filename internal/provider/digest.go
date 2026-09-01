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
// One tier answers, not several ORed together. A declared parser is
// AUTHORITATIVE, including when it says no: a Codex install authenticating with
// an API key genuinely has no address in it, and that is a fact about the
// login rather than a gap in aaswap's knowledge. Overriding it with a digest
// would manufacture an OAuth-shaped identity for a credential that has none,
// and send it down the wrong path at every layer above.
//
// The digest answers only for a provider with no parser at all. There the
// absence IS the situation: nobody has looked at this token format, and a
// fingerprint is the honest name for an account until a person renames it.
//
// The fingerprint rides along on a parsed identity as well, where it names the
// credential GENERATION rather than the account. That is what answers "has
// someone logged in outside aaswap since we last looked" — an address compares
// equal across a re-login and the token behind it does not. Best effort: a
// credential aaswap cannot read as a file, such as one in the macOS Keychain,
// leaves it empty without making the identity any less real.
func (s Spec) Resolve(files map[string]string) (Identity, bool) {
	fingerprint := s.Digest(files)
	if s.Identity != nil {
		identity, ok := s.Identity.Identify(files)
		if !ok {
			return Identity{}, false
		}
		identity.Fingerprint = fingerprint
		return identity, true
	}
	if fingerprint == "" {
		return Identity{}, false
	}
	return Identity{Fingerprint: fingerprint}, true
}
