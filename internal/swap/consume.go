package swap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/lockfile"
)

// demotingStashReasons are the stash outcomes that mean the successor is PARKED
// rather than persisted, so the slot still holds the generation whose grant was
// just spent.
//
// A caller reading "no error" as "the slot is freshened and safe to activate"
// would then install an expired access token that can never refresh, and Claude
// Code logs the account out. Reporting these as transient makes the caller
// defer; the next pass adopts the stash and succeeds normally.
//
// Two stash reasons are deliberately absent, for opposite reasons. A REMOVED
// slot has nothing left to activate or retry, so deferring would turn a
// completed user action into a recurring error. A CAS CONFLICT means the slot
// IS freshened — a racing writer won and wrote a newer valid lineage, which was
// adopted and is exactly what the caller asked for.
var demotingStashReasons = []string{
	"consume-gate-persist-failed",
	"consume-gate-persist-lock-failed",
	"consume-gate-unpersisted",
	"consume-gate-store-unreadable",
}

// ConsumeBackupGrant is the gate through which a slot's backup refresh token is
// consumed.
//
// A refresh token is one-time-use, so the POST must spend the provably-freshest
// copy of the slot's grant — never the caller's snapshot, which may be a
// superseded generation. The whole sequence runs under a per-slot CONSUME lock:
// re-read under the store lock, POST outside it, then compare-and-swap on the
// lineage fingerprint and either persist or stash.
//
// The store lock never covers the network call — nothing may hold a lock others
// contend for across a request — which is exactly the window a second gate used
// to slip through and POST the same one-time grant. The consume lock is
// contended only by other gates, so waiting on it IS the serialization wanted.
//
// A consumed generation is never discarded. The gate never fails after the
// grant is spent, because its callers run inside a collect pass that promises
// not to.
//
// The caller must NOT hold the store lock: this takes it, and it is not
// reentrant.
func (s *Switcher) ConsumeBackupGrant(ctx context.Context, accountNum, email, snapshot string) claudeapi.RefreshOutcome {
	// Claude Code honors a secure-storage override for its credential store;
	// aaswap mirrors that when CAPTURING a credential but not when consuming
	// one. Consuming a grant read from the default store while Claude Code
	// reads and writes a redirected one is the stale-copy failure by
	// construction — refuse rather than operate on a store it left behind.
	//
	// A distinct kind, because it is deterministic and self-inflicted: a
	// transient would fall through to a guaranteed 401 every pass and read as
	// generic network trouble forever.
	if s.Paths.SecureStorageConfigDir != "" {
		slog.Warn("CLAUDE_SECURESTORAGE_CONFIG_DIR is set; aaswap mirrors it when "+
			"capturing a credential but not when consuming one, so it refuses to "+
			"consume this account's refresh token", "account", accountNum)
		return claudeapi.RefreshOutcome{Error: claudeapi.KindStoreUnmirrored}
	}

	if err := os.MkdirAll(s.Creds.CredentialsDir(), 0o700); err != nil {
		slog.Warn("could not create the credentials directory for the consume lock",
			"account", accountNum, "error", err)
		return claudeapi.RefreshOutcome{Error: claudeapi.KindTransient}
	}
	consumeLock := lockfile.New(s.consumeLockPath(accountNum), 0)
	acquired, err := consumeLock.Acquire()
	if err != nil || !acquired {
		// Nothing failed and nothing is remote: another gate holds the slot and
		// will finish, and this pass simply yields. Its own kind, so a tick
		// error does not blame the network for local serialization working as
		// designed.
		slog.Info("another consume is in flight; deferring to the next pass",
			"account", accountNum, "error", err)
		return claudeapi.RefreshOutcome{Error: claudeapi.KindConsumeBusy}
	}
	defer func() { _ = consumeLock.Release() }()

	return s.consumeLocked(ctx, accountNum, email, snapshot)
}

func (s *Switcher) consumeLockPath(accountNum string) string {
	return filepath.Join(s.Creds.CredentialsDir(), fmt.Sprintf(".consume-%s.lock", accountNum))
}

