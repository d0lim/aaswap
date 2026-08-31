package lockfile

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/realiti4/claude-swap/internal/apperr"
)

// Timings from Claude Code's own lock options (verified against the 2.1.218
// bundle's uKi helper).
const (
	// CredentialsStaleness matches Claude Code's `stale: 60000` on the
	// credential-refresh locks. A lock younger than this belongs to a live
	// holder and must never be stolen: the holder's toucher can stall well past
	// 10s (machine suspend, a blocked event loop) while it still legitimately
	// owns the lock.
	CredentialsStaleness = 60 * time.Second

	// ConfigStaleness matches the older proper-lockfile defaults that Claude
	// Code keeps for ~/.claude.json.lock: stale after 10s, touched every 5s.
	ConfigStaleness = 10 * time.Second

	// TouchInterval is how often we refresh our own lock's mtime while holding
	// it. Deliberately faster than Claude Code's 5s, for margin.
	TouchInterval = 3 * time.Second

	// DefaultProperTimeout bounds the wait for one lock. Claude Code holds the
	// credentials lock for a single token-endpoint round trip (sub-second to a
	// few seconds) and its config lock for a local read-modify-write, so 9s of
	// bounded waiting comfortably outlasts both without stalling the CLI
	// forever. Note this is a *per-lock* budget: WithClaudeCredentials takes
	// two locks in sequence, so its worst case is roughly twice this.
	DefaultProperTimeout = 9 * time.Second
)

// ProperOptions tunes one directory-lock acquisition. The zero value means
// "use the defaults".
type ProperOptions struct {
	// Timeout bounds the wait for the lock. Zero means DefaultProperTimeout.
	Timeout time.Duration
	// Staleness is the age past which a held lock is presumed abandoned and may
	// be taken over. Zero means ConfigStaleness.
	Staleness time.Duration
	// TouchInterval is how often the held lock's mtime is refreshed. Zero means
	// TouchInterval. Tests shorten it; production should not.
	TouchInterval time.Duration
	// Logger receives warnings about locks that vanished while held. Nil means
	// slog.Default().
	Logger *slog.Logger
}

func (o ProperOptions) withDefaults() ProperOptions {
	if o.Timeout == 0 {
		o.Timeout = DefaultProperTimeout
	}
	if o.Staleness == 0 {
		o.Staleness = ConfigStaleness
	}
	if o.TouchInterval == 0 {
		o.TouchInterval = TouchInterval
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return o
}

// heldProperLock is an acquired proper-lockfile directory lock, together with
// the goroutine keeping its mtime fresh.
type heldProperLock struct {
	dir    string
	logger *slog.Logger
	stop   chan struct{}
	done   chan struct{}
}

// acquireProper takes a proper-lockfile-compatible directory lock.
//
// The lock artifact is a *directory*: mkdir's atomicity is the mutex, which is
// what makes this interoperable with the npm proper-lockfile package Claude
// Code uses. A lock whose mtime is older than the staleness budget is presumed
// abandoned and taken over.
func acquireProper(dir string, opts ProperOptions) (*heldProperLock, error) {
	opts = opts.withDefaults()

	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return nil, fmt.Errorf("create lock parent for %s: %w: %w", dir, apperr.ErrLock, err)
	}

	deadline := time.Now().Add(opts.Timeout)
	for {
		err := os.Mkdir(dir, 0o700)
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return nil, fmt.Errorf("create lock %s: %w: %w", dir, apperr.ErrLock, err)
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf(
				"could not acquire %s — Claude Code appears to be refreshing "+
					"credentials; retry in a few seconds: %w",
				filepath.Base(dir), apperr.ErrClaudeCodeLockTimeout)
		}

		info, err := os.Stat(dir)
		if err != nil {
			// The holder released between our mkdir and our stat. Retry at once.
			continue
		}
		if time.Since(info.ModTime()) > opts.Staleness {
			// A dead holder, per the protocol: remove and retake. Losing the
			// rmdir/mkdir race to another waiter just means looping again.
			if err := os.Remove(dir); err != nil {
				time.Sleep(50 * time.Millisecond) // can't remove it either; don't spin hot
			}
			continue
		}
		// Jittered so two waiters do not synchronize on the same retry beat.
		time.Sleep(250*time.Millisecond + time.Duration(rand.N(250))*time.Millisecond)
	}

	h := &heldProperLock{
		dir:    dir,
		logger: opts.Logger,
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go h.keepFresh(opts.TouchInterval)
	return h, nil
}

// keepFresh touches the lock directory's mtime so other holders do not deem us
// stale while we are doing real work.
func (h *heldProperLock) keepFresh(interval time.Duration) {
	defer close(h.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-h.stop:
			return
		case <-ticker.C:
			now := time.Now()
			if err := os.Chtimes(h.dir, now, now); err != nil {
				return // lock stolen or removed; nothing left to keep alive
			}
		}
	}
}

// release stops the toucher and removes the lock directory.
func (h *heldProperLock) release() {
	close(h.stop)
	select {
	case <-h.done:
	case <-time.After(time.Second):
		// The toucher is wedged on a stalled filesystem. Removing the directory
		// anyway is still correct: a stray Chtimes on a missing path just makes
		// the goroutine return.
	}
	switch err := os.Remove(h.dir); {
	case err == nil:
	case errors.Is(err, fs.ErrNotExist):
		h.logger.Warn("lock vanished while held (taken over as stale?)", "lock", h.dir)
	default:
		h.logger.Warn("failed to release lock", "lock", h.dir, "error", err)
	}
}

// withProper runs fn while holding the directory lock at dir.
func withProper(dir string, opts ProperOptions, fn func() error) error {
	h, err := acquireProper(dir, opts)
	if err != nil {
		return err
	}
	defer h.release()
	return fn()
}
