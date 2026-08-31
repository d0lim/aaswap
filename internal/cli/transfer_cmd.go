package cli

import (
	"encoding/json/jsontext"
	json "encoding/json/v2"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/realiti4/claude-swap/internal/apperr"
	"github.com/realiti4/claude-swap/internal/buildinfo"
	"github.com/realiti4/claude-swap/internal/fsutil"
	"github.com/realiti4/claude-swap/internal/transfer"
	"github.com/spf13/cobra"
)

func (a *App) exportCommand() *cobra.Command {
	var account string
	var full bool
	cmd := &cobra.Command{
		Use:   "export PATH",
		Short: "Write accounts to a portable file",
		Long: "Use \"-\" to write to stdout, which is how you add your own encryption:\n" +
			"  ccswap export - | gpg -c > accounts.gpg\n\n" +
			"The file carries live refresh tokens in the clear. ccswap does not encrypt\n" +
			"it, because your own tools do that better than a scheme invented here.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runExport(args[0], account, full)
		},
	}
	cmd.Flags().StringVar(&account, "account", "", "export only this account")
	cmd.Flags().BoolVar(&full, "full", false,
		"keep the whole config and credential per account (for backing up a machine to itself)")
	silenceUsage(cmd)
	return cmd
}

func (a *App) importCommand() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "import PATH",
		Short: "Restore accounts from a portable file",
		Long: "Use \"-\" to read from stdin:\n" +
			"  gpg -d accounts.gpg | ccswap import -\n\n" +
			"An account that already exists here is left alone unless --force says\n" +
			"otherwise — except one whose stored credential is quarantined as dead,\n" +
			"which a plain import replaces.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runImport(args[0], force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite accounts that already exist here")
	silenceUsage(cmd)
	return cmd
}

func (a *App) runExport(destination, account string, full bool) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}
	result, err := transfer.Export(s, transfer.ExportRequest{Account: account, Full: full},
		buildinfo.Version())
	if err != nil {
		return err
	}

	data, err := json.Marshal(result.Envelope, jsontext.WithIndent("  "))
	if err != nil {
		return fmt.Errorf("%w: encoding the export: %w", apperr.ErrTransfer, err)
	}
	data = append(data, '\n')

	// Skipped slots and the summary go to STDERR, so piping the export
	// somewhere yields nothing but the envelope.
	for _, skipped := range result.Skipped {
		a.errs.Println(a.errs.Dimmed("Skipping " + skipped))
	}

	if destination == "-" {
		_, err := a.Out.Write(data)
		return err
	}

	path, err := expandHome(destination)
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(path); statErr == nil && info.IsDir() {
		return fmt.Errorf("%w: the export destination must be a file, not a directory: %s",
			apperr.ErrTransfer, path)
	}
	// Owner-only from creation, never briefly world-readable: the file carries
	// live refresh tokens.
	if err := fsutil.WriteFileAtomic(path, data); err != nil {
		return fmt.Errorf("%w: writing the export: %w", apperr.ErrTransfer, err)
	}
	a.errs.Println(a.errs.Accent("Exported"), " ",
		fmt.Sprintf("%d account(s) to %s", len(result.Envelope.Accounts), path))
	return nil
}

func (a *App) runImport(source string, force bool) error {
	s, err := a.switcher()
	if err != nil {
		return err
	}

	var data []byte
	if source == "-" {
		data, err = io.ReadAll(a.In)
		if err != nil {
			return fmt.Errorf("%w: reading the export from stdin: %w", apperr.ErrTransfer, err)
		}
	} else {
		path, expandErr := expandHome(source)
		if expandErr != nil {
			return expandErr
		}
		data, err = os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("%w: import file not found: %s", apperr.ErrTransfer, path)
			}
			return fmt.Errorf("%w: reading the export: %w", apperr.ErrTransfer, err)
		}
	}

	result, err := transfer.Import(s, data, force)
	if err != nil {
		return err
	}

	if a.json {
		type row struct {
			Number  string   `json:"number"`
			Email   string   `json:"email"`
			Outcome string   `json:"outcome"`
			Notes   []string `json:"notes,omitzero"`
		}
		rows := make([]row, len(result.Accounts))
		for i, account := range result.Accounts {
			rows[i] = row{
				Number: account.Number, Email: account.Email,
				Outcome: string(account.Outcome), Notes: account.Notes,
			}
		}
		a.emitJSON(map[string]any{"accounts": rows, "activeSlot": result.ActiveSlot})
		return nil
	}

	verbs := map[transfer.ImportOutcome]string{
		transfer.Imported:  "Imported",
		transfer.Overwrote: "Overwrote",
		transfer.Replaced:  "Replaced",
		transfer.Skipped:   "Skipped",
	}
	for _, account := range result.Accounts {
		line := fmt.Sprintf("%s (slot %s)", account.Email, account.Number)
		if account.Outcome == transfer.Skipped {
			a.printer.Println(a.printer.Dimmed(verbs[account.Outcome] + " " + line))
		} else {
			a.printer.Println(a.printer.Accent(verbs[account.Outcome]), " ", line)
		}
		for _, note := range account.Notes {
			a.printer.Println("  ", a.printer.Dimmed("└ "+note))
		}
	}
	return nil
}

// expandHome resolves a leading ~ so a path typed in a shell that did not
// expand it still lands where the user meant.
func expandHome(path string) (string, error) {
	if path == "~" || len(path) > 1 && path[0] == '~' && (path[1] == '/' || path[1] == filepath.Separator) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("%w: expanding ~ in %q: %w", apperr.ErrConfig, path, err)
		}
		return filepath.Join(home, path[1:]), nil
	}
	return path, nil
}
