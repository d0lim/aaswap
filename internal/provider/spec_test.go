package provider

import (
	"slices"
	"strings"
	"testing"
)

// Every provider aaswap offers has to be declared, or `--provider x` names
// something no code can act on.
func TestEveryOfferedProviderIsDeclared(t *testing.T) {
	for _, name := range Names() {
		spec, ok := Lookup(name)
		if !ok {
			t.Errorf("%q is offered but has no declaration", name)
			continue
		}
		if spec.Name != name {
			t.Errorf("%q is registered under the wrong name: %q", name, spec.Name)
		}
	}
}

func TestAnUndeclaredProviderIsNotFound(t *testing.T) {
	if _, ok := Lookup("nonesuch"); ok {
		t.Error("an undeclared provider was found")
	}
}

// A declaration that names no secret describes a provider whose login aaswap
// cannot store, which is the one thing it exists to do.
func TestEveryDeclarationNamesASecret(t *testing.T) {
	for _, name := range Names() {
		spec, _ := Lookup(name)
		if len(spec.SecretFiles()) == 0 {
			t.Errorf("%q declares no file holding its secret", name)
		}
	}
}

// Machine-role files must never be swapped: they hold the model choice, the
// MCP servers and the service tier, and carrying one account's onto another is
// a silent misconfiguration rather than a visible failure.
func TestMachineFilesAreNeverSwapped(t *testing.T) {
	for _, name := range Names() {
		spec, _ := Lookup(name)
		for _, file := range spec.SwappedFiles() {
			if file.Role.Has(RoleMachine) {
				t.Errorf("%q swaps %q, which is declared machine-scoped",
					name, file.Path)
			}
		}
	}
}

// Claude is the only provider whose auth reaches outside its home directory.
// If a second one ever appears the assumption baked into the storage layout
// has to be revisited, so this is a tripwire rather than a style rule.
func TestOnlyClaudeKeepsAuthOutsideItsHome(t *testing.T) {
	for _, name := range Names() {
		spec, _ := Lookup(name)
		if len(spec.Home.Outside) > 0 && name != "claude" {
			t.Errorf("%q keeps auth outside its home; the vault layout assumes "+
				"only Claude does", name)
		}
	}
}

// The two providers this build must support, and what each can actually do.
func TestTheImplementedProvidersDeclareWhatTheyCanDo(t *testing.T) {
	claude, ok := Lookup("claude")
	if !ok {
		t.Fatal("claude is not declared")
	}
	for _, capability := range []Capability{CapSession, CapUsage, CapRefresh} {
		if !claude.Can(capability) {
			t.Errorf("claude cannot %s", capability)
		}
	}

	codex, ok := Lookup("codex")
	if !ok {
		t.Fatal("codex is not declared")
	}
	if !codex.Can(CapSession) {
		t.Error("codex cannot host a session")
	}
	if !codex.Can(CapUsage) {
		t.Error("codex reports no usage at all")
	}
	// No refresher: an expired Codex token's only answer is a new login, and
	// pretending otherwise would send Anthropic's refresh endpoint an OpenAI
	// token.
	if codex.Can(CapRefresh) {
		t.Error("codex claims a token refresher it does not have")
	}
}

// Codex cannot tell whether a session is running against a profile, and the
// design turns that into "never auto-reseed" rather than "reseed blindly".
func TestCodexSessionsAreNeverAutoReseeded(t *testing.T) {
	codex, _ := Lookup("codex")
	if codex.Session == nil {
		t.Fatal("codex declares no session support")
	}
	if codex.Session.Liveness != nil {
		t.Skip("codex liveness has been implemented; the fail-safe no longer applies")
	}
	if codex.Session.MayReseed() {
		t.Error("codex would auto-reseed a profile it cannot prove is idle")
	}
}

