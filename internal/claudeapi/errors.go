package claudeapi

import "fmt"

// ErrorKind is a short, stable token naming why a token or usage exchange
// failed.
//
// It is a string and not an error value on purpose: kinds are persisted in the
// usage store as a slot's `last_error`, compared across processes, and rendered
// to users, so their spelling is part of the on-disk contract. The set is open —
// an unrecognized transport failure contributes its own kind rather than being
// flattened — so callers must switch on the constants below and treat anything
// else as an opaque label.
type ErrorKind string

// Refresh-grant verdicts.
const (
	// KindInvalidGrant means the token endpoint rejected the grant: this
	// slot's refresh-token lineage is dead and re-login is required. Callers
	// quarantine on it, so nothing ambiguous may map here.
	KindInvalidGrant ErrorKind = "invalid_grant"

	// KindInvalidClient means ccswap's OAuth client credential was rejected.
	// Systemic — it says nothing about any one slot, so it lands no strike.
	KindInvalidClient ErrorKind = "invalid_client"

	// KindNoRefreshToken means the stored credential is a structurally
	// complete OAuth object that genuinely carries no refresh token. Also
	// permanent for retry purposes.
	KindNoRefreshToken ErrorKind = "no_refresh_token"

	// KindTransient is every failure that may resolve on its own: network
	// blips, 5xx, and — deliberately — any 4xx body ccswap could not parse a
	// verdict out of. A misclassified transient costs one retry; a
	// misclassified permanent wrongly quarantines a live account.
	KindTransient ErrorKind = "transient"
)

// Pre-request failures, named distinctly from the transport kinds so a fetch
// that never left the machine is not mistaken for a server verdict.
const (
	// KindNoAccessToken means the credential carries no access token to send.
	KindNoAccessToken ErrorKind = "no-access-token"

	// KindRefreshFailed is the generic "the refresh did not produce usable
	// credentials" kind, used only where the underlying verdict is transient
	// or unclassified.
	KindRefreshFailed ErrorKind = "refresh-failed"
)

// Deterministic refusals raised by a caller-supplied refresh gate (the switcher
// consume gate, in practice). Each names a condition that will not resolve
// within this pass, which is why [FetchUsageForAccount] refuses to fall through
// to the usage endpoint after one.
const (
	// KindStoreUnmirrored means the secure-storage config dir override is in
	// effect, so the store ccswap would write is not the one Claude Code reads.
	KindStoreUnmirrored ErrorKind = "store-unmirrored"

	// KindConsumeBusy means another ccswap surface holds this slot's gate.
	KindConsumeBusy ErrorKind = "consume-busy"

	// KindStashUnreadable means the slot's stashed successor credential cannot
	// be read, so the gate cannot tell which generation is current.
	KindStashUnreadable ErrorKind = "stash-unreadable"
)

// Transport kinds.
const (
	KindTimeout     ErrorKind = "timeout"
	KindNetwork     ErrorKind = "network"
	KindBadResponse ErrorKind = "bad-response"
)

// HTTPKind names an HTTP status failure, e.g. "http-429".
func HTTPKind(status int) ErrorKind { return ErrorKind(fmt.Sprintf("http-%d", status)) }

// deterministicRefreshKinds are refresh failures that will NOT resolve by
// retrying within this pass, so the caller must not fall through to the usage
// endpoint carrying a token it already knows is expired.
//
// KindConsumeBusy belongs here for a reason only visible in hindsight: the
// retry re-enters the same gate, finds it still held, and the distinct kind
// arrives as a generic KindRefreshFailed — hiding the remedy, and spending a
// guaranteed 401 per pass to learn nothing.
var deterministicRefreshKinds = []ErrorKind{
	KindStoreUnmirrored,
	KindInvalidClient,
	KindConsumeBusy,
	KindStashUnreadable,
}

// permanentRefreshKinds are verdicts about the refresh-token lineage itself:
// server-rejected or structurally absent. A caller may quarantine the slot on
// these, so the set is deliberately small.
var permanentRefreshKinds = []ErrorKind{KindInvalidGrant, KindNoRefreshToken}

// errorNotes is the user-facing remedy for each kind that has one.
//
// Every deterministic kind must appear here. Keeping such a kind distinct from
// KindRefreshFailed is only worth doing because of the note it carries — a kind
// with no note renders as a bare identifier, which is strictly worse than the
// generic string it displaced. The test in this package enforces that.
var errorNotes = map[ErrorKind]string{
	KindStoreUnmirrored: "CLAUDE_SECURESTORAGE_CONFIG_DIR set — unset it or run from a normal shell",
	KindInvalidClient:   "ccswap's OAuth client was rejected — systemic, not this account",
	KindConsumeBusy:     "another ccswap surface holds the slot — retries next pass",
	KindStashUnreadable: "this slot's stashed successor is unreadable — unlock the keychain " +
		"or fix the file, then retry; `ccswap unclaimed` inspects it",
}

// Note returns the user-facing explanation for a kind, falling back to the kind
// itself so an unrecognized one still renders something.
func Note(kind ErrorKind) string {
	if note, ok := errorNotes[kind]; ok {
		return note
	}
	return string(kind)
}

// KindUnknown is the fallback for a failure that fits none of the classified
// shapes. It is retryable — an unrecognized failure is not evidence of a
// permanent one — and its presence in a log means this package has a gap.
const KindUnknown ErrorKind = "unknown"