// consumeLocked is the gate's body; the caller holds the consume lock.
func (s *Switcher) consumeLocked(ctx context.Context, accountNum, email, snapshot string) claudeapi.RefreshOutcome {
	var refreshInput string

	// Phase one, under the store lock: establish the freshest copy of this
	// slot's grant. Nothing has been consumed yet, so every failure here defers
	// cleanly.
	err := s.withLock(func() error {
		current, unreadable := s.Creds.ReadAccount(accountNum, email)
		if unreadable {
			// The backup may exist but cannot be seen. The snapshot is exactly
			// the possibly-superseded copy this gate exists never to consume.
			slog.Info("the stored credential is unreadable; deferring the refresh",
				"account", accountNum)
			return errDeferTransient
		}

		adopted, err := s.adoptStashedSuccessor(accountNum, email, current)
		if err != nil {
			// The slot's only successor is unreadable. Deferring is right: the
			// bytes are the SOLE copy of a generation this slot already
			// consumed, and nothing on disk tells "locked for a minute" from
			// "locked forever". What must not stay is the LABEL — a generic
			// transient renders as "network?", sending the operator to check a
			// connection that is fine, forever, on a condition only they can
			// clear.
			slog.Info("the stashed successor is unreadable; deferring the refresh",
				"account", accountNum, "error", err)
			return errDeferStashUnreadable
		}
		if adopted != "" {
			current = adopted
		}
		if current == "" {
			// ABSENT, not unreadable: the slot was removed between the caller's
			// read and this locked re-read. Falling back to the snapshot would
			// spend a grant for an account the user just deleted, and stash a
			// successor keyed to a generation no slot holds — which nothing
			// could ever adopt.
			slog.Info("the stored credential is gone; deferring rather than consuming a "+
				"grant for a slot that no longer exists", "account", accountNum)
			return errDeferTransient
		}
		refreshInput = current
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, errDeferStashUnreadable):
			return claudeapi.RefreshOutcome{Error: claudeapi.KindStashUnreadable}
		case errors.Is(err, errDeferTransient):
			return claudeapi.RefreshOutcome{Error: claudeapi.KindTransient}
		}
		// A lock holder, or a torn roster. No grant of ours is outstanding, so
		// deferring costs a pass and spends nothing.
		slog.Warn("the pre-consume window failed; deferring", "account", accountNum, "error", err)
		return claudeapi.RefreshOutcome{Error: claudeapi.KindTransient}
	}

	consumedFP := claudeapi.Fingerprint(refreshInput)

	// The world may already have moved past the caller's snapshot. When it has
	// AND the current generation is fresh, the refresh the caller wanted has
	// effectively happened — a racing gate's rotation, or an adopted stash —
	// and consuming another grant on top would burn a generation for nothing.
	//
	// When the re-read EQUALS the snapshot, this never fires and the POST
	// proceeds: that is the 401-retry shape, where the server just rejected
	// these exact bytes.
	if payload, ok := claudeapi.OAuthPayload(refreshInput); ok {
		access, _ := payload["accessToken"].(string)
		snapAccess := claudeapi.AccessToken(snapshot)
		if access != "" && snapAccess != "" && access != snapAccess &&
			!claudeapi.Expired(payload, s.now()) {
			return claudeapi.RefreshOutcome{Credentials: refreshInput, ConsumedFP: consumedFP}
		}
	}

	// The POST, with no lock held.
	result := s.refreshClient().Refresh(ctx, refreshInput, s.now())
	if !result.OK() {
		// Strike binding follows the POSTed bytes: the gate may have
		// substituted a locked re-read for the caller's snapshot, and failures
		// are the only outcomes that strike.
		result.ConsumedFP = consumedFP
		return result
	}

	return s.persistSuccessor(accountNum, email, consumedFP, result)
}

