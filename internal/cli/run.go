package cli

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/d0lim/aaswap/internal/claudeapi"
	"github.com/d0lim/aaswap/internal/credstore"
	"github.com/d0lim/aaswap/internal/jsonout"
	"github.com/d0lim/aaswap/internal/mappings"
	"github.com/d0lim/aaswap/internal/paths"
	"github.com/d0lim/aaswap/internal/pollpolicy"
	"github.com/d0lim/aaswap/internal/procdetect"
	"github.com/d0lim/aaswap/internal/render"
	"github.com/d0lim/aaswap/internal/swap"
	"github.com/spf13/cobra"
)

// ageNoteThreshold is when a served measurement starts carrying its age.
//
// Below it the number is current enough that annotating it would be noise;
// above it, the user needs to know they are looking at something the store kept
// rather than something just measured.
const ageNoteThreshold = pollpolicy.ServeTTL

func (a *App) runList(cmd *cobra.Command, tokenStatus bool) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	snapshot, err := s.TakeSnapshot(cmd.Context(), swap.CollectRequest{})
	if err != nil {
		return err
	}

	if a.json {
		a.emitJSON(s.ListPayload(snapshot))
		return nil
	}

	if len(snapshot.Views) == 0 {
		a.printer.Println(a.printer.Dimmed("No accounts are managed yet. Log in with Claude Code, then run: aaswap add"))
		a.notePredecessorStore(s.Paths)
		return nil
	}

	now := s.Now()
	for _, view := range snapshot.Views {
		marker := "  "
		if view.IsActive {
			marker = a.printer.Accent("● ")
		}
		label := fmt.Sprintf("%s: %s", view.Name, view.Account.Email)
		if view.IsActive {
			label = a.printer.Bold(label)
		}
		trailing := a.printer.Muted(" [" + view.Account.DisplayTag() + "]")
		if view.Account.Disabled {
			trailing += a.printer.Dimmed("  (out of rotation)")
		}
		a.printer.Println(marker, label, trailing)

		lines := render.EntryLines(snapshot.Entries[view.Name], now, ageNoteThreshold)
		for i, line := range lines {
			branch := "├"
			if i == len(lines)-1 {
				branch = "└"
			}
			a.printer.Println("  ", a.printer.Dimmed(branch), " ", a.printer.Muted(line))
		}
		if tokenStatus {
			if status, ok := claudeapi.TokenStatus(view.Credentials, now); ok {
				a.printer.Println("    ", a.printer.Dimmed(status))
			}
		}
	}

	for _, warning := range swap.DuplicateAccountWarnings(snapshot.Views) {
		a.printer.Blank()
		a.printer.Warning(warning)
	}
	for _, warning := range swap.LockstepUsageWarnings(snapshot.Views, snapshot.Entries) {
		a.printer.Blank()
		a.printer.Warning(warning)
	}
	return nil
}

func (a *App) runStatus(cmd *cobra.Command) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	snapshot, err := s.TakeSnapshot(cmd.Context(), swap.CollectRequest{})
	if err != nil {
		return err
	}

	if a.json {
		a.emitJSON(s.StatusPayload(snapshot))
		return nil
	}

	live, ok := s.LiveIdentity()
	if !ok {
		a.printer.Println(a.printer.Bold("Status: "), a.printer.Dimmed("no active Claude account"))
		return nil
	}
	slot, managed := snapshot.Roster.FindName(live.Identity())
	if !managed {
		a.printer.Println(a.printer.Bold("Status: "), live.Email,
			a.printer.Muted(" ["+live.DisplayTag()+"]"))
		a.printer.Println(a.printer.Dimmed("  Not managed by aaswap. Run `aaswap add` to store it."))
		return nil
	}

	a.printer.Println(a.printer.Bold("Status: "), a.printer.Accent(slot),
		" ", live.Email, a.printer.Muted(" ["+live.DisplayTag()+"]"))
	for _, line := range render.EntryLines(snapshot.Entries[slot], s.Now(), ageNoteThreshold) {
		a.printer.Println("  ", a.printer.Muted(line))
	}
	a.printer.Println(a.printer.Dimmed(fmt.Sprintf("  %d account(s) managed",
		len(snapshot.Roster.Accounts))))
	a.reportRunningInstances(s)
	return nil
}

