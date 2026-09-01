package swap

import (
	"context"
	"strings"

	"github.com/d0lim/ccswap/internal/claudeapi"
)

// OutgoingKind is what a switch may do with the live credential it is about to
// replace.
//
// The question is not "did the bytes change" — Claude Code rotates them
// routinely — but "whose bytes are these". The live credential and the config
// that names it are two files with independent writers, and when they disagree,
// copying the live bytes into the slot the config names destroys that slot's
// only refresh token. That happened in the field; this taxonomy is the fix.
type OutgoingKind string

const (
	// KindOwnBytes: byte-identical to the slot's stored backup. Nothing
	// changed, so nothing needs capturing.
	KindOwnBytes OutgoingKind = "own-bytes"

	// KindOwnFamily: the same refresh-token lineage, with the access token
	// rotated. This slot's credential; back it up.
	KindOwnFamily OutgoingKind = "own-family"

	// KindOwnRotated: a full rotation, and the endpoint resolved the live token
	// to this slot's identity. Backing it up is what keeps slots alive across
	// Claude Code's routine refresh-token rotations.
	KindOwnRotated OutgoingKind = "own-rotated"

	// KindForeign: positively resolved to ANOTHER managed slot holding a
	// different lineage. Never written into any slot, and never destroyed —
	// identity proves ownership, not which generation is fresher.
	KindForeign OutgoingKind = "foreign"

	// KindForeignSynced: another managed slot's bytes, and that slot already
	// holds this exact lineage. Nothing needs preserving, nothing may be
	// written.
	KindForeignSynced OutgoingKind = "foreign-synced"

	// KindWiped: an OAuth blob whose token fields are all empty. Claude Code
	// empties them in place when a refresh is rejected, keeping the wrapper and
	// the metadata. With no token the oracle is structurally silent, so this
	// used to fall through to unresolved — and the fail-open backup then copied
	// the empty strings over the slot's only surviving refresh token. There is
	// nothing here to preserve and nothing that may be written.
	KindWiped OutgoingKind = "wiped"

	// KindAlien: a structurally complete identity — uuid, address and
	// organization — matching no managed slot. An unmanaged login, or a
	// recycled address wearing a managed one. Preserved.
	KindAlien OutgoingKind = "alien"

	// KindKnownForeign: the switch-time lookup failed, but an earlier probe in
	// this process already condemned this exact lineage. Preserved like an
	// alien: a transient lookup failure must not let the fail-open backup
	// poison a slot with bytes already proven foreign — this switch may BE the
	// repair that verdict triggered.
	KindKnownForeign OutgoingKind = "known-foreign"

	// KindUnresolved: the bytes differ and ownership could not be established,
	// or was only partially established. The caller falls back to the exact
	// pre-taxonomy backup, because the oracle is advisory and endpoint state
	// must never change whether a switch completes.
	KindUnresolved OutgoingKind = "unresolved"
)

// Preserve reports whether this verdict means the live credential must be
// stashed rather than filed or discarded.
func (k OutgoingKind) Preserve() bool {
	switch k {
	case KindForeign, KindAlien, KindKnownForeign:
		return true
	}
	return false
}

// WritesCredential reports whether the live credential may be written into the
// departing slot's backup.
func (k OutgoingKind) WritesCredential() bool {
	switch k {
	case KindOwnFamily, KindOwnRotated, KindUnresolved:
		return true
	}
	return false
}

// Provenance is what a pre-lock lookup established about the live credential.
//
// Resolved is only trustworthy while Live has not moved, which is why the
// under-lock classifier re-checks byte equality before using it.
type Provenance struct {
	Live     string
	Resolved *claudeapi.Identity
}

// PrefetchProvenance resolves the live credential's owner BEFORE any lock is
// taken.
//
// The switch-time backup copies live bytes into the slot the config names, and
// those are two files with independent writers. When they agree — same bytes,
// or same refresh lineage as the slot's stored backup — no network is needed at
// all. When they diverge, only the endpoint can say whose token the live bytes
// are, because the credential itself carries no identity.
//
// It happens here, and not under the lock, because no network call may be made
// while Claude Code's credential lock is held: the exchange can block for
// seconds, and Claude Code would block behind it.
func (s *Switcher) PrefetchProvenance(ctx context.Context, roster *Roster) Provenance {
	active := s.Creds.ReadActive()
	provenance := Provenance{Live: active.Value}
	if active.Value == "" {
		return provenance
	}

	identity, ok := s.LiveIdentity()
	if !ok {
		return provenance
	}
	slot, managed := roster.FindSlot(identity.Identity())
	if !managed {
		return provenance
	}

	backup, _ := s.Creds.ReadAccount(slot, identity.Email)
	if backup == active.Value ||
		claudeapi.Fingerprint(backup) == claudeapi.Fingerprint(active.Value) {
		// Provenance is already established locally; the network has nothing to
		// add.
		return provenance
	}

	token := claudeapi.AccessToken(active.Value)
	if token == "" || s.Oracle == nil {
		// A raw API key, a garbled blob, or no oracle at all. Nothing to
		// resolve, and the classifier fails open.
		return provenance
	}
	provenance.Resolved = s.Oracle.Profile(ctx, token)
	return provenance
}