// persistSuccessor commits the rotated credential, or parks it where the next
// pass will adopt it.
//
// The grant is already spent by this point, so nothing here may fail outward:
// every path ends in either a persisted successor or a stashed one, and the
// caller is told which.
func (s *Switcher) persistSuccessor(accountNum, email, consumedFP string, result claudeapi.RefreshOutcome) claudeapi.RefreshOutcome {
	outcomeCreds := result.Credentials
	stashedReason := ""

	stash := func(reason, note string) {
		// A consumed generation is never discarded: park the successor where
		// the next pass adopts it. ConsumedFP is the adoption key — the
		// generation this successor superseded.
		if _, err := s.Creds.WriteUnclaimed(result.Credentials, credstore.StashEntry{
			Reason:      reason,
			ConfigSlot:  accountNum,
			ConsumedFP:  consumedFP,
			Fingerprint: claudeapi.Fingerprint(result.Credentials),
		}, s.now()); err != nil {
			// Both the persist and the stash failed. The successor survives
			// only in the returned credentials.
			stashedReason = "consume-gate-unpersisted"
			slog.Error("the consumed successor could not be persisted or stashed — it "+
				"survives only for this pass. Fix the storage failure, then log in again "+
				"and run `aaswap login --capture` if the account strikes",
				"account", accountNum, "error", err)
			return
		}
		stashedReason = reason
		slog.Warn(note, "account", accountNum)
	}

	err := s.withLock(func() error {
		storeNow, unreadable := s.Creds.ReadAccount(accountNum, email)
		switch {
		case unreadable:
			// The Keychain locked during the POST. The compare-and-swap cannot
			// be evaluated — writing back could clobber a racing writer, and an
			// empty read would otherwise look like "the slot was emptied",
			// whose reason is deliberately NOT demoting. The grant IS spent and
			// the slot may still hold the generation that spent it, so this
			// must stash AND demote.
			stash("consume-gate-store-unreadable",
				"the stored credential was unreadable after a refresh POST; the "+
					"successor was stashed and nothing was rewritten")
		case storeNow == "":
			// The slot was emptied mid-POST. Writing the successor back would
			// resurrect credentials the user just deleted.
			stash("consume-gate-slot-removed",
				"the stored credential disappeared during a refresh POST; the "+
					"successor was stashed and nothing was rewritten")
		case claudeapi.Fingerprint(storeNow) != consumedFP:
			// A writer replaced the lineage while the POST was in flight: stash
			// this successor and adopt the store's newer credential.
			stash("consume-gate-cas-conflict",
				"the backup lineage moved during a refresh POST; the successor was "+
					"stashed and the newer store credential adopted")
			outcomeCreds = storeNow
		default:
			if err := s.Creds.WriteAccount(accountNum, email, result.Credentials); err != nil {
				return err
			}
			s.BackupWritten(accountNum, email)
		}
		return nil
	})
	if err != nil {
		// The grant IS consumed, so the successor must survive even though the
		// persist could not complete.
		stash("consume-gate-persist-failed",
			"persisting the refreshed credential failed; the successor was stashed "+
				"for the next pass")
	}

	if slices.Contains(demotingStashReasons, stashedReason) {
		return claudeapi.RefreshOutcome{
			Credentials:  outcomeCreds,
			Error:        claudeapi.KindTransient,
			TokenAccount: result.TokenAccount,
			ConsumedFP:   consumedFP,
			// The unpersisted reason is set precisely WHEN the stash write
			// failed, so it is the one demoting reason that parked nothing.
			Stashed: stashedReason != "consume-gate-unpersisted",
		}
	}
	return claudeapi.RefreshOutcome{
		Credentials:  outcomeCreds,
		TokenAccount: result.TokenAccount,
		ConsumedFP:   consumedFP,
	}
}

