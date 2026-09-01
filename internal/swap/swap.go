// Package swap is the account switcher: the roster of managed slots, the
// identity rules that decide which slot a credential belongs to, and the
// transactions that move credentials between a slot and the machine's live
// Claude Code login.
//
// # The decomposition
//
// The Python original was one 7,000-line class. Here the same behavior is split
// by role — roster.go owns sequence.json, identity.go owns "whose credential is
// this", activate.go owns the switch transaction, and so on — with the storage
// layer reached only through the narrow interfaces on [Switcher]. That is the
// rule credstore already established from the other side: the storage layer
// never imports the orchestration above it, so the two cannot re-couple.
//
// # What must never break
//
// This package moves live login credentials between stores. Three invariants
// carry that:
//
//   - A credential is never written to a slot until it has been read back and
//     verified. Every destructive step runs after the replacement is in memory.
//   - Identity is the (email, organization) composite, never the email alone.
//     The same address exists across a personal account and its organizations,
//     and those are different accounts with different quotas.
//   - A transaction that cannot complete rolls back every step it took, in
//     reverse. A half-applied switch leaves a user logged into an account they
//     did not choose, with the credential for the one they wanted nowhere.
package swap

import (
	"context"
	"path/filepath"
	"time"

	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/keychain"
	"github.com/d0lim/aaswap/internal/lockfile"
	"github.com/d0lim/aaswap/internal/paths"
	"github.com/d0lim/aaswap/internal/settings"
	"github.com/d0lim/aaswap/internal/usagestore"
)

// BackupWritten announces a replaced backup credential, if anyone is listening.
// Exported because import writes credentials from its own package.
func (s *Switcher) BackupWritten(accountNum, email string) {
	if s.OnBackupWritten != nil {
		s.OnBackupWritten(accountNum, email)
	}
}

// RosterFileName is the roster's name inside the backup root.
const RosterFileName = paths.RosterFileName

// LockFileName guards every roster mutation. One lock for the whole store: the
// operations that matter — adding, removing, relocating, switching — touch the
// roster and the credential backups together, and a per-slot lock would let two
// of them interleave into a roster that names credentials nobody wrote.
const LockFileName = ".aaswap.lock"

// IdentityOracle resolves an access token to the account it belongs to.
//
// Narrow on purpose: this package asks the network exactly one question, and a
// test that needs to answer it should not have to stand up a whole client.
type IdentityOracle interface {
	Profile(ctx context.Context, accessToken string) *claudeapi.Identity
}

// TokenRefresher exchanges a refresh token for a rotated credential.
type TokenRefresher interface {
	Refresh(ctx context.Context, credentials string, now time.Time) claudeapi.RefreshOutcome
}

// UsageFetcher measures one account's rate-limit usage.
type UsageFetcher interface {
	FetchUsageForAccount(ctx context.Context, req claudeapi.FetchRequest) claudeapi.UsageOutcome
}

