package swap

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/d0lim/aaswap/internal/apperr"
	providerpkg "github.com/d0lim/aaswap/internal/provider"
)

// loginDirName is where login sandboxes live under the backup root.
const loginDirName = "login"

// LoginSandbox is a throwaway home the provider's tool logs into.
//
// This is how aaswap logs someone in without touching the login they have. The
// tool owns its OAuth flow and nothing here reimplements it; what aaswap owns
// is WHERE the flow lands. Pointed at a directory nobody else reads, the tool
// runs its ordinary login, the credential and identity land there, and aaswap
// files them as an account and deletes the directory. The live login is never
// read, never overwritten, and never logged out of — which is what made the
// old way dangerous: current Claude Code revokes the refresh token on /logout,
// so "log out, log in as the other account" destroyed the stored one.
type LoginSandbox struct {
	// Home is the directory the tool is pointed at.
	Home string

	owner   *Switcher
	sandbox *Switcher
	// before is the owner's live credential when the sandbox was opened, and
	// hadLive whether an account was logged in at all. See FinishLogin.
	before  string
	hadLive bool
}

// BeginLogin opens a sandbox for one login.
//
// Under the backup root rather than the system temp directory: it holds a live
// credential for the seconds between the login and the filing, and the backup
// root is already the place on this machine that holds those, with the modes
// to match.
func (s *Switcher) BeginLogin() (*LoginSandbox, error) {
	spec := s.spec()
	if spec.Home.Env == "" {
		return nil, fmt.Errorf("%w: %s has no home variable, so its login cannot be "+
			"pointed anywhere but the live profile. Log in with the tool, then run "+
			"`aaswap login --capture`", apperr.ErrConfig, spec.DisplayName())
	}
	if spec.Login == nil || len(spec.Login.Argv) == 0 {
		return nil, fmt.Errorf("%w: %s declares no login command",
			apperr.ErrConfig, spec.DisplayName())
	}
	var nonce [6]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, fmt.Errorf("%w: naming the login sandbox: %w", apperr.ErrConfig, err)
	}
	home := filepath.Join(s.BackupRoot(), loginDirName, spec.Name+"-"+hex.EncodeToString(nonce[:]))
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("%w: creating the login sandbox: %w", apperr.ErrConfig, err)
	}
	_, hadLive := s.LiveIdentity()
	return &LoginSandbox{
		Home:    home,
		owner:   s,
		sandbox: s.at(home),
		before:  s.Creds.ReadActive().Value,
		hadLive: hadLive,
	}, nil
}

// Argv is the login command to run, pointed at the sandbox by Environment.
func (sb *LoginSandbox) Argv() []string {
	return sb.owner.spec().Login.Argv
}

// Environment is the environment to run the login command in: the tool's home
// variable pointed at the sandbox, and every override that could send the
// credential anywhere else scrubbed.
func (sb *LoginSandbox) Environment(base []string) []string {
	env, _ := providerpkg.Environment(base, sb.Home, sb.owner.spec())
	// Not in the declaration's override list because it is not an auth
	// override: it relocates Claude Code's Keychain item alone, and a sandbox
	// that inherited it would log into the caller's live store.
	return withoutEnv(env, "CLAUDE_SECURESTORAGE_CONFIG_DIR")
}

// Discard removes the sandbox and everything the login left in it. Safe to
// call more than once, and after FinishLogin.
func (sb *LoginSandbox) Discard() {
	if sb == nil || sb.Home == "" {
		return
	}
	if sb.sandbox != nil {
		sb.sandbox.Creds.DiscardActive()
	}
	if err := os.RemoveAll(sb.Home); err != nil {
		slog.Warn("could not remove a login sandbox", "path", sb.Home, "error", err)
	}
	sb.Home = ""
}

