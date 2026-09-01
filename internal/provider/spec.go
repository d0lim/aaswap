package provider

import (
	"cmp"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// Role is what a file inside a provider's home belongs to.
//
// A bitmask because one file can be two things at once: Codex's auth.json is
// both the secret and the identity document, and splitting it into two entries
// would make the swap copy it twice.
type Role uint8

const (
	// RoleSecret is the credential itself. Swapped, and protected.
	RoleSecret Role = 1 << iota
	// RoleIdentity is where an account's name comes from. Swapped.
	RoleIdentity
	// RoleMachine belongs to the machine rather than to whoever is logged in:
	// the model choice, the MCP servers, the service tier. NEVER swapped —
	// carrying one account's onto another is a silent misconfiguration.
	RoleMachine
)

// Has reports whether every bit in want is set.
func (r Role) Has(want Role) bool { return r&want == want }

func (r Role) String() string {
	var parts []string
	for _, named := range []struct {
		bit  Role
		name string
	}{{RoleSecret, "secret"}, {RoleIdentity, "identity"}, {RoleMachine, "machine"}} {
		if r.Has(named.bit) {
			parts = append(parts, named.name)
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, "|")
}

// File is one path inside a provider's home, and what it belongs to.
type File struct {
	// Path is relative to the provider's home — or to the user's home
	// directory for the entries in Home.Outside.
	Path string
	Role Role
	// Optional marks a file whose absence is normal rather than an error. A
	// machine-scoped config nobody has customised does not exist yet.
	Optional bool
}

// Home is where a provider's tool keeps everything, and how to repoint it.
type Home struct {
	// Env relocates the whole home, the way CODEX_HOME and CLAUDE_CONFIG_DIR
	// do. Empty when the tool has no such variable, which also means it cannot
	// host an isolated session profile.
	Env string
	// Default is relative to the user's home directory: ".codex".
	Default string
	// Outside holds auth files that live outside Home. Claude's ~/.claude.json
	// is the only known case, and treating that shape as the common one is why
	// the pre-contract implementation did not generalise.
	Outside []File
}

// Login is how a person authenticates this provider's tool.
//
// aaswap does not run it. `login` watches for a credential to land and captures
// what appears (see swap.AwaitNewLogin), so this exists to TELL the user what
// to type — which is why an unknown provider's login flow is not a blocker.
type Login struct {
	Argv []string
}

// ShareSet is what an isolated profile mirrors from the default one.
type ShareSet struct {
	// Customizations are settings, skills, commands and agents.
	Customizations []string
	// History is the conversation record, mirrored only when asked for: it is
	// the one shared item a user cannot regenerate.
	History []string
}

// Liveness reports the processes running against a profile directory.
//
// complete is false when a record could not be read. Callers must treat an
// incomplete answer exactly like "something is running": the whole point is to
// avoid writing a credential out from under a live session, and an unreadable
// record is not evidence of absence.
type Liveness interface {
	PIDs(profileDir string) (pids []int, complete bool)
}

// Session is what a provider needs to host an isolated `run` profile.
type Session struct {
	// HomeEnv repoints the tool at a profile directory. Without one there is
	// no way to isolate a session at all.
	HomeEnv string
	// Argv is what `run` launches.
	Argv []string
	// Share is what a profile mirrors from the default one.
	Share ShareSet
	// Liveness answers "is anything running against this profile".
	//
	// Nil is a legitimate declaration, not an omission to fix later: it means
	// aaswap cannot tell, and MayReseed turns that into the conservative
	// answer. See MayReseed.
	Liveness Liveness
}

// MayReseed reports whether aaswap may refresh a stale profile's credential on
// its own.
//
// Only when it can prove nothing is running against the profile. A provider
// with no liveness source can never prove that, so it never auto-reseeds —
// the session launches with the credential the profile already has, and the
// user is told to refresh it deliberately.
//
// This is what lets a provider ship session support before anyone has worked
// out how to detect its running processes. The unsafe direction requires
// evidence; the safe one is the default.
func (s *Session) MayReseed() bool {
	return s != nil && s.Liveness != nil
}

// Hazard is state that outlives a credential swap.
//
// Claude Code's Agent View daemon survives a session and keeps using the
// account it started with, so swapping the credential file is not the whole
// swap. Codex keeps an app-server and several SQLite files in the same shape.
// A provider that declares nothing here is not safe — it is unexamined.
type Hazard struct {
	// Env is injected to disable the offending feature, as KEY=VALUE.
	Env []string
	// Purge names paths inside the home to remove on switch: caches keyed to
	// the account that is going away.
	Purge []string
	// Warn names a process that should not be running during a swap.
	Warn string
}

// Capability is something a command needs and a provider may or may not have.
type Capability string

const (
	// CapAccounts is naming, listing and forgetting accounts. Every provider
	// has it: it needs nothing but the roster.
	CapAccounts Capability = "manage accounts"
	// CapSwitch is moving the live login between stored accounts.
	CapSwitch Capability = "switch accounts"
	// CapLogin is capturing a login that lands.
	CapLogin Capability = "capture logins"
	// CapTransfer is export and import.
	CapTransfer Capability = "export and import"
	// CapSession is `run` and the directory mappings that feed it.
	CapSession Capability = "run isolated sessions"
	// CapUsage is reporting rate-limit headroom.
	CapUsage Capability = "report usage"
	// CapRefresh is renewing an expired token without a new login.
	CapRefresh Capability = "refresh tokens"
)

// BaselineCapabilities are what a provider gets from Name, Home and Files
// alone.
//
// The list is the contract's promise: declaring where a tool keeps its login is
// enough to manage accounts for it. Everything outside this list must be
// declared, and its absence is reported rather than worked around.
var BaselineCapabilities = []Capability{
	CapAccounts, CapSwitch, CapLogin, CapTransfer,
}

// Tier is how an account's name is discovered, in descending order of how much
// the provider had to be understood.
type Tier int

const (
	// TierHash names an account by a digest of its secret. Works for any
	// provider, including one whose token format nobody has looked at.
	TierHash Tier = iota
	// TierProbe asks the provider's own CLI who is logged in.
	TierProbe
	// TierParse reads an address out of a known field.
	TierParse
)

func (t Tier) String() string {
	switch t {
	case TierParse:
		return "parse"
	case TierProbe:
		return "probe"
	default:
		return "hash"
	}
}

// Spec is everything aaswap needs to know about one agent CLI.
//
// Only Name, Home and Files are required. Everything else is a capability, and
// a provider that does not declare one is not broken: commands needing it
// report it unsupported, by name and with a reason, instead of silently doing
// something else. That is the difference between adding a provider and porting
// aaswap to it.
type Spec struct {
	Name string
	// Label is the tool's name as a person writes it — "Claude Code" against
	// the identifier "claude". Messages a user reads name the TOOL, not the
	// `--provider` value, and a store key makes a poor sentence. Optional:
	// empty falls back to Name, so a minimal declaration still reads correctly.
	Label string
	Home  Home
	Files []File

	Login    *Login
	Identity IdentitySource // nil → the hash fallback
	Usage    UsageSource    // nil → headroom is unknown, never zero
	Session  *Session       // nil → `run` is unsupported here
	Hazards  []Hazard

	// Keychain says this provider's tool ALSO keeps its live credential in the
	// macOS Keychain, so aaswap has to reconcile two stores that can disagree.
	//
	// Claude Code is the only one. Declaring it here rather than assuming it
	// is what makes the file-only path the default: every other tool reads and
	// writes a single file on every platform, and treating Claude's
	// arrangement as normal is what kept the old implementation from
	// generalising.
	Keychain bool

	// Refreshable says an expired credential can be renewed without a new
	// login. A declared fact rather than an implementation: the renewal needs
	// an HTTP client, which belongs to the layer that owns the network, and a
	// provider declaration must stay constructible without one.
	Refreshable bool
}

// DisplayName is what to call this tool in a sentence.
func (s Spec) DisplayName() string { return cmp.Or(s.Label, s.Name) }

// IdentitySource reads who a credential belongs to.
//
// Implementations receive the provider's secret files by their declared path.
// Reporting false is normal, not an error: an API-key install has no address in
// it, and the caller falls back to the hash rather than refusing the account.
type IdentitySource interface {
	Identify(files map[string]string) (Identity, bool)
	// Tier says how the name was obtained, for the capability report.
	Tier() Tier
}

// UsageSource reports rate-limit headroom.
//
// Scope says whose numbers it can produce: some providers publish per-account
// figures, and some only record what the account currently logged in consumed.
type UsageSource interface {
	Scope() UsageScope
}

// UsageScope is which accounts a usage source can speak for.
type UsageScope int

const (
	// UsagePerAccount can measure any stored account.
	UsagePerAccount UsageScope = iota
	// UsageLiveOnly can only measure whoever is logged in now. An idle account
	// reports no measurement, which reads as unknown rather than exhausted and
	// so is safe by construction.
	UsageLiveOnly
)

// Can reports whether this provider supports a capability.
func (s Spec) Can(c Capability) bool {
	if slices.Contains(BaselineCapabilities, c) {
		return true
	}
	switch c {
	case CapSession:
		return s.Session != nil
	case CapUsage:
		return s.Usage != nil
	case CapRefresh:
		return s.Refreshable
	default:
		return false
	}
}

// Why explains an unsupported capability in a sentence a user can act on.
func (s Spec) Why(c Capability) string {
	if s.Can(c) {
		return ""
	}
	switch c {
	case CapSession:
		return fmt.Sprintf("%s does not declare an isolated profile directory, "+
			"so aaswap cannot run a session as one of its accounts", s.Name)
	case CapUsage:
		return fmt.Sprintf("aaswap has no way to read %s's rate limits, so it "+
			"reports headroom as unknown rather than guessing", s.Name)
	case CapRefresh:
		return fmt.Sprintf("aaswap cannot renew a %s token; log in again with "+
			"`aaswap --provider %s login`", s.Name, s.Name)
	default:
		return fmt.Sprintf("%s does not support %s", s.Name, c)
	}
}

// IdentityTier is how this provider's accounts get their names.
func (s Spec) IdentityTier() Tier {
	if s.Identity == nil {
		return TierHash
	}
	return s.Identity.Tier()
}

// UsageScope is whose usage this provider can report.
func (s Spec) UsageScope() (UsageScope, bool) {
	if s.Usage == nil {
		return UsagePerAccount, false
	}
	return s.Usage.Scope(), true
}

// SecretFiles are the files holding the credential, in declared order.
//
// The digest that names an account when nothing better is available is taken
// over these, so their order is part of the identity and must not depend on map
// iteration.
func (s Spec) SecretFiles() []File {
	return s.filesWith(RoleSecret)
}

// SwappedFiles are the files a switch moves: everything belonging to the
// account rather than to the machine.
func (s Spec) SwappedFiles() []File {
	var out []File
	for _, file := range s.allFiles() {
		if file.Role.Has(RoleSecret) || file.Role.Has(RoleIdentity) {
			out = append(out, file)
		}
	}
	return out
}

// IdentityFiles are the files an address can be read out of.
func (s Spec) IdentityFiles() []File {
	return s.filesWith(RoleIdentity)
}

func (s Spec) filesWith(role Role) []File {
	var out []File
	for _, file := range s.allFiles() {
		if file.Role.Has(role) {
			out = append(out, file)
		}
	}
	return out
}

// allFiles is every declared file, home entries first then the outside ones, so
// the ordering the digest depends on is fixed by the declaration.
func (s Spec) allFiles() []File {
	return slices.Concat(s.Files, s.Home.Outside)
}

// Validate reports a declaration that no command could act on.
//
// Run against every shipped declaration by the tests, and worth running against
// one loaded from anywhere else: the failure modes here are a swap that copies
// nothing and a path that escapes the home it was meant to stay inside.
func (s Spec) Validate() error {
	if s.Name == "" {
		return fmt.Errorf("a provider declaration needs a name")
	}
	if s.Home.Default == "" {
		return fmt.Errorf("%s: a provider declaration needs a home directory", s.Name)
	}
	if len(s.SecretFiles()) == 0 {
		return fmt.Errorf("%s: no declared file holds the secret, so there is "+
			"nothing for aaswap to store", s.Name)
	}
	for _, file := range s.allFiles() {
		if err := validPath(file.Path); err != nil {
			return fmt.Errorf("%s: %q must be relative to the home directory: %w",
				s.Name, file.Path, err)
		}
	}
	if s.Session != nil && s.Session.HomeEnv == "" {
		return fmt.Errorf("%s: session support needs a HomeEnv to point the "+
			"tool at a profile directory", s.Name)
	}
	if s.Session != nil && len(s.Session.Argv) == 0 {
		return fmt.Errorf("%s: session support needs an Argv to launch", s.Name)
	}
	return nil
}

// validPath refuses anything that would resolve outside the home it is joined
// to. A declaration is developer-written, but it is also the one place a path
// is assembled from a string and then written through.
func validPath(path string) error {
	if path == "" {
		return fmt.Errorf("the path is empty")
	}
	if filepath.IsAbs(path) {
		return fmt.Errorf("the path is absolute")
	}
	clean := filepath.Clean(path)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("the path escapes the directory")
	}
	return nil
}

// ConfigFile is the account-scoped config that sits BESIDE the credential, if
// this provider has one.
//
// A declared identity file that is not itself the secret. Claude has one —
// ~/.claude.json holds the address while .credentials.json holds the token — and
// Codex does not, because its auth.json is both at once.
//
// That difference is why capture cannot be a path swap: with a separate config
// there are two files to keep in step and a window between reading them, and
// with one file there is neither. Callers ask this rather than the provider's
// name so a provider added later lands on whichever side it belongs to.
func (s Spec) ConfigFile() (File, bool) {
	for _, file := range s.IdentityFiles() {
		if !file.Role.Has(RoleSecret) {
			return file, true
		}
	}
	return File{}, false
}
