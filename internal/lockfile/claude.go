package lockfile

import (
	"path/filepath"

	"github.com/d0lim/aaswap/internal/paths"
)

// Cooperating with Claude Code's own advisory locks while mutating its files.
//
// Claude Code guards its OAuth token refresh with the npm proper-lockfile
// package, and its ~/.claude.json writes with the same mechanism on the config
// file. The protocol, verified against the 2.1.218 bundle:
//
//   - The lock artifact is a directory; mkdir's atomicity is the mutex.
//   - The refresh path takes *two* locks, in order: the primary
//     <config-home>/.oauth_refresh.lock, then the legacy <config-home>.lock
//     (~/.claude.lock) kept for compatibility with external tools. Both run
//     stale: 60000, update: 5000 — a credential lock is stale only past 60s,
//     and live holders touch every 5s. On a contended legacy lock, Claude Code
//     releases the primary and retries.
//   - The config lock (~/.claude.json.lock) keeps the older defaults: stale
//     after 10s, touched every 5s.
//   - Claude Code retries a held credentials lock 5 times with 1-2s jittered
//     sleeps before giving up, so briefly holding it is fully cooperative.
//
// Holding these while swapping credentials closes the one real race with a
// running Claude Code: its refresh reads credentials, refreshes over the
// network, and saves — all under both credential locks — so a swap landing
// inside that window would be overwritten by the refreshed old-account token,
// and the backup just taken would keep a pre-rotation refresh token. Under the
// lock, Claude Code's own double-checked re-read sees the swapped (non-expired)
// credential and aborts its refresh instead.
//
// References (claude-code 2.1.218 bundle): the uKi lock-options helper
// (lockfilePath: join(dir, ".oauth_refresh.lock"), stale: 60000, update: 5000)
// and CKi (dual acquisition, legacy released on contention with
// tengu_oauth_refresh_legacy_lock_contended telemetry).

// CredentialsLockDir returns the legacy credential lock, ~/.claude.lock.
// Claude Code still takes it for compatibility, and external exclusion today
// rests on this one.
func CredentialsLockDir(r *paths.Resolver) string {
	home := r.ClaudeConfigHome()
	return filepath.Join(filepath.Dir(home), filepath.Base(home)+".lock")
}

// OAuthRefreshLockDir returns Claude Code's primary OAuth refresh lock,
// <config-home>/.oauth_refresh.lock (2.1.218+).
func OAuthRefreshLockDir(r *paths.Resolver) string {
	return filepath.Join(r.ClaudeConfigHome(), ".oauth_refresh.lock")
}

// ConfigLockDir returns the lock guarding the global config file,
// ~/.claude.json.lock.
func ConfigLockDir(r *paths.Resolver) string {
	path := r.GlobalConfigPath()
	return filepath.Join(filepath.Dir(path), filepath.Base(path)+".lock")
}

// WithClaudeCredentials runs fn while holding Claude Code's credential-refresh
// locks, in Claude Code's own order.
//
// Mirroring both the pair and the order means a waiting aaswap and a waiting
// Claude Code can never deadlock against each other, and exclusion still holds
// after Claude Code drops the legacy lock. Both use Claude Code's 60s
// staleness: never steal a lock a live Claude Code may still hold.
func WithClaudeCredentials(r *paths.Resolver, opts ProperOptions, fn func() error) error {
	opts.Staleness = CredentialsStaleness

	primary, err := acquireProper(OAuthRefreshLockDir(r), opts)
	if err != nil {
		return err
	}
	// On legacy contention Claude Code releases the primary before retrying;
	// releasing ours on the same edge keeps the two implementations symmetric
	// and is what prevents a mutual wait.
	legacy, err := acquireProper(CredentialsLockDir(r), opts)
	if err != nil {
		primary.release()
		return err
	}
	defer primary.release()
	defer legacy.release()
	return fn()
}

// WithClaudeConfig runs fn while holding Claude Code's global-config write lock
// (~/.claude.json.lock), which keeps the older 10s staleness.
func WithClaudeConfig(r *paths.Resolver, opts ProperOptions, fn func() error) error {
	opts.Staleness = ConfigStaleness
	return withProper(ConfigLockDir(r), opts, fn)
}