// ClassifyOutgoing decides what the switch-time backup may do with the live
// credential.
//
// foreignSlot names the other managed slot the credential resolved to, and is
// meaningful only for the two foreign verdicts.
func (s *Switcher) ClassifyOutgoing(
	roster *Roster,
	currentSlot, currentEmail, liveCredentials string,
	provenance Provenance,
	condemned func(fingerprint string) bool,
) (kind OutgoingKind, foreignSlot string) {
	backup, _ := s.Creds.ReadAccount(currentSlot, currentEmail)
	if backup != "" && backup == liveCredentials {
		return KindOwnBytes, ""
	}
	if backup != "" && claudeapi.Fingerprint(backup) == claudeapi.Fingerprint(liveCredentials) {
		return KindOwnFamily, ""
	}

	if payload, ok := claudeapi.OAuthPayload(liveCredentials); ok {
		access, _ := payload["accessToken"].(string)
		if access == "" && claudeapi.RefreshToken(payload) == "" {
			return KindWiped, ""
		}
	}

	resolved := provenance.Resolved
	if resolved == nil || provenance.Live != liveCredentials {
		// Either the lookup failed, or the bytes moved since it ran — in which
		// case its answer is about a credential that is no longer here.
		if condemned != nil && condemned(claudeapi.Fingerprint(liveCredentials)) {
			return KindKnownForeign, ""
		}
		return KindUnresolved, ""
	}

	own := roster.Accounts[currentSlot]
	ownUUID, ownOrg := "", ""
	if own != nil {
		ownUUID, ownOrg = own.UUID, own.OrganizationUUID
	}
	resolvedUUID := strings.TrimSpace(resolved.UUID)

	// The outgoing slot's own uuid is checked first: it survives a response
	// that dropped the address, and an account whose address changed. The
	// organization has to agree only when both sides record one — this
	// codebase's usual leniency about organizations.
	if resolvedUUID != "" && ownUUID != "" && resolvedUUID == ownUUID &&
		(resolved.OrganizationUUID == "" || ownOrg == "" || resolved.OrganizationUUID == ownOrg) {
		return KindOwnRotated, ""
	}

	slot := s.attributeSlot(roster, resolved)
	if slot == currentSlot {
		return KindOwnRotated, ""
	}
	if slot == "" {
		// A positive "alien" needs a structurally complete identity — an
		// address AND an organization — matching nothing. A partial one is
		// indistinguishable from a renamed field, and preserve-and-skip on that
		// would silently recreate the fail-closed behavior this design forbids.
		if resolved.Email != "" && resolved.OrganizationUUID != "" {
			return KindAlien, ""
		}
		return KindUnresolved, ""
	}

	// Naming another slot in user-facing output must be uuid-positive. An
	// address-and-organization match against a slot with no recorded uuid is
	// not evidence enough to accuse it.
	storedUUID := strings.TrimSpace(roster.Accounts[slot].UUID)
	if resolvedUUID == "" || storedUUID != resolvedUUID {
		return KindAlien, ""
	}

	foreignBackup, _ := s.Creds.ReadAccount(slot, roster.Accounts[slot].Email)
	if foreignBackup != "" && (foreignBackup == liveCredentials ||
		claudeapi.Fingerprint(foreignBackup) == claudeapi.Fingerprint(liveCredentials)) {
		return KindForeignSynced, slot
	}
	return KindForeign, slot
}

// attributeSlot finds the managed slot a resolved identity names.
func (s *Switcher) attributeSlot(roster *Roster, resolved *claudeapi.Identity) string {
	resolvedUUID := strings.TrimSpace(resolved.UUID)

	slot := ""
	if resolved.Email != "" {
		if num, ok := roster.FindSlot(Identity{
			Email:            resolved.Email,
			OrganizationUUID: resolved.OrganizationUUID,
		}); ok {
			slot = num
		}
	}
	if slot != "" && resolvedUUID != "" {
		// When both sides carry a uuid it must agree. An address-and-org match
		// with a conflicting uuid is a DIFFERENT account wearing a recycled
		// address — a deleted and recreated login — and treating it as the slot
		// would poison that slot's backup.
		if stored := strings.TrimSpace(roster.Accounts[slot].UUID); stored != "" && stored != resolvedUUID {
			slot = ""
		}
	}
	if slot == "" && resolvedUUID != "" {
		// Fall back to the account uuid, scoped by organization, in case a
		// slot's stored address is stale or synthesized — an add-token
		// placeholder, for instance.
		for _, num := range roster.Numbers() {
			account := roster.Accounts[num]
			if account.UUID != "" && account.UUID == resolvedUUID &&
				account.OrganizationUUID == resolved.OrganizationUUID {
				return num
			}
		}
	}
	return slot
}