// reportRunningInstances lists what is attached to the DEFAULT profile right
// now — Claude Code sessions and editor connections.
//
// It belongs in status because switching replaces the credential those
// instances are authenticated with. Knowing something is attached is what turns
// "the switch did nothing" into "the running session is still on the old
// account until it restarts".
//
// Only the default profile: a `aaswap run` session has its own profile and its
// own credential, and a switch does not touch it.
func (a *App) reportRunningInstances(s *swap.Switcher) {
	configDir := s.Paths.DefaultClaudeConfigHome()
	sessions, _ := procdetect.Scan(configDir)
	ides := procdetect.IDEInstances(configDir)
	if len(sessions) == 0 && len(ides) == 0 {
		return
	}

	// Grouped by what the reader can act on — which program, in which folder.
	// Four windows on one repo is one line, not four.
	type place struct{ label, folder string }
	counts := map[place]int{}
	for _, session := range sessions {
		counts[place{entrypointLabel(session.Entrypoint), session.CWD}]++
	}
	for _, ide := range ides {
		for _, folder := range ide.WorkspaceFolders {
			counts[place{ide.IDEName, folder}]++
		}
	}

	a.printer.Blank()
	a.printer.Println(a.printer.Dimmed("Attached to this login right now:"))
	for _, key := range slices.SortedFunc(maps.Keys(counts), func(x, y place) int {
		return cmp.Or(cmp.Compare(x.label, y.label), cmp.Compare(x.folder, y.folder))
	}) {
		line := "  " + key.label + "  " + key.folder
		if n := counts[key]; n > 1 {
			line += fmt.Sprintf("  ×%d", n)
		}
		a.printer.Println(a.printer.Muted(line))
	}
	a.printer.Println(a.printer.Dimmed(
		"  A switch replaces the credential; these keep the old account until they restart."))
}

// entrypointLabel names how an instance was started, in the words a user would
// use. An unrecognized entrypoint is shown verbatim rather than dropped: a new
// one from a newer Claude Code is still worth reporting.
func entrypointLabel(entrypoint string) string {
	switch entrypoint {
	case "cli":
		return "claude"
	case "claude-vscode":
		return "VS Code"
	case "claude-desktop":
		return "Claude Desktop"
	case "sdk-cli":
		return "SDK"
	case "mcp":
		return "MCP"
	case "":
		return "unknown"
	}
	return entrypoint
}

func (a *App) runSwitch(cmd *cobra.Command, target, strategy string, force bool) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	roster, err := s.RosterOrEmpty()
	if err != nil {
		return err
	}
	if len(roster.Accounts) == 0 {
		return fmt.Errorf("no accounts are managed yet. Log in with Claude Code, then run: aaswap add")
	}

	if target == "" {
		target, err = a.chooseTarget(cmd, s, roster, strategy)
		if err != nil {
			return err
		}
		if target == "" {
			return nil // a strategy that chose to stay put has already said so
		}
	} else {
		if target, err = resolveTarget(s, roster, target); err != nil {
			return err
		}
	}

	outcome, err := s.Switch(cmd.Context(), swap.SwitchRequest{Target: target, Force: force})
	if err != nil {
		return err
	}

	if a.json {
		payload := jsonout.SwitchPayload{
			SchemaVersion: jsonout.SchemaVersion,
			Switched:      true,
			To:            accountRef(outcome.To),
			Warnings:      outcome.Warnings,
		}
		if outcome.From != nil {
			from := accountRef(*outcome.From)
			payload.From = &from
		}
		a.emitJSON(payload)
		return nil
	}

	verb := "Switched to"
	if outcome.Activated {
		verb = "Activated"
	}
	a.printer.Println(a.printer.Accent(verb), " ",
		fmt.Sprintf("%s (%s)", outcome.To.Name, outcome.To.Email))
	for _, warning := range outcome.Warnings {
		a.printer.Warning(warning)
	}
	return nil
}

