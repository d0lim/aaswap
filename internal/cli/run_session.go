package cli

import (
	"cmp"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/d0lim/aaswap/internal/apperr"
	"github.com/d0lim/aaswap/internal/mappings"
	"github.com/d0lim/aaswap/internal/provider"
	"github.com/d0lim/aaswap/internal/session"
	"github.com/d0lim/aaswap/internal/swap"
	"github.com/spf13/cobra"
)

// runCommand launches Claude Code as a chosen account without disturbing the
// machine's default login.
func (a *App) runCommand() *cobra.Command {
	var share, shareHistory bool
	cmd := &cobra.Command{
		Use:   "run [NUM|EMAIL|ALIAS] [-- CLAUDE ARGS...]",
		Short: "Launch Claude Code as one account, leaving the default login alone",
		Long: "With no account, the working directory decides: `aaswap map` remembers which\n" +
			"account a directory belongs to, and the nearest mapped ancestor wins.\n" +
			"An unmapped directory launches Claude Code exactly as typing `claude` would.\n\n" +
			"Everything after `--` is passed through to Claude Code.",
		Example: "  aaswap run 2\n" +
			"  aaswap run work -- --resume\n" +
			"  aaswap run                      # whichever account this directory maps to",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			identifier, claudeArgs := splitRunArgs(cmd, args)
			return a.runSession(cmd, identifier, claudeArgs, session.ShareOptions{
				Customizations: share, History: shareHistory,
			})
		},
	}
	cmd.Flags().BoolVar(&share, "share", true,
		"mirror the default profile's settings, skills, commands and agents")
	cmd.Flags().BoolVar(&shareHistory, "share-history", false,
		"also share conversation history with the default profile")
	silenceUsage(cmd)
	return cmd
}

// splitRunArgs separates the account from the arguments meant for Claude Code.
//
// Cobra records where `--` appeared, so `aaswap run -- --resume` passes the flag
// through instead of reading it as an account.
func splitRunArgs(cmd *cobra.Command, args []string) (identifier string, claudeArgs []string) {
	return splitRunArgsAt(args, cmd.ArgsLenAtDash())
}

// splitRunArgsAt is the split itself, given where cobra saw the dash.
func splitRunArgsAt(args []string, at int) (identifier string, claudeArgs []string) {
	if at < 0 {
		// No `--`: a single leading token is the account, and anything else is
		// a mistake cobra's argument check will not catch, so it goes to Claude
		// Code where the user can see it.
		if len(args) > 0 {
			return args[0], args[1:]
		}
		return "", nil
	}
	if at > 0 {
		return args[0], args[at:]
	}
	return "", args
}

// requireCapability refuses a command the addressed provider cannot support.
//
// The refusal comes from the provider's own declaration rather than from a
// hardcoded name, so a provider added later is refused for the right reason and
// a capability added later cannot be forgotten for an existing one. Every
// refusal names the provider, says what is missing, and points at the command
// that does work — a refusal with no way forward is a dead end.
func (a *App) requireCapability(s *swap.Switcher, capability provider.Capability) error {
	spec := provider.MustLookup(cmp.Or(s.Provider, swap.ProviderClaude))
	if spec.Can(capability) {
		return nil
	}
	return fmt.Errorf("%w: %s. Use `aaswap --provider %s switch` to change that "+
		"provider's active login instead",
		apperr.ErrConfig, spec.Why(capability), spec.Name)
}

