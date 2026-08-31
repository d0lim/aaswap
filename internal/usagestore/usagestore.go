// Package usagestore is the per-account usage table: last-known-good
// measurements plus the fetch state that schedules and throttles the next
// attempt.
//
// # Why a table and not a snapshot
//
// The measurement it replaces was an all-or-nothing snapshot with a 15-second
// shelf life, so one failed round trip blanked every account at once. Here a
// failure updates only the error and backoff fields and never touches the
// last-good measurement — stale-on-error. Every surface shares the table, so a
// fetch made for the list command also serves the auto-switch engine, and each
// learns from the other's requests.
//
// # What is persisted, and what is not
//
// Only measurements and fetch state. Sentinel states — "api key", "token
// expired", "keychain unavailable" — are re-derived by the collector on every
// pass and overlaid on the read model, never written, so a stale sentinel can
// never outlive the condition that produced it.
//
// # The locking protocol
//
// The lock is never held across network I/O:
//
//  1. lock, read, decide and claim the fetch set (stamping a bounded lease),
//     unlock;
//  2. fetch with no lock held;
//  3. lock, re-read, merge the outcomes, clear the claims, write, unlock.
//
// The lease lets a concurrent collector skip an account another process is
// still fetching, recording the outcome releases it, and a crashed claimer's
// lease ages out rather than blocking forever. Leases are recorded per batch,
// so a fast account's lease is held until its batch's slowest fetch lands; the
// TTL is sized for the whole batch, not one request.
package usagestore

import (
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/realiti4/claude-swap/internal/claudeapi"
	"github.com/realiti4/claude-swap/internal/fsutil"
	"github.com/realiti4/claude-swap/internal/lockfile"
	"github.com/realiti4/claude-swap/internal/usage"
)

// SchemaVersion is the on-disk format. A file at any other version — a
// version-less legacy snapshot, or one written by a future release — reads as
// empty rather than being interpreted, which costs one round of re-fetching and
// nothing else.
const SchemaVersion = 2

// StaleOK is how old a measurement may be and still be trusted for a switch
// decision. Older than this and headroom reads as unknown, unless the
// staleness is deliberate — see [Entry.TrustExtended].
//
// Freshness is the reader's judgment per purpose rather than one global TTL:
// pollpolicy.ServeTTL governs whether to fetch at all, and this governs whether
// to decide on what is already stored.
const StaleOK = 300 * time.Second

// ClaimTTL bounds a collector's fetch lease.
//
// It covers the bounded refresh-and-usage path, the full inventory's stagger,
// and queueing, so another surface cannot reclaim a request still in flight. A
// crashed collector's row waits this long and no longer, which stays well
// inside the provider-safe polling interval.
const ClaimTTL = 90 * time.Second

// LegacyClaimTTL reads a lease written by a collector predating the fenced
// form, which stamped only the attempt time.
const LegacyClaimTTL = 10 * time.Second

// TrustMaxAge is the hard ceiling on deliberate staleness.
//
// Failure backoff and a scheduler-chosen cadence both extend decision trust
// past [StaleOK], but never past this: a forever-failing account must
// eventually read as unknown so the unknown-path machinery — escalate-all,
// unhealthy ticks, verified failover — takes back over. It overrides even a
// Retry-After longer than itself, because trust must never be server-controlled
// and unbounded.
const TrustMaxAge = 3600 * time.Second

// RateLimitTrustMaxAge is the ceiling for data made stale by a 429
// specifically.
//
// A usage-endpoint 429 throttles polling; it does not move the account's real
// quota windows. Usage only rises within a window, so a frozen measurement
// stays a valid lower bound on the true usage right up to that window's reset.
// Flipping such an account to "unknown" on a short fixed clock made a
// merely-throttled account an unusable switch target and drove failover
// flapping. Trust is therefore data-driven — bounded by the earliest window
// reset — with this as the fallback ceiling for rows carrying no reset at all.
//
// A non-429 failure always uses [TrustMaxAge] instead: a timeout is no evidence
// the stored measurement still holds.
const RateLimitTrustMaxAge = 7200 * time.Second