// chooseTarget picks a rotation or strategy target.
func (a *App) chooseTarget(cmd *cobra.Command, s *swap.Switcher, roster *swap.Roster, strategy string) (string, error) {
	switchable := s.SwitchableNumbers(roster)
	if len(switchable) == 0 {
		return "", fmt.Errorf("no account has both a stored credential and a stored config; " +
			"re-add one with: aaswap add --slot N")
	}

	current, _ := s.CurrentNumber(roster)

	switch strategy {
	case "":
		// Plain rotation: the next switchable slot after the current one,
		// wrapping. With no current account, the first.
		return rotate(switchable, current), nil

	case "best":
		snapshot, err := s.TakeSnapshot(cmd.Context(), swap.CollectRequest{})
		if err != nil {
			return "", err
		}
		target, note := s.SelectBestSwitchable(snapshot, current)
		if target != "" {
			return target, nil
		}
		a.explainStay(note, current)
		return "", nil

	case "next-available":
		snapshot, err := s.TakeSnapshot(cmd.Context(), swap.CollectRequest{})
		if err != nil {
			return "", err
		}
		// Rotate, skipping accounts known to be at their limit. UNKNOWN is not
		// exhausted: an account whose usage could not be measured stays a
		// candidate, or a failing endpoint would strand the user.
		usageByAccount := swap.UsageByAccount(snapshot.Entries)
		start := rotationIndex(switchable, current)
		for i := range switchable {
			candidate := switchable[(start+i)%len(switchable)]
			if candidate == current {
				continue
			}
			result, measured := usageByAccount[candidate]
			if measured && result != nil {
				if headroom, known := result.Headroom(nil); known && headroom <= 0 {
					continue
				}
			}
			return candidate, nil
		}
		a.printer.Println(a.printer.Dimmed(
			"Every other account is at its rate limit; staying put."))
		return "", nil
	}
	return "", fmt.Errorf("unknown strategy %q: use 'best' or 'next-available'", strategy)
}

// explainStay says why a strategy declined to move.
func (a *App) explainStay(note swap.SelectionNote, current string) {
	messages := map[swap.SelectionNote]string{
		swap.NoteNone:                 "No other account is switchable.",
		swap.NoteCurrentUnavailable:   "The current account's usage is unknown, so no target can be shown to be better. Staying put.",
		swap.NoteNoComparison:         "No other account's usage could be measured, so no target can be shown to be better. Staying put.",
		swap.NoteIncompleteComparison: "The current account has the most headroom of those that could be measured, but not every account could be. Staying put.",
		swap.NoteStay:                 "The current account already has the most headroom. Staying put.",
		swap.NoteExhausted:            "Every account is at its rate limit; switching would not help. Staying put.",
	}
	message, known := messages[note]
	if !known {
		message = "Staying put."
	}
	_ = current
	a.printer.Println(a.printer.Dimmed(message))
}

// rotate returns the slot after current, wrapping.
func rotate(switchable []string, current string) string {
	if len(switchable) == 0 {
		return ""
	}
	index := rotationIndex(switchable, current)
	return switchable[index%len(switchable)]
}

// rotationIndex is where a rotation starts: just past the current account, or
// at the beginning when it is not in the list.
func rotationIndex(switchable []string, current string) int {
	for i, num := range switchable {
		if num == current {
			return (i + 1) % len(switchable)
		}
	}
	return 0
}

func accountRef(ref swap.AccountRef) jsonout.AccountRef {
	return jsonout.AccountRef{Name: ref.Name, Email: ref.Email}
}

func (a *App) runAdd(cmd *cobra.Command, name string, wait bool) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	// Ahead of Add, never inside it: the capture must run against whatever is
	// live at the moment it runs, and a wait that resolved into the same call
	// would hand Add an identity read seconds before its own.
	if wait || a.shouldWaitForLogin(s) {
		if err := a.awaitLogin(cmd.Context(), s); err != nil {
			return err
		}
	}
	outcome, err := s.Add(cmd.Context(), swap.AddRequest{
		Name: name, AssumeYes: a.assumeYes, Confirm: a.confirm,
	})
	if err != nil {
		return err
	}
	if outcome.Cancelled {
		a.printer.Println(a.printer.Dimmed("Cancelled"))
		return nil
	}

	if outcome.Unverified != "" {
		// Never silently: registering with the ownership question unanswered is
		// the state the failure this check exists for was in.
		a.printer.Println(a.printer.Accent("Notice: "),
			fmt.Sprintf("could not verify that the stored credential belongs to %s (%s). "+
				"Registering anyway; re-run where the check can complete to confirm.",
				outcome.Email, outcome.Unverified))
	}
	if outcome.RenamedFrom != "" {
		a.printer.Println(a.printer.Dimmed(
			fmt.Sprintf("Moved from slot %s → %s", outcome.RenamedFrom, outcome.Name)))
	}
	verb := "Added"
	if outcome.Refreshed {
		verb = "Updated credentials for"
	}
	a.printer.Println(a.printer.Accent(verb), " ",
		fmt.Sprintf("%s: %s", outcome.Name, outcome.Email),
		a.printer.Muted(" ["+outcome.Tag+"]"))
	return nil
}