func (a *App) runSession(cmd *cobra.Command, identifier string, claudeArgs []string, share session.ShareOptions) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	if err := a.requireCapability(s, provider.CapSession); err != nil {
		return err
	}
	spec := provider.MustLookup(cmp.Or(s.Provider, swap.ProviderClaude))

	if identifier == "" {
		resolved, found, err := a.resolveFromDirectory(s)
		if err != nil {
			return err
		}
		if !found {
			// No mapping: exactly what typing `claude` would do, with the
			// environment untouched.
			a.printer.Println(a.printer.Dimmed(fmt.Sprintf(
				"This directory maps to no account — launching %s as the default login.",
				spec.Name)))
			return a.execProvider(spec, claudeArgs, os.Environ())
		}
		identifier = resolved
	}

	num, account, err := s.ResolveAccount(identifier)
	if err != nil {
		return err
	}
	if account.AuthKind() == swap.KindAPIKey {
		return fmt.Errorf("%w: %s is an API-key account, which %s cannot "+
			"run in session mode", apperr.ErrSession, num, spec.Name)
	}

	manager := a.sessionManager(s)
	sessionDir := manager.Dir(num, account.Email)
	identity := session.Identity{Email: account.Email, OrganizationUUID: account.OrganizationUUID}

	// The same-account fast path: never make a second credential copy for the
	// account that IS the default login. Two copies of one account drift apart
	// the moment the server rotates the refresh token.
	if os.Getenv(spec.Session.HomeEnv) == "" {
		if live, ok := s.LiveIdentity(); ok && live.Identity() == account.Identity() {
			a.printer.Println(a.printer.Dimmed(fmt.Sprintf(
				"%s (%s) is already the default login — launching %s directly.",
				num, account.Email, spec.Name)))
			return a.execProvider(spec, claudeArgs, os.Environ())
		}
	} else {
		a.printer.Warning(fmt.Sprintf(
			"%s is already set; overriding it for this launch.", spec.Session.HomeEnv))
	}

	if err := a.prepareSession(manager, s, sessionDir, num, account, identity, spec); err != nil {
		return err
	}
	if err := manager.SyncSharing(sessionDir, defaultProfileDir(s), share); err != nil {
		return err
	}
	// Mirroring MCP definitions edits the profile's account-scoped config. A
	// provider with none keeps its servers in a machine-scoped file, and
	// writing there would rewrite the user's own settings for every account.
	if _, hasConfig := spec.ConfigFile(); hasConfig {
		if err := manager.SyncMCPServers(sessionDir, s.Paths.DefaultGlobalConfigPath(),
			share.Customizations); err != nil {
			return err
		}
	}

	env, scrubbed := session.Environment(os.Environ(), sessionDir, spec)
	if len(scrubbed) > 0 {
		a.printer.Warning(fmt.Sprintf("Ignoring %s for this session — it would override "+
			"the account inside %s.", strings.Join(scrubbed, ", "), spec.Name))
	}
	a.printer.Println(a.printer.Accent("Launching"), " ",
		fmt.Sprintf("%s (%s)", num, account.Email),
		a.printer.Muted(" ["+spec.Name+" session mode]"))
	return a.execProvider(spec, claudeArgs, env)
}

// prepareSession makes sure the profile is usable, seeding it when it is not.
func (a *App) prepareSession(manager *session.Manager, s *swap.Switcher, sessionDir, num string, account *swap.Account, identity session.Identity, spec provider.Spec) error {
	// A deferred invalidation is honored only when nothing is running: a second
	// launch joining a live session must not re-seed under it. The marker
	// survives for a later launch.
	stale := session.IsStale(sessionDir) && manager.Quiescent(num, account.Email)

	// A profile that wants refreshing but belongs to a provider whose running
	// sessions aaswap cannot see. Quiescent already answered false — the safe
	// direction — but silence would leave the user with a session on an old
	// credential and no idea why. Naming it is the whole difference between a
	// declared limitation and a bug.
	if !manager.CanDetectSessions() &&
		(session.IsStale(sessionDir) || manager.ProfileSuperseded(sessionDir, num, account.Email)) {
		a.printer.Warning(fmt.Sprintf(
			"This %s profile's credential is out of date, but aaswap cannot tell "+
				"whether a %s session is running against it, so it will not replace "+
				"it automatically. Close any running %s sessions and run "+
				"`aaswap --provider %s login` to refresh it.",
			spec.Name, spec.Name, spec.Name, spec.Name))
	}

	// Generation, not just identity. Usable asks whether the profile is logged
	// in as the right ACCOUNT; it cannot ask whether the credential is the one
	// the slot currently stores. A profile seeded before a rotation is the same
	// account holding a superseded token — well-formed, unexpired, and rejected
	// by the server on its first refresh.
	//
	// Invalidation at write time normally clears these, so this is the case
	// where that did not reach: another process wrote the backup, or the
	// invalidation itself failed. Only for a quiet profile — re-seeding under a
	// running session is the thing the stale marker exists to avoid.
	superseded := manager.Quiescent(num, account.Email) &&
		manager.ProfileSuperseded(sessionDir, num, account.Email)

	if !stale && !superseded && manager.Usable(sessionDir, identity) {
		return nil
	}

	if stale {
		session.ClearStale(sessionDir)
	}
	if err := manager.Bootstrap(sessionDir, num, account.Email); err != nil {
		return err
	}

	switch manager.Validity(sessionDir, identity) {
	case session.Valid:
		return nil
	case session.Unknown:
		// A probe that did not answer must not fail a launch that is fine: the
		// profile was just seeded from the stored credential, so the same local
		// artifacts that answer the reuse path answer this one.
		if manager.ArtifactsSayUsable(sessionDir, identity) {
			return nil
		}
		return fmt.Errorf("%w: the session profile for %s (%s) could not be "+
			"verified — `claude auth status` did not answer. The profile is left in "+
			"place; check that `claude` is on your PATH, then retry",
			apperr.ErrSession, num, account.Email)
	case session.Unreachable:
		return fmt.Errorf("%w: `claude` could not be run, so the session profile for "+
			"%s could not be verified. Check that Claude Code is installed and "+
			"on your PATH", apperr.ErrSession, num)
	}

	// Only a definite Invalid licenses destroying the profile.
	if err := manager.Remove(sessionDir); err != nil {
		return err
	}
	return fmt.Errorf("%w: the session profile for %s (%s) failed validation. Log "+
		"in with that account and re-add it: aaswap add --slot %s",
		apperr.ErrSession, num, account.Email, num)
}

