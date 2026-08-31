package autoswitch

import (
	"context"
	"log/slog"

	"github.com/realiti4/claude-swap/internal/claudeapi"
	"github.com/realiti4/claude-swap/internal/jsonout"
	"github.com/realiti4/claude-swap/internal/session"
	"github.com/realiti4/claude-swap/internal/swap"
)

// freshenVerdict is what happened when the engine tried to make a candidate's
// stored token usable.
type freshenVerdict int

const (
	// freshenOK: the token will outlive Claude Code's own refresh buffer.
	freshenOK freshenVerdict = iota
	// freshenDead: the refresh lineage is gone. Only a re-login helps, so the
	// account is quarantined rather than retried forever.
	freshenDead
	// freshenIdentityConflict: the token is alive but authenticates as a
	// DIFFERENT account. Activating it would put the user on the wrong account
	// with every gauge reading normal.
	freshenIdentityConflict
	// freshenSkipLiveSession: a `ccswap run` session owns this account's token in
	// its own profile.
	freshenSkipLiveSession
	// freshenSystemic: a deterministic refusal every candidate hits identically.
	freshenSystemic
	// freshenTransient: network trouble. Try again next tick.
	freshenTransient
)

// freshen makes sure a candidate's stored token outlives Claude Code's refresh
// buffer before it is activated.
//
// Only ever touches the slot's BACKUP store. The active credential belongs to
// Claude Code, and rotating it here would invalidate the token a running editor
// is holding.
func (e *Engine) freshen(ctx context.Context, num string, account *swap.Account) (freshenVerdict, claudeapi.ErrorKind) {
	if account.AuthKind() == swap.KindAPIKey {
		return freshenOK, "" // API keys do not expire
	}

	// A live session owns this account's token in its own profile. Activating
	// it as the default login too would put one rotating refresh token in two
	// config directories — the stale-copy failure — with nobody reading the
	// warning, and its quota is already being consumed there anyway. A manual
	// switch keeps its warn-and-proceed behavior; the engine skips.
	sessions := &session.Manager{
		BackupRoot: e.Switcher.BackupRoot(),
		Platform:   e.Switcher.Paths.Platform,
		Creds:      e.Switcher.Creds,
	}
	if len(sessions.LivePIDs(num, account.Email)) > 0 {
		return freshenSkipLiveSession, ""
	}

	credentials, _ := e.Switcher.Creds.ReadAccount(num, account.Email)
	if credentials == "" {
		// Absent or merely unreadable, both transient here: a locked Keychain
		// clears, and a slot with no stored credential is not something to
		// quarantine over — the roster already excludes it from the switchable
		// set, so reaching this means it appeared and went between two reads.
		return freshenTransient, ""
	}
	payload, ok := claudeapi.OAuthPayload(credentials)
	if !ok {
		// Not an OAuth credential at all, and not a recognized API key either.
		// Nothing here can be refreshed or activated.
		return freshenDead, ""
	}

	expiry, stated := claudeapi.ExpiresAt(payload)
	if stated && expiry.After(e.now().Add(FreshenBuffer)) {
		return freshenOK, ""
	}
	if !stated {
		// No stated expiry: nothing to pre-empt, and spending a grant to find
		// out would cost a generation for no information.
		return freshenOK, ""
	}

	// The consume gate serializes every backup-token POST: it re-reads under
	// the slot lock — this snapshot may already be superseded — and persists
	// through a compare-and-swap, so a freshen racing the collector cannot
	// double-spend one grant.
	outcome := e.Switcher.ConsumeBackupGrant(ctx, num, account.Email, credentials)
	if outcome.OK() {
		if e.noteTokenIdentity(num, account, outcome.TokenAccount) {
			return freshenIdentityConflict, ""
		}
		return freshenOK, ""
	}
	if outcome.Permanent() {
		return freshenDead, ""
	}
	if _, systemic := systemicMessage(outcome.Error); systemic {
		return freshenSystemic, outcome.Error
	}
	return freshenTransient, outcome.Error
}