func TestClaudeSessionsMayBeReseeded(t *testing.T) {
	claude, _ := Lookup("claude")
	if !claude.Session.MayReseed() {
		t.Error("claude declares liveness detection but refuses to reseed")
	}
}

// The point of the whole contract: a provider nobody has written code for still
// works for everything that does not need provider-specific knowledge.
//
// This is what makes cursor, antigravity and grok additions rather than
// projects. If this test ever needs more than Name, Home and Files to pass,
// the contract has grown a requirement it should not have.
func TestAMinimalDeclarationIsUsable(t *testing.T) {
	minimal := Spec{
		Name:  "cursor",
		Home:  Home{Default: ".cursor"},
		Files: []File{{Path: "auth.json", Role: RoleSecret}},
	}

	if err := minimal.Validate(); err != nil {
		t.Fatalf("a minimal declaration was rejected: %v", err)
	}

	// Everything that does not require a declared capability must work.
	for _, capability := range BaselineCapabilities {
		if !minimal.Can(capability) {
			t.Errorf("a minimal declaration cannot %s, so adding a provider is "+
				"not the two-line job the design claims", capability)
		}
	}

	// And everything that does must report itself unsupported, by name.
	for _, capability := range []Capability{CapSession, CapUsage, CapRefresh} {
		if minimal.Can(capability) {
			t.Errorf("a minimal declaration claims %s it never declared", capability)
		}
	}

	// Identity degrades to hashing rather than failing.
	if minimal.IdentityTier() != TierHash {
		t.Errorf("IdentityTier = %v, want the hash fallback", minimal.IdentityTier())
	}
}

// A declaration with no way to reach its files is a typo, not a provider.
func TestValidateRejectsIncompleteDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec Spec
		want string
	}{
		{
			name: "no name",
			spec: Spec{Home: Home{Default: ".x"}, Files: []File{{Path: "a", Role: RoleSecret}}},
			want: "name",
		},
		{
			name: "no home",
			spec: Spec{Name: "x", Files: []File{{Path: "a", Role: RoleSecret}}},
			want: "home",
		},
		{
			name: "no secret",
			spec: Spec{Name: "x", Home: Home{Default: ".x"},
				Files: []File{{Path: "a", Role: RoleMachine}}},
			want: "secret",
		},
		{
			name: "a session with no env var to isolate it",
			spec: Spec{Name: "x", Home: Home{Default: ".x"},
				Files:   []File{{Path: "a", Role: RoleSecret}},
				Session: &Session{Argv: []string{"x"}}},
			want: "HomeEnv",
		},
		{
			name: "an absolute path escapes the home",
			spec: Spec{Name: "x", Home: Home{Default: ".x"},
				Files: []File{{Path: "/etc/passwd", Role: RoleSecret}}},
			want: "relative",
		},
		{
			name: "a traversing path escapes the home",
			spec: Spec{Name: "x", Home: Home{Default: ".x"},
				Files: []File{{Path: "../../.ssh/id_rsa", Role: RoleSecret}}},
			want: "relative",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if err == nil {
				t.Fatalf("%+v was accepted", tc.spec)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

// Every shipped declaration has to pass the same check the tests above apply to
// invented ones, or the registry can hold something no command can use.
func TestShippedDeclarationsValidate(t *testing.T) {
	for _, name := range Names() {
		spec, _ := Lookup(name)
		if err := spec.Validate(); err != nil {
			t.Errorf("the shipped %q declaration is invalid: %v", name, err)
		}
	}
}

// Names is what `--provider` accepts and what help lists, so its order is user
// visible and must not depend on map iteration.
func TestNamesAreStablyOrdered(t *testing.T) {
	first := Names()
	for range 20 {
		if !slices.Equal(Names(), first) {
			t.Fatalf("Names() varies between calls: %v vs %v", Names(), first)
		}
	}
	if !slices.Contains(first, "claude") || !slices.Contains(first, "codex") {
		t.Errorf("Names() = %v, want both implemented providers", first)
	}
}
