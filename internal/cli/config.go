package cli

import (
	"fmt"
	"strings"

	"github.com/realiti4/claude-swap/internal/settings"
	"github.com/spf13/cobra"
)

// configCommand is the settings surface.
//
// Every subcommand goes through the same spec registry the loader does, so a
// value the file would clamp is refused here with the reason instead of being
// accepted and quietly adjusted.
func (a *App) configCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show or change cswap's settings",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runConfigList()
		},
	}
	silenceUsage(cmd)

	list := &cobra.Command{
		Use:   "list",
		Short: "Show every effective setting (the default)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runConfigList()
		},
	}
	get := &cobra.Command{
		Use:   "get KEY",
		Short: "Print one setting's effective value",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runConfigGet(args[0])
		},
	}
	set := &cobra.Command{
		Use:   "set KEY VALUE",
		Short: "Validate and store one setting",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runConfigSet(args[0], args[1])
		},
	}
	unset := &cobra.Command{
		Use:   "unset KEY",
		Short: "Remove one setting, reverting to its default",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runConfigUnset(args[0])
		},
	}
	path := &cobra.Command{
		Use:   "path",
		Short: "Print where settings.json lives",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.runConfigPath()
		},
	}
	for _, sub := range []*cobra.Command{list, get, set, unset, path} {
		silenceUsage(sub)
		cmd.AddCommand(sub)
	}
	return cmd
}

func (a *App) runConfigList() error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	rows := settings.Effective(s.BackupRoot())

	if a.json {
		type row struct {
			Key       string `json:"key"`
			Value     any    `json:"value"`
			IsDefault bool   `json:"isDefault"`
			Help      string `json:"help"`
		}
		out := make([]row, len(rows))
		for i, r := range rows {
			out[i] = row{Key: r.Spec.Dotted(), Value: r.Value, IsDefault: !r.IsSet, Help: r.Spec.Help}
		}
		a.emitJSON(out)
		return nil
	}

	width := 0
	for _, r := range rows {
		width = max(width, len(r.Spec.Dotted()))
	}
	for _, r := range rows {
		line := fmt.Sprintf("%-*s  %s", width, r.Spec.Dotted(), settings.FormatValue(r.Value))
		if !r.IsSet {
			// The marker reflects the FILE, not value equality: a key set
			// explicitly to its default is still set, and saying otherwise
			// would make `config unset` look like a no-op when it is not.
			line += a.printer.Dimmed("  (default)")
		}
		a.printer.Println(line)
	}
	return nil
}

func (a *App) runConfigGet(key string) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	for _, row := range settings.Effective(s.BackupRoot()) {
		if row.Spec.Dotted() == key {
			a.printer.Println(settings.FormatValue(row.Value))
			return nil
		}
	}
	return unknownKey(key)
}

func (a *App) runConfigSet(key, value string) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	stored, err := settings.Set(s.BackupRoot(), key, value)
	if err != nil {
		return err
	}
	a.printer.Println(a.printer.Accent("Set"), " ", key, " = ", settings.FormatValue(stored))
	return nil
}

func (a *App) runConfigUnset(key string) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	removed, err := settings.Unset(s.BackupRoot(), key)
	if err != nil {
		return err
	}
	if !removed {
		a.printer.Println(a.printer.Dimmed(key + " was not set."))
		return nil
	}
	spec, err := settings.SpecFor(key)
	if err != nil {
		return err
	}
	a.printer.Println(a.printer.Accent("Unset"), " ", key, " → ",
		settings.FormatValue(spec.Default()), a.printer.Dimmed(" (default)"))
	return nil
}

func (a *App) runConfigPath() error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	a.printer.Println(settings.Path(s.BackupRoot()))
	return nil
}

// unknownKey names the key and lists what is available, because a typo in a
// dotted key is the common case and the list is short.
func unknownKey(key string) error {
	var known []string
	for _, spec := range settings.Specs() {
		known = append(known, spec.Dotted())
	}
	return fmt.Errorf("unknown setting %q; the settings are: %s", key, joinWords(known))
}

func joinWords(words []string) string {
	var out strings.Builder
	for i, word := range words {
		switch {
		case i == 0:
		case i == len(words)-1:
			out.WriteString(" and ")
		default:
			out.WriteString(", ")
		}
		out.WriteString(word)
	}
	return out.String()
}