// adoptStashedSuccessor completes a prior gate's failed persist.
//
// A stash entry records the generation its credential superseded. When the slot
// still stores exactly that generation, the stored token is already consumed
// and the stash holds its live successor: writing it back IS the pending
// persist, and it saves consuming a grant at all.
//
// Returns the adopted credentials, or empty when nothing applies. The caller
// holds the store lock.
func (s *Switcher) adoptStashedSuccessor(accountNum, email, current string) (string, error) {
	currentFP := claudeapi.Fingerprint(current)
	if currentFP == "" {
		return "", nil
	}

	entries, verdict := s.Creds.ListUnclaimed()
	if verdict == credstore.StashUnreadable ||
		(verdict == credstore.StashCorrupt && s.Creds.StashEntryFilesExist()) {
		// Not "nothing stashed": the rows cannot be established and entry bytes
		// are at risk. Every row this scan would have read is the sole record
		// of a generation some pass already consumed, so an empty scan would
		// make the caller POST the slot's spent generation.
		//
		// That POST does not cost "one retry". The generation is spent by
		// construction, so it returns invalid_grant, the gate returns before
		// any manifest write, nothing self-heals — and at one strike a live
		// account is quarantined while its successor sits orphaned on disk.
		return "", fmt.Errorf("%w: the unclaimed manifest is %s and stashed entry files "+
			"exist; deferring adoption rather than POSTing a generation a stashed "+
			"successor may already have superseded (`aaswap account unclaimed` lists "+
			"them, "+
			"`--purge` drops one)", apperr.ErrCredentialRead, verdict)
	}

	// A row unreadable THIS instant must not abort the scan before a later,
	// readable sibling on the same generation is tried: repeated persist
	// failures can stash more than one row against one generation.
	deferred := ""

	for _, entryID := range credstore.SortedStashIDs(entries) {
		entry := entries[entryID]
		if entry.ConfigSlot != accountNum {
			continue
		}
		if entry.ConsumedFP != currentFP {
			s.retireUnadoptable(entryID, entry, accountNum)
			continue
		}

		credentials, unreadable := s.Creds.ReadUnclaimed(entryID)
		if unreadable {
			// This entry is the SOLE copy of a generation a prior pass already
			// consumed and could not persist. Falling through would make the
			// caller POST the slot's spent generation — an unrecoverable
			// rejection for an account whose live credential is sitting right
			// here, merely unreadable this instant.
			deferred = entryID
			continue
		}
		if credentials == "" {
			continue
		}

		if err := s.Creds.WriteAccount(accountNum, email, credentials); err != nil {
			// The successor is still safely stashed; the next pass retries.
			slog.Warn("could not adopt a stashed successor", "account", accountNum, "error", err)
			return "", nil
		}
		s.BackupWritten(accountNum, email)
		// Housekeeping, and never fatal: the slot has already advanced.
		if err := s.Creds.DeleteUnclaimed(entryID); err != nil {
			slog.Warn("could not retire an adopted stash entry",
				"account", accountNum, "entry", entryID, "error", err)
		}
		slog.Info("adopted a stashed successor, completing a prior failed persist",
			"account", accountNum, "entry", entryID)
		return credentials, nil
	}

	if deferred != "" {
		return "", fmt.Errorf("%w: account %s's stashed successor %s could not be read",
			apperr.ErrCredentialRead, accountNum, deferred)
	}
	return "", nil
}

// retireUnadoptable drops a stash row no pass could ever adopt.
func (s *Switcher) retireUnadoptable(entryID string, entry *credstore.StashEntry, accountNum string) {
	switch entry.Reason {
	case "consume-gate-cas-conflict":
		// A conflict entry can NEVER match: the conflict is by definition "the
		// store moved off the generation we consumed", and the store only moves
		// forward. Left alone these accumulate one file per conflict, each
		// indistinguishable from an entry still awaiting adoption.
		//
		// Retiring is safe: the gate adopted the store's newer lineage in the
		// same breath, so this successor branches off a generation that lineage
		// already superseded.
		if err := s.Creds.DeleteUnclaimed(entryID); err == nil {
			slog.Info("retired a conflict stash entry: its generation was superseded by "+
				"the writer that won the race, so no pass can adopt it",
				"account", accountNum, "entry", entryID)
		}
	default:
		value, unreadable := s.Creds.ReadUnclaimed(entryID)
		if unreadable || value != "" {
			// A merely unreadable row survives: its bytes may hold a real
			// superseded token, so dropping it stays the operator's call.
			return
		}
		// No bytes and no matching generation: nothing can ever adopt this row.
		// This is the state a failed retire leaves — the bytes are unlinked
		// before the manifest rewrite, so a failure there orphans the row.
		if err := s.Creds.DeleteUnclaimed(entryID); err == nil {
			slog.Info("retired a byte-less stash entry: its credential is gone and its "+
				"generation has passed", "account", accountNum, "entry", entryID)
		}
	}
}

// refreshClient is the token-endpoint client, or a no-op that defers when none
// is configured.
func (s *Switcher) refreshClient() refresher {
	if s.Refresher != nil {
		return s.Refresher
	}
	return offlineRefresher{}
}

// refresher performs the token-endpoint POST.
type refresher interface {
	Refresh(ctx context.Context, credentials string, now time.Time) claudeapi.RefreshOutcome
}

// offlineRefresher stands in when no client is configured, deferring rather
// than pretending a grant failed.
type offlineRefresher struct{}

func (offlineRefresher) Refresh(context.Context, string, time.Time) claudeapi.RefreshOutcome {
	return claudeapi.RefreshOutcome{Error: claudeapi.KindTransient}
}

// Sentinel errors for the pre-consume phase, so its deferral reason survives
// the lock helper's single error return.
var (
	errDeferTransient       = errors.New("deferring the refresh")
	errDeferStashUnreadable = errors.New("the stashed successor is unreadable")
)