// noteTokenIdentity uses the token endpoint's free identity to verify or
// backfill a slot, reporting a CONFLICT.
//
// The grant just ran against the slot's own stored credential, so this names who
// that credential really is. A conflict means activating it would log the user
// into a different account than the one they chose.
//
// The organization is compared first, whenever both sides record one: a
// wrong-organization credential is evidence the slot holds the wrong account,
// and backfilling ITS uuid would poison the slot's identity record permanently —
// a backfill never rewrites a non-empty uuid, so that corruption would stick.
func (e *Engine) noteTokenIdentity(num string, account *swap.Account, identity *claudeapi.Identity) bool {
	if identity == nil || identity.UUID == "" {
		return false
	}
	if identity.OrganizationUUID != "" && account.OrganizationUUID != "" &&
		identity.OrganizationUUID != account.OrganizationUUID {
		return true
	}
	if account.UUID == "" {
		// A placeholder record learning its real identity while the roster is
		// open anyway. Never fatal: the successor credential is already
		// persisted by the time this runs.
		if err := e.backfillUUID(num, identity.UUID); err != nil {
			slog.Debug("could not backfill a slot's uuid", "account", num, "error", err)
		}
		return false
	}
	return account.UUID != identity.UUID
}

// backfillUUID records an identity the token endpoint supplied for free.
func (e *Engine) backfillUUID(num, uuid string) error {
	roster, err := e.Switcher.RosterOrEmpty()
	if err != nil {
		return err
	}
	account, exists := roster.Accounts[num]
	if !exists || account.UUID != "" {
		return nil
	}
	account.UUID = uuid
	roster.LastUpdated = swap.Timestamp(e.now())
	return e.Switcher.WriteRoster(roster)
}

// quarantine holds an account out of rotation until its credential is replaced.
func (e *Engine) quarantine(num, email, reason string) error {
	credentials, _ := e.Switcher.Creds.ReadAccount(num, email)
	fingerprint := ""
	if credentials != "" {
		fingerprint = claudeapi.Fingerprint(credentials)
	}

	if _, err := e.State.Mutate(func(state *State) {
		if state.Quarantine == nil {
			state.Quarantine = map[string]*QuarantineEntry{}
		}
		state.Quarantine[num] = &QuarantineEntry{
			Email:                   email,
			Reason:                  reason,
			At:                      jsonout.Timestamp(e.now()),
			RefreshTokenFingerprint: fingerprint,
		}
	}); err != nil {
		return err
	}

	event := e.event(KindQuarantine)
	event.Number, event.Email, event.Reason = num, email, reason
	e.emit(event)
	return nil
}

// releaseRecovered drops quarantine entries whose credential has been replaced.
//
// A changed lineage fingerprint — or a slot that changed hands — means the user
// logged in again and re-captured the account. The dead lineage is gone, so it
// re-enters rotation on its own, with no command to remember.
func (e *Engine) releaseRecovered(state State) (State, error) {
	if len(state.Quarantine) == 0 {
		return state, nil
	}
	roster, err := e.Switcher.RosterOrEmpty()
	if err != nil {
		return state, err
	}

	type release struct{ num, email, reason string }
	var releases []release

	for num, entry := range state.Quarantine {
		account, exists := roster.Accounts[num]
		if !exists || account.Email != entry.Email {
			releases = append(releases, release{num, entry.Email, "account-replaced"})
			continue
		}
		credentials, _ := e.Switcher.Creds.ReadAccount(num, account.Email)
		fingerprint := ""
		if credentials != "" {
			fingerprint = claudeapi.Fingerprint(credentials)
		}
		if fingerprint != entry.RefreshTokenFingerprint {
			releases = append(releases, release{num, account.Email, "credentials-replaced"})
		}
	}
	if len(releases) == 0 {
		return state, nil
	}

	updated, err := e.State.Mutate(func(s *State) {
		for _, r := range releases {
			delete(s.Quarantine, r.num)
		}
	})
	if err != nil {
		return state, err
	}
	for _, r := range releases {
		event := e.event(KindUnquarantine)
		event.Number, event.Email, event.Reason = r.num, r.email, r.reason
		e.emit(event)
	}
	return updated, nil
}