// resolveFromDirectory maps the working directory to an account.
func (a *App) resolveFromDirectory(s *swap.Switcher) (string, bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", false, fmt.Errorf("%w: reading the working directory: %w", apperr.ErrConfig, err)
	}
	store := mappings.New(s.BackupRoot())
	_, entry, found := store.Resolve(cwd)
	if !found {
		return "", false, nil
	}

	roster, err := s.RosterOrEmpty()
	if err != nil {
		return "", false, err
	}
	num, exists := roster.FindName(swap.Identity{
		Email:            entry.Email,
		OrganizationUUID: entry.OrganizationUUID,
	})
	if !exists {
		// The mapping outlived its account. Say so rather than failing: the
		// user asked to run Claude Code, and the default login still can.
		a.printer.Warning(fmt.Sprintf(
			"This directory maps to %s, which is no longer managed. Remove the mapping "+
				"with `aaswap unmap`, or re-add the account.", entry.Email))
		return "", false, nil
	}
	return num, true, nil
}

func (a *App) sessionManager(s *swap.Switcher) *session.Manager {
	spec := provider.MustLookup(cmp.Or(s.Provider, swap.ProviderClaude))
	return &session.Manager{
		BackupRoot: s.BackupRoot(),
		Platform:   s.Paths.Platform,
		Creds:      s.Creds,
		Profiles:   s.Profiles,
		Spec:       spec,
		ConfigsDir: s.ConfigsDir(),
		Probe:      proberFor(spec),
		Now:        s.Now,
	}
}

// proberFor is the authentication probe for a provider, nil when there is none.
//
// The probe runs `claude auth status --json`, which is Claude Code's own
// command. Running it for another provider would ask the wrong binary a
// question it does not answer — and a probe that always fails is worse than no
// probe, because the manager resolves a nil one from local artifacts instead of
// treating the profile as broken.
func proberFor(spec provider.Spec) session.Prober {
	if spec.Name != swap.ProviderClaude {
		return nil
	}
	return session.ExecProber{Spec: spec}
}

// defaultProfileDir is the provider's own default home, which sharing always
// mirrors — even when the invoking shell is itself inside a session.
//
// The PROVIDER's, not Claude's: mirroring ~/.claude into a Codex profile would
// link one tool's settings and history into another's home, where they mean
// nothing and shadow the files that do.
func defaultProfileDir(s *swap.Switcher) string {
	spec := provider.MustLookup(cmp.Or(s.Provider, swap.ProviderClaude))
	if spec.Name == swap.ProviderClaude {
		// Claude's default home ignores CLAUDE_CONFIG_DIR by design: sharing
		// mirrors the default profile even from inside a session.
		return s.Paths.DefaultClaudeConfigHome()
	}
	return filepath.Join(s.Paths.Home, spec.Home.Default)
}