// FinishLogin files what the login left in the sandbox as an account, then
// discards the sandbox.
//
// The account is made live only when nothing was: on a fresh machine "log in"
// means "and use it", while on one with a live login that login is exactly what
// the sandbox existed to leave alone. The outcome says which happened.
func (s *Switcher) FinishLogin(ctx context.Context, sb *LoginSandbox, req AddRequest) (AddOutcome, error) {
	defer sb.Discard()
	if sb.owner != s {
		return AddOutcome{}, fmt.Errorf("%w: the login sandbox belongs to another switcher", apperr.ErrConfig)
	}
	var name string
	if req.Name != "" {
		normalized, err := NormalizeName(req.Name)
		if err != nil {
			return AddOutcome{}, err
		}
		name = normalized
	}

	if err := s.reclaimSharedStore(sb); err != nil {
		return AddOutcome{}, err
	}
	identity, ok := sb.sandbox.LiveIdentity()
	if !ok {
		return AddOutcome{}, fmt.Errorf("%w: the login ended without a %s account landing. "+
			"Nothing was changed. If the browser step was completed, the tool may have "+
			"written elsewhere: log in with it directly and run `aaswap login --capture`",
			apperr.ErrConfig, s.spec().DisplayName())
	}

	var outcome AddOutcome
	err := s.withLock(func() error {
		roster, err := s.RosterOrEmpty()
		if err != nil {
			return err
		}
		outcome, err = s.addFrom(ctx, sb.sandbox, roster, req, name, identity, false)
		return err
	})
	if err != nil || outcome.Cancelled || sb.hadLive {
		return outcome, err
	}
	// Discard before activating: Switch reads the live store, and the sandbox's
	// copy must not be the second thing on this machine holding this token.
	sb.Discard()
	if _, err := s.Switch(ctx, SwitchRequest{Target: outcome.Name}); err != nil {
		// Not a failure of the login: the account is stored, and reporting
		// this as an error would tell the person to log in again — which
		// would file the same account again and fail the same way.
		outcome.ActivationFailed = err.Error()
		return outcome, nil
	}
	outcome.Activated = true
	return outcome, nil
}

// reclaimSharedStore handles a tool that ignored the sandbox and wrote its
// credential into the live store anyway.
//
// Older Claude Code did exactly that on macOS: CLAUDE_CONFIG_DIR moved the
// config but the Keychain item stayed the shared one. The live login is then
// clobbered, and the sandbox holds the identity of a credential it does not
// hold. Both halves are put right — the live store gets its previous credential
// back, and the sandbox gets the one the login produced, so the filing below
// reads it the ordinary way.
func (s *Switcher) reclaimSharedStore(sb *LoginSandbox) error {
	after := s.Creds.ReadActive().Value
	if after == "" || after == sb.before {
		return nil
	}
	if sb.before != "" {
		if err := s.Creds.WriteActive(sb.before); err != nil {
			return fmt.Errorf("%w: the login overwrote the live credential and it could "+
				"not be restored: %w", apperr.ErrCredentialWrite, err)
		}
	} else {
		s.Creds.DiscardActive()
	}
	return sb.sandbox.Creds.WriteActive(after)
}

// at is this switcher with the provider's home relocated: the same store, the
// same roster, the same clock, reading its LIVE files somewhere else.
func (s *Switcher) at(home string) *Switcher {
	spec := s.spec()
	r := s.Paths.WithProviderHome(spec.Home.Env, home)
	clone := *s
	clone.Paths = r
	clone.Creds = s.Creds.At(r, liveLayoutFor(r, spec))
	return &clone
}

func withoutEnv(env []string, name string) []string {
	out := env[:0:0]
	for _, entry := range env {
		if key, _, _ := cutEnv(entry); key != name {
			out = append(out, entry)
		}
	}
	return out
}

func cutEnv(entry string) (name, value string, found bool) {
	for i := range len(entry) {
		if entry[i] == '=' {
			return entry[:i], entry[i+1:], true
		}
	}
	return entry, "", false
}