// Failure backoff when the server sent no Retry-After: 30s x 2^(n-1), capped.
const (
	BackoffBase = 30 * time.Second
	BackoffCap  = 600 * time.Second

	// backoffMaxShift clamps the exponent, and the clamp is not cosmetic: a
	// permanently failing account increments its failure count forever, and
	// shifting a duration past 63 places overflows into a NEGATIVE wait — an
	// account that would then be retried instantly, forever.
	//
	// The curve reaches BackoffCap by the fifth shift, so any clamp at or above
	// that is behavior-preserving. This one leaves room to widen the cap
	// without revisiting it.
	backoffMaxShift = 8
)

// RetryAfterMargin is added to a long Retry-After before honoring it.
//
// Honoring the server's deadline exactly is not enough: the retry lands ON the
// deadline, where the server is not reliably ready, and a re-block earns a
// fresh full hour. Measured across this machine's log, 20 of 35 lapses
// re-blocked within 900s of their own deadline (+2s to +887s) and the next was
// +1004s — a bimodal distribution whose band edge this clears with 13 seconds
// to spare.
//
// The margin is absolute, not a fraction of the ask: Retry-After counts down to
// a fixed deadline, so a machine polling into a block another one opened sees
// only the remainder, and a fraction of that shrinks toward zero exactly when
// it matters most.
const RetryAfterMargin = 900 * time.Second

// RetryAfterFloorCap bounds a rate-limited wait, so a pathological header can
// never park an account for hours.
//
// Sized to the measured shape: 40 of 41 observed blocks opened at exactly one
// hour, and 3600s + [RetryAfterMargin] is this. At an ask of exactly this value
// the margin is already fully eaten; beyond it the wait is shorter than the
// server asked, which costs one request rather than a fresh hour, since probing
// does not re-arm a block.
const RetryAfterFloorCap = 4500 * time.Second

// AuthDeadStrikes is how many permanent auth verdicts quarantine a slot.
//
// One is already definitive: the token endpoint explicitly rejected the grant,
// which no transient 429, timeout or network blip does, so there is nothing to
// gain by retrying — and each retry with a dead token just draws a fresh 401. A
// single success, or any path that rewrites the credential, resets the count
// and lifts the quarantine.
const AuthDeadStrikes = 1

// permanentAuthErrors are the fetch outcomes that prove the stored credential
// is unusable rather than merely unlucky. Only these advance the strike count;
// a transient error leaves it untouched, being no evidence the token is alive
// or dead.
func permanentAuthError(kind claudeapi.ErrorKind) bool {
	return kind == claudeapi.KindInvalidGrant || kind == claudeapi.KindNoRefreshToken
}

// Identity is who a slot number currently maps to. A row whose stored identity
// differs from the caller's is invisible to reads and replaced on write, so
// reusing a slot never serves the previous account's usage.
type Identity struct {
	Email            string
	OrganizationUUID string
}

// row is one account's stored state.
//
// Field spelling is an on-disk contract: the Python implementation reads and
// writes the same table during the migration. Absent and null are equivalent
// for every optional field, so omitting one on write is compatible with a
// reader that expects an explicit null.
type row struct {
	Email            string `json:"email"`
	OrganizationUUID string `json:"organizationUuid"`

	LastGood  *usage.Result `json:"lastGood,omitzero"`
	FetchedAt *float64      `json:"fetchedAt,omitzero"`

	LastAttemptAt       *float64            `json:"lastAttemptAt,omitzero"`
	ConsecutiveFailures int                 `json:"consecutiveFailures,omitzero"`
	LastError           claudeapi.ErrorKind `json:"lastError,omitzero"`
	BackoffUntil        *float64            `json:"backoffUntil,omitzero"`

	NextPollAt    *float64 `json:"nextPollAt,omitzero"`
	PollIntervalS *float64 `json:"pollIntervalS,omitzero"`

	Last429At *float64 `json:"last429At,omitzero"`

	AuthDeadStrikes   int    `json:"authDeadStrikes,omitzero"`
	StruckFingerprint string `json:"struckFingerprint,omitzero"`

	ClaimID string `json:"claimId,omitzero"`
	// ClaimUntil is written as an explicit zero when a lease is released, not
	// omitted. An absent value means "written by a collector predating the
	// fenced lease", which sends liveClaim down the legacy path and would keep
	// a just-released row looking claimed.
	ClaimUntil *float64 `json:"claimUntil"`
}