// Switcher owns one backup root.
//
// Its collaborators are fields rather than globals so a test can drive the
// whole surface against fakes, which is what replaces the Python suite's
// runtime audit hook: there is no ambient path into the developer's real store
// to guard against, because every path in is a field.
type Switcher struct {
	// Provider names the auth domain this switcher operates on. Empty means
	// Claude, which is what every store written before providers existed holds.
	//
	// One switcher per provider rather than a provider argument on every
	// method: the active account is per provider, and a call that could be
	// about either would have to be told which on every hop.
	Provider string

	// Paths answers every "where does that file live" question.
	Paths *paths.Resolver

	// Creds routes credentials between the Keychain and files.
	Creds *credstore.Store

	// Usage is the shared measurement table.
	Usage *usagestore.Store

	// Oracle answers "whose token is this". Nil disables the ownership check,
	// which is the documented fail-open behavior — a switch proceeds without
	// it — rather than an error.
	Oracle IdentityOracle

	// Fetcher measures an account's usage. Nil means no measurement is taken
	// and every account reads as unknown, never as exhausted.
	Fetcher UsageFetcher

	// Refresher POSTs a refresh-token grant. Nil defers every refresh rather
	// than reporting one as failed — an absent client is not evidence about a
	// token.
	Refresher TokenRefresher

	// OnBackupWritten, when set, is called after a slot's stored credential is
	// replaced — a re-login, an import, a relocation, a consumed refresh.
	//
	// It exists for session profiles. A profile seeded from the previous
	// generation can still pass the local reuse check while holding a token the
	// server has already rotated out, so something has to invalidate it. That
	// something needs to know about running processes and profile directories,
	// which is knowledge this package deliberately does not carry — hence a
	// callback rather than a session dependency.
	//
	// Best effort by contract: it returns nothing, and a credential write must
	// never fail because a profile could not be invalidated. The write is the
	// operation the user asked for.
	OnBackupWritten func(accountNum, email string)

	// Settings is the effective configuration after CLI overrides.
	Settings settings.Settings

	// Now is the clock, injected so a test can drive timestamps.
	//
	// It must be the SAME clock the usage store uses: the store decides
	// freshness, leases and backoff against its own now, and two clocks would
	// let a measurement read as fresh to one half of a collect pass and stale
	// to the other. Use [Switcher.SetClock] rather than assigning this
	// directly.
	Now func() time.Time

	// LockTimeout bounds how long a roster mutation waits for the lock.
	LockTimeout time.Duration

	// FetchStagger spaces the request starts of a parallel collect. Zero uses
	// [DefaultFetchStagger]; a negative value disables the spacing, which only
	// a test should ever want.
	FetchStagger time.Duration
}

// New returns a Switcher wired to the given paths.
func New(r *paths.Resolver) *Switcher {
	root := r.BackupRoot()
	client := claudeapi.New()
	return &Switcher{
		Paths:       r,
		Creds:       credstore.NewForProvider(r, root, keychain.New(), ProviderClaude),
		Usage:       usagestore.New(r.CacheDir()),
		Oracle:      client,
		Fetcher:     client,
		Refresher:   client,
		Settings:    settings.Defaults(),
		Now:         time.Now,
		LockTimeout: lockfile.DefaultTimeout,
	}
}

// SetClock points the Switcher and its usage store at one clock.
func (s *Switcher) SetClock(now func() time.Time) {
	s.Now = now
	if s.Usage != nil {
		s.Usage.Now = now
	}
}

func (s *Switcher) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

// BackupRoot is where aaswap keeps everything it owns.
func (s *Switcher) BackupRoot() string { return s.Paths.BackupRoot() }

// RosterPath is sequence.json's location.
func (s *Switcher) RosterPath() string {
	return filepath.Join(s.BackupRoot(), RosterFileName)
}

// LockPath guards roster mutations.
func (s *Switcher) LockPath() string {
	return filepath.Join(s.BackupRoot(), LockFileName)
}

// ConfigsDir holds each account's captured config, scoped to this provider.
//
// Scoped for the same reason the credentials are: two providers can hold one
// person's account under one name, and a shared directory would give whichever
// wrote last both accounts' configs.
func (s *Switcher) ConfigsDir() string {
	return filepath.Join(s.BackupRoot(), "configs", s.provider())
}

// legacyConfigsDir is where a store written before providers existed kept
// captured configs. Read by the upgrade, and by nothing else.
func (s *Switcher) legacyConfigsDir() string {
	return filepath.Join(s.BackupRoot(), "configs")
}

// withLock runs fn under the store lock.
//
// Every roster mutation goes through here. Reads do not: the roster is
// published by an atomic replace, so a lock-free reader sees one version or the
// next, never a mix.
func (s *Switcher) withLock(fn func() error) error {
	timeout := s.LockTimeout
	if timeout == 0 {
		timeout = lockfile.DefaultTimeout
	}
	return lockfile.With(s.LockPath(), timeout, fn)
}