// execProvider hands the terminal over to the provider's own binary.
//
// On POSIX it does not return: see handOver.
func (a *App) execProvider(spec provider.Spec, args []string, env []string) error {
	argv := spec.Session.Argv
	binary, err := exec.LookPath(argv[0])
	if err != nil {
		return fmt.Errorf("%w: `%s` was not found on your PATH. Install %s first",
			apperr.ErrSession, argv[0], spec.Name)
	}
	// Anything the declaration puts after the binary comes before the user's
	// arguments, so a provider that needs a subcommand to start a session gets
	// one without the caller knowing.
	args = slices.Concat(argv[1:], args)
	if a.HandOver != nil {
		return a.HandOver(binary, args, env)
	}
	return handOver(binary, args, env, a.Out, a.Err, a.In)
}

// mapCommand remembers which account a directory belongs to.
func (a *App) mapCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "map ACCOUNT [DIRECTORY]",
		Short: "Remember which account a directory belongs to",
		Long: "`aaswap run` with no account resolves the working directory to its nearest\n" +
			"mapped ancestor, so a project always gets the same login.",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 2 {
				dir = args[1]
			}
			return a.runMap(args[0], dir)
		},
	}
	silenceUsage(cmd)
	return cmd
}

func (a *App) unmapCommand() *cobra.Command {
	var list bool
	cmd := &cobra.Command{
		Use:   "unmap [DIRECTORY]",
		Short: "Forget a directory's account",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if list {
				return a.runMappings()
			}
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			return a.runUnmap(dir)
		},
	}
	cmd.Flags().BoolVar(&list, "list", false, "show every mapping instead")
	silenceUsage(cmd)
	return cmd
}

func (a *App) mappingsCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "Show every directory mapping",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runMappings()
		},
	}
	silenceUsage(cmd)
	return cmd
}

func (a *App) runMap(identifier, dir string) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	if err := a.requireCapability(s, provider.CapSession); err != nil {
		return err
	}
	num, account, err := s.ResolveAccount(identifier)
	if err != nil {
		return err
	}

	store := mappings.New(s.BackupRoot())
	store.Now = s.Now
	key, err := store.Set(dir, mappings.Identity{
		Email:            account.Email,
		OrganizationUUID: account.OrganizationUUID,
	})
	if err != nil {
		return err
	}
	a.printer.Println(a.printer.Accent("Mapped"), " ", key, " → ",
		fmt.Sprintf("%s (%s)", num, account.Email))
	return nil
}

func (a *App) runUnmap(dir string) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	if err := a.requireCapability(s, provider.CapSession); err != nil {
		return err
	}
	store := mappings.New(s.BackupRoot())
	removed, err := store.Remove(dir)
	if err != nil {
		return err
	}
	if !removed {
		a.printer.Println(a.printer.Dimmed("That directory has no mapping."))
		return nil
	}
	a.printer.Println(a.printer.Accent("Unmapped"), " ", dir)
	return nil
}

func (a *App) runMappings() error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	if err := a.requireCapability(s, provider.CapSession); err != nil {
		return err
	}
	store := mappings.New(s.BackupRoot())
	table := store.Load()

	if a.json {
		type row struct {
			Directory        string `json:"directory"`
			Email            string `json:"email"`
			OrganizationUUID string `json:"organizationUuid"`
			Added            string `json:"added,omitzero"`
		}
		rows := make([]row, 0, len(table))
		for _, dir := range sortedMapKeys(table) {
			entry := table[dir]
			rows = append(rows, row{
				Directory: dir, Email: entry.Email,
				OrganizationUUID: entry.OrganizationUUID, Added: entry.Added,
			})
		}
		a.emitJSON(rows)
		return nil
	}

	if len(table) == 0 {
		a.printer.Println(a.printer.Dimmed(
			"No directories are mapped. Map one with: aaswap map <account> [directory]"))
		return nil
	}
	for _, dir := range sortedMapKeys(table) {
		a.printer.Println("  ", a.printer.Accent(dir), " → ", table[dir].Email)
	}
	return nil
}

func sortedMapKeys(table map[string]*mappings.Entry) []string {
	keys := make([]string, 0, len(table))
	for key := range table {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