// table is the file's top level.
type table struct {
	SchemaVersion int             `json:"schemaVersion"`
	Accounts      map[string]*row `json:"accounts"`
}

// matches reports whether a stored row still belongs to the given account.
func (r *row) matches(identity Identity) bool {
	return r != nil && r.Email == identity.Email && r.OrganizationUUID == identity.OrganizationUUID
}

func freshRow(identity Identity) *row {
	return &row{Email: identity.Email, OrganizationUUID: identity.OrganizationUUID}
}

// Store is the usage table on disk.
//
// Writes are read-modify-write under a lock file beside the table; reads are
// lock-free, which is safe because every write is an atomic replace.
//
// Every method takes the caller's current slot-to-identity map and touches only
// rows for those slots. Rows for slots outside the map are left alone, so a
// caller operating on one account — the status command, say — cannot disturb
// the rest of the table.
type Store struct {
	path     string
	lockPath string

	// Now is the clock. Injected so tests can drive the lease and backoff
	// windows without sleeping.
	Now func() time.Time

	// NewClaimID mints a lease's fencing token. Injected for the same reason.
	NewClaimID func() string

	// LockTimeout bounds how long a write waits for the lock.
	LockTimeout time.Duration
}

// New returns a store over the given cache directory.
func New(cacheDir string) *Store {
	return &Store{
		path:        filepath.Join(cacheDir, "usage.json"),
		lockPath:    filepath.Join(cacheDir, ".usage.lock"),
		Now:         time.Now,
		NewClaimID:  newClaimID,
		LockTimeout: lockfile.DefaultTimeout,
	}
}

// Path is the table's location, for diagnostics.
func (s *Store) Path() string { return s.path }

// newClaimID mints a fencing token. Uniqueness is all that is required: a lease
// is only ever compared for equality against the one the claimer holds.
func newClaimID() string {
	var b [16]byte
	for i := range b {
		b[i] = byte(rand.UintN(256))
	}
	return fmt.Sprintf("%x", b)
}

// readRows loads the table, returning an empty one for anything unreadable.
//
// A missing, corrupt, truncated or wrong-version file all read as empty. That
// is the right answer for a cache: the cost is one round of re-fetching, and
// refusing to start because a throwaway file is malformed would be strictly
// worse.
func (s *Store) readRows() map[string]*row {
	data, err := fsutil.ReadText(s.path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			slog.Debug("usage table unreadable; starting from empty", "error", err)
		}
		return map[string]*row{}
	}
	var parsed table
	if err := json.Unmarshal([]byte(data), &parsed); err != nil {
		slog.Debug("usage table malformed; starting from empty", "error", err)
		return map[string]*row{}
	}
	if parsed.SchemaVersion != SchemaVersion || parsed.Accounts == nil {
		return map[string]*row{}
	}
	return parsed.Accounts
}

func (s *Store) writeRows(rows map[string]*row) error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("creating the cache directory: %w", err)
	}
	return fsutil.WriteJSONAtomic(s.path, table{SchemaVersion: SchemaVersion, Accounts: rows})
}

// withLock runs fn under the table's write lock.
func (s *Store) withLock(fn func() error) error {
	if err := os.MkdirAll(filepath.Dir(s.lockPath), 0o700); err != nil {
		return fmt.Errorf("creating the cache directory: %w", err)
	}
	return lockfile.With(s.lockPath, s.LockTimeout, fn)
}
