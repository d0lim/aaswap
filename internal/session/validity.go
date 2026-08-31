package session

import (
	json "encoding/json/v2"
	"log/slog"
	"os"
)

// Verdict is what a probe established about a profile.
//
// Four values, not two, because "the profile is bad" and "the probe failed" are
// different facts with opposite consequences: the caller that acts on Invalid
// deletes the profile's Keychain item and the whole directory, and a probe that
// could not run is no evidence about the profile at all.
type Verdict string

const (
	// Valid means the probe ran and the profile authenticates as its account.
	Valid Verdict = "valid"

	// Invalid means the probe ran and the profile does NOT. This is the only
	// verdict that licenses destroying a profile.
	Invalid Verdict = "invalid"

	// Unknown means the probe RAN and did not answer — a timeout, or output
	// that would not parse. Nothing here suggests the profile is bad, so reuse
	// leans valid: on a loaded machine Claude Code's cold start exceeds the
	// timeout, and forcing a re-seed there fails a launch that would have
	// worked.
	Unknown Verdict = "unknown"

	// Unreachable means Claude Code could not be started at all. Reuse leans
	// INVALID here, and for the opposite reason: a binary that cannot run
	// cannot serve the session either way, so the honest outcome is the error
	// that says to check the PATH — not a silent reuse.
	Unreachable Verdict = "unreachable"
)

// AuthStatus is what Claude Code's local auth check reports.
type AuthStatus struct {
	LoggedIn   bool   `json:"loggedIn"`
	AuthMethod string `json:"authMethod"`
	Email      string `json:"email"`
	OrgID      string `json:"orgId"`
}

// Prober asks Claude Code whether a profile can authenticate.
//
// An interface because the real implementation spawns a process, and no test
// should need one to exercise the four verdicts.
type Prober interface {
	// AuthStatus runs the local check against a profile. It returns Unreachable
	// when the binary could not be started, Unknown when it ran without
	// answering, and the parsed status otherwise.
	AuthStatus(sessionDir string) (AuthStatus, Verdict)
}

// Validity is the full verdict for a profile.
//
// The check is LOCAL — it makes no API call — so a revoked but unexpired token
// still passes here and fails on first real use. That is the honest limit of
// what can be established without spending a request.
func (m *Manager) Validity(sessionDir string, want Identity) Verdict {
	if info, err := os.Stat(sessionDir); err != nil || !info.IsDir() {
		return Invalid
	}
	if m.Probe == nil {
		// No way to ask. That establishes nothing, which is Unknown — not
		// Invalid, which would license destroying the profile.
		return Unknown
	}

	status, verdict := m.Probe.AuthStatus(sessionDir)
	if verdict != "" {
		return verdict
	}
	switch {
	case !status.LoggedIn:
		return Invalid
	case status.AuthMethod != "claude.ai":
		// An environment API key reports a different method — and the probe's
		// environment already drops those variables, so seeing one means the
		// profile itself is not an account login.
		return Invalid
	case status.Email != want.Email:
		return Invalid
	case status.OrgID != "" && want.OrganizationUUID != "" && status.OrgID != want.OrganizationUUID:
		// Lenient: compared only when both sides state one, so a renamed field
		// degrades to an address check instead of producing false negatives.
		return Invalid
	}
	return Valid
}

// Usable is the REUSE-shaped view of a verdict: whether a profile is good
// enough to launch into, as far as anything can tell.
//
// Unknown is decided from local artifacts rather than answered with a blanket
// yes, because a probe that timed out is not by itself a reason to reuse: the
// credential must not be definitely absent, and the recorded identity must not
// have drifted. Anything subtler still fails on first real use.
//
// Unreachable answers no WITHOUT consulting artifacts: the question there is
// not whether the profile holds a credential but whether Claude Code can be run
// at all, and no local file answers that.
//
// A caller that DESTROYS on a negative must use [Manager.Validity] instead. For
// it, the difference between "invalid" and "could not tell" is the difference
// between a correct cleanup and deleting a working profile, and a boolean
// cannot carry that.
func (m *Manager) Usable(sessionDir string, want Identity) bool {
	switch m.Validity(sessionDir, want) {
	case Valid:
		return true
	case Unknown:
		return m.ArtifactsSayUsable(sessionDir, want)
	}
	return false
}

// ArtifactsSayUsable is what local files can say about a profile without a
// probe.
//
// Deliberately NOT consulted for Unreachable — see [Manager.Usable].
func (m *Manager) ArtifactsSayUsable(sessionDir string, want Identity) bool {
	return m.MayHaveCredentialMaterial(sessionDir) && !m.IdentityDrifted(sessionDir, want)
}

// parseAuthStatus decodes the probe's output, reporting Unknown for anything
// that will not parse.
func parseAuthStatus(stdout []byte) (AuthStatus, Verdict) {
	var status AuthStatus
	if err := json.Unmarshal(stdout, &status); err != nil {
		slog.Debug("the auth probe's output would not parse", "error", err)
		return AuthStatus{}, Unknown
	}
	return status, ""
}
