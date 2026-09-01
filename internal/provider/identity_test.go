package provider

import (
	"strings"
	"testing"
)

// The digest is what names an account nobody has written a parser for, so it
// has to be stable across runs and processes.
func TestTheDigestIsStable(t *testing.T) {
	spec := Spec{
		Name:  "x",
		Home:  Home{Default: ".x"},
		Files: []File{{Path: "auth.json", Role: RoleSecret}},
	}
	files := map[string]string{"auth.json": `{"token":"abc"}`}

	first := spec.Digest(files)
	if first == "" {
		t.Fatal("a secret with content produced no digest")
	}
	for range 10 {
		if got := spec.Digest(files); got != first {
			t.Fatalf("Digest varies between calls: %q vs %q", got, first)
		}
	}
}

// Two different credentials must not collide, or two accounts share a name and
// one overwrites the other.
func TestDifferentSecretsDigestDifferently(t *testing.T) {
	spec := Spec{
		Name:  "x",
		Home:  Home{Default: ".x"},
		Files: []File{{Path: "auth.json", Role: RoleSecret}},
	}
	a := spec.Digest(map[string]string{"auth.json": `{"token":"a"}`})
	b := spec.Digest(map[string]string{"auth.json": `{"token":"b"}`})
	if a == b {
		t.Errorf("two credentials share the digest %q", a)
	}
}

// A digest becomes a name, so it has to survive NormalizeName's rules: no
// leading dash, nothing purely numeric, nothing but [a-z0-9_.-].
func TestTheDigestIsAUsableName(t *testing.T) {
	spec := Spec{
		Name:  "x",
		Home:  Home{Default: ".x"},
		Files: []File{{Path: "auth.json", Role: RoleSecret}},
	}
	// Enough samples to catch an all-digits digest, which hex produces about
	// once in 4 billion but a shorter prefix produces far more often.
	for i := range 500 {
		// From 1: an empty secret is correctly no identity at all, which
		// TestNoSecretMeansNoDigest covers.
		digest := spec.Digest(map[string]string{"auth.json": strings.Repeat("x", i+1)})
		if digest == "" {
			t.Fatalf("sample %d produced no digest", i)
		}
		if strings.HasPrefix(digest, "-") {
			t.Errorf("digest %q starts with a dash", digest)
		}
		for _, r := range digest {
			if !strings.ContainsRune("abcdefghijklmnopqrstuvwxyz0123456789-_.", r) {
				t.Errorf("digest %q contains %q, which a name may not hold", digest, r)
				break
			}
		}
		if strings.Trim(digest, "0123456789") == "" {
			t.Errorf("digest %q is purely numeric, which a name may not be", digest)
		}
	}
}

// Only secrets. A machine-scoped config changes when someone edits their model
// choice, and letting that move the digest would rename the account.
func TestTheDigestIgnoresMachineFiles(t *testing.T) {
	spec := Spec{
		Name: "x",
		Home: Home{Default: ".x"},
		Files: []File{
			{Path: "auth.json", Role: RoleSecret},
			{Path: "config.toml", Role: RoleMachine, Optional: true},
		},
	}
	base := map[string]string{"auth.json": "secret", "config.toml": "model=a"}
	edited := map[string]string{"auth.json": "secret", "config.toml": "model=b"}
	if spec.Digest(base) != spec.Digest(edited) {
		t.Error("editing a machine-scoped config changed the account's digest")
	}
}

// Nothing stored is not an identity. Returning a digest of the empty string
// would give every logged-out provider the same account name.
func TestNoSecretMeansNoDigest(t *testing.T) {
	spec := Spec{
		Name:  "x",
		Home:  Home{Default: ".x"},
		Files: []File{{Path: "auth.json", Role: RoleSecret}},
	}
	for _, files := range []map[string]string{
		nil,
		{},
		{"auth.json": ""},
		{"auth.json": "   \n "},
	} {
		if got := spec.Digest(files); got != "" {
			t.Errorf("Digest(%v) = %q, want empty", files, got)
		}
	}
}

// A provider with a parser uses it, and the identity carries an address.
func TestResolvePrefersTheDeclaredParser(t *testing.T) {
	spec, _ := Lookup(Codex)
	identity, ok := spec.Resolve(map[string]string{"auth.json": codexAuthFixture(t)})
	if !ok {
		t.Fatal("a parseable Codex credential was not resolved")
	}
	if identity.Email != "person@example.com" {
		t.Errorf("Email = %q, want the parsed address", identity.Email)
	}
	if identity.Fingerprint == "" {
		t.Error("a resolved identity carries no fingerprint; the generation " +
			"cannot be compared later")
	}
}

// The point of the ladder: a provider whose format nobody parsed still yields
// an identity, so every account command works for it.
func TestResolveFallsBackToTheDigest(t *testing.T) {
	spec := Spec{
		Name:  "cursor",
		Home:  Home{Default: ".cursor"},
		Files: []File{{Path: "auth.json", Role: RoleSecret}},
	}
	identity, ok := spec.Resolve(map[string]string{"auth.json": `{"opaque":true}`})
	if !ok {
		t.Fatal("an unparseable credential yielded no identity at all, so a " +
			"provider without a parser cannot be managed")
	}
	if identity.Email != "" {
		t.Errorf("Email = %q, want empty: nothing parsed an address", identity.Email)
	}
	if identity.Fingerprint == "" {
		t.Error("the fallback produced no fingerprint")
	}
	if identity.Handle() != identity.Fingerprint {
		t.Errorf("Handle() = %q, want the fingerprint when there is no address",
			identity.Handle())
	}
}

// A declared parser that cannot read this particular credential — an API-key
// install, a half-written file — must not sink the account. It degrades.
func TestResolveDegradesWhenTheParserDeclines(t *testing.T) {
	spec, _ := Lookup(Codex)
	identity, ok := spec.Resolve(map[string]string{"auth.json": `{"auth_mode":"apikey"}`})
	if !ok {
		t.Fatal("a credential the parser declined produced no identity")
	}
	if identity.Email != "" {
		t.Errorf("Email = %q, want empty", identity.Email)
	}
	if identity.Fingerprint == "" {
		t.Error("the degraded path produced no fingerprint")
	}
}

// Nothing logged in is nothing, at every tier.
func TestResolveReportsNothingForAnEmptyStore(t *testing.T) {
	for _, name := range Names() {
		spec, _ := Lookup(name)
		if _, ok := spec.Resolve(nil); ok {
			t.Errorf("%q resolved an identity from no files at all", name)
		}
	}
}

// An address is preferred for the handle, because a person recognises it.
func TestHandlePrefersTheAddress(t *testing.T) {
	identity := Identity{Email: "person@example.com", Fingerprint: "a1b2c3d4"}
	if identity.Handle() != "person@example.com" {
		t.Errorf("Handle() = %q, want the address", identity.Handle())
	}
}

// codexAuthFixture is a Codex credential whose id_token carries an address.
func codexAuthFixture(t *testing.T) string {
	t.Helper()
	return codexAuth(t, map[string]any{"email": "person@example.com"})
}