func (a *App) runRemove(cmd *cobra.Command, identifier string) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	outcome, err := s.Remove(swap.RemoveRequest{
		Identifier: identifier, AssumeYes: a.assumeYes, Confirm: a.confirm,
		ChooseAmbiguous: a.chooseAmbiguous,
	})
	if err != nil {
		return err
	}
	if outcome.Cancelled {
		a.printer.Println(a.printer.Dimmed("Cancelled"))
		return nil
	}
	if outcome.WasActive {
		a.printer.Warning(fmt.Sprintf("%s (%s) was the active login. "+
			"You are still logged in; aaswap simply no longer has a copy.",
			outcome.Name, outcome.Email))
	}
	a.printer.Println(a.printer.Accent("Removed"), " ",
		fmt.Sprintf("%s (%s)", outcome.Name, outcome.Email))
	a.pruneMappingsFor(s, mappings.Identity{
		Email:            outcome.Email,
		OrganizationUUID: outcome.OrganizationUUID,
	})
	return nil
}

// pruneMappingsFor drops directory mappings that pointed at an account which no
// longer exists.
//
// Best effort, and reported rather than returned: the account is already gone
// by the time this runs, and failing the command afterwards would say the
// removal did not happen. A surviving mapping sends `aaswap run` in that
// directory looking for a slot that is not there, which is a confusing error
// but not a dangerous one.
func (a *App) pruneMappingsFor(s *swap.Switcher, identity mappings.Identity) {
	store := mappings.New(s.BackupRoot())
	store.Now = s.Now
	removed, err := store.PruneAccount(identity)
	if err != nil {
		a.printer.Warning("could not drop this account's directory mappings: " + err.Error())
		return
	}
	if removed > 0 {
		a.printer.Println(a.printer.Dimmed(
			fmt.Sprintf("  dropped %d directory mapping(s) that pointed at it", removed)))
	}
}

// chooseAmbiguous asks which of several slots sharing an address was meant.
func (a *App) chooseAmbiguous(matches []swap.AmbiguousMatch) (string, bool) {
	if a.In == nil {
		return "", false
	}
	a.printer.Println("Several accounts share that address:")
	for _, match := range matches {
		a.printer.Println("  ", match.Name, ": ", match.Email,
			a.printer.Muted(" ["+match.Tag+"]"))
	}
	a.printer.Printf("Enter the account number: ")
	var answer string
	if _, err := fmt.Fscanln(a.In, &answer); err != nil {
		a.printer.Blank()
		return "", false
	}
	answer = strings.TrimSpace(answer)
	for _, match := range matches {
		if match.Name == answer {
			return answer, true
		}
	}
	return "", false
}

func (a *App) runSetDisabled(cmd *cobra.Command, identifier string, disabled bool) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	num, email, changed, err := s.SetDisabled(identifier, disabled)
	if err != nil {
		return err
	}

	verb := "Enabled"
	if disabled {
		verb = "Disabled"
	}
	if !changed {
		a.printer.Println(a.printer.Dimmed(fmt.Sprintf(
			"%s (%s) is already %s.", num, email, strings.ToLower(verb))))
		return nil
	}
	a.printer.Println(a.printer.Accent(verb), " ", fmt.Sprintf("%s (%s)", num, email))

	if !disabled {
		a.printer.Println(a.printer.Dimmed("  It is back in the rotation."))
		return nil
	}

	roster, err := s.RosterOrEmpty()
	if err != nil {
		return err
	}
	if current, ok := s.CurrentNumber(roster); ok && current == num {
		a.printer.Println(a.printer.Dimmed(
			"  It is the active account — it stays live until you switch away; " +
				"it just will not be an automatic target."))
	}
	if len(s.SwitchableNumbers(roster)) == 0 {
		a.printer.Warning("No accounts remain in rotation — auto-switch and a bare " +
			"switch have nothing to pick. Re-enable one with: aaswap enable <num|email>")
	}
	return nil
}

func (a *App) runRename(cmd *cobra.Command, identifier, to string) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	from, name, err := s.Rename(identifier, to)
	if err != nil {
		return err
	}
	if from == name {
		a.printer.Println(a.printer.Dimmed(fmt.Sprintf("%s is already called that.", name)))
		return nil
	}
	a.printer.Println(a.printer.Accent("Renamed"), " ", fmt.Sprintf("%s → %s", from, name))
	return nil
}

func (a *App) runPurge(cmd *cobra.Command) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	outcome, err := s.Purge(a.confirm, a.assumeYes)
	if err != nil {
		return err
	}
	if outcome.Cancelled {
		a.printer.Println(a.printer.Dimmed("Cancelled"))
		return nil
	}
	if len(outcome.Removed) == 0 {
		a.printer.Println(a.printer.Dimmed("No accounts were managed."))
		return nil
	}
	a.printer.Println(a.printer.Accent("Removed"), " ",
		fmt.Sprintf("%d account(s). Your live login is untouched.", len(outcome.Removed)))
	return nil
}

func (a *App) runUnclaimed(cmd *cobra.Command, purge string) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	entries, verdict := s.Creds.ListUnclaimed()

	if purge != "" {
		return a.purgeUnclaimed(s, entries, purge)
	}

	if verdict != "ok" {
		a.printer.Warning(fmt.Sprintf(
			"The record of preserved credentials is %s; the ids below come from the "+
				"files on disk and may be incomplete.", verdict))
	}
	if len(entries) == 0 {
		a.printer.Println(a.printer.Dimmed("No preserved credentials."))
		return nil
	}
	for _, id := range sortedIDs(entries) {
		entry := entries[id]
		a.printer.Println(a.printer.Accent(id))
		if entry.Reason != "" {
			a.printer.Println("  ", a.printer.Muted("reason: "+entry.Reason))
		}
		if entry.ConfigSlot != "" {
			a.printer.Println("  ", a.printer.Muted("the config named slot "+entry.ConfigSlot))
		}
		if entry.CreatedAt != "" {
			a.printer.Println("  ", a.printer.Muted("preserved "+entry.CreatedAt))
		}
		if _, unreadable := s.Creds.ReadUnclaimed(id); unreadable {
			a.printer.Println("  ", a.printer.Yellow("its bytes could not be read"))
		}
	}
	a.printer.Blank()
	a.printer.Println(a.printer.Dimmed(
		"Drop one with: aaswap unclaimed --purge <id>, or all of them with --purge all"))
	return nil
}

func (a *App) purgeUnclaimed(s *swap.Switcher, entries map[string]*credstore.StashEntry, purge string) error {
	if purge == "all" {
		if !a.confirm(fmt.Sprintf("Permanently drop all %d preserved credentials?", len(entries))) {
			a.printer.Println(a.printer.Dimmed("Cancelled"))
			return nil
		}
		for _, id := range sortedIDs(entries) {
			if err := s.Creds.DeleteUnclaimed(id); err != nil {
				return err
			}
		}
		a.printer.Println(a.printer.Accent("Dropped"), " ",
			fmt.Sprintf("%d preserved credential(s)", len(entries)))
		return nil
	}
	if _, known := entries[purge]; !known {
		return fmt.Errorf("no preserved credential with id %q", purge)
	}
	if !a.confirm(fmt.Sprintf("Permanently drop preserved credential %s?", purge)) {
		a.printer.Println(a.printer.Dimmed("Cancelled"))
		return nil
	}
	if err := s.Creds.DeleteUnclaimed(purge); err != nil {
		return err
	}
	a.printer.Println(a.printer.Accent("Dropped"), " ", purge)
	return nil
}

// sortedIDs orders preserved-credential ids, which is chronological because an
// id begins with its timestamp.
func sortedIDs(entries map[string]*credstore.StashEntry) []string {
	return credstore.SortedStashIDs(entries)
}

// noteClaudeSwapStore points out a store left by the claude-swap project.
//
// Shown only when aaswap has no accounts of its own, which is the only moment
// the suggestion is actionable — adopt refuses to merge into a populated
// store, so nagging someone who already has accounts would offer a command that
// cannot run.
//
// A note, never an automatic import: this names another tool's live credential
// store, and moving one out from under its owner on a first run is not a
// decision a listing gets to make.
func (a *App) notePredecessorStore(resolver *paths.Resolver) {
	found, ok := resolver.FindPredecessor()
	if !ok {
		return
	}
	a.printer.Println(a.printer.Dimmed(fmt.Sprintf(
		"Found a %s store at %s — `aaswap adopt` moves it over.", found.Name, found.Root)))
}
