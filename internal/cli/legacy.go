package cli

import "strings"

// legacyFlags maps the flag spelling a command used to have to its verb.
//
// Every one of these is still in someone's shell history and someone's script,
// so they keep working — hidden from help, because the verbs are the one
// documented interface, but never removed.
var legacyFlags = map[string]string{
	"--list":            "list",
	"--status":          "status",
	"--add-account":     "add",
	"--add-token":       "add-token",
	"--remove-account":  "remove",
	"--disable-account": "disable",
	"--enable-account":  "enable",
	"--swap-accounts":   "swap",
	"--move-account":    "move",
	"--export":          "export",
	"--import":          "import",
	"--purge":           "purge",
	"--upgrade":         "upgrade",
	"--unclaimed":       "unclaimed",
	"--alias":           "alias",
	"--switch-to":       "switch",
	"--auto":            "auto",
	"--run":             "run",
	"--map":             "map",
	"--unmap":           "unmap",
	"--mappings":        "mappings",
}

// translateLegacyFlags rewrites a leading legacy flag into its verb.
//
// Only the FIRST token is considered, and only when it is one of the flags
// above. Everything after it passes through verbatim, so `--list --json` and
// `list --json` reach the same command with the same flags.
//
// A bare `--switch` is the rotation, which has no argument; `--switch-to X`
// jumps to one account. The verb collapses both: `switch` rotates and `switch
// X` jumps, so the translation of `--switch-to` drops the flag and leaves its
// argument as the verb's.
func translateLegacyFlags(args []string) []string {
	if len(args) == 0 {
		return args
	}
	head, rest := args[0], args[1:]

	if head == "--switch" {
		return append([]string{"switch"}, rest...)
	}
	// `--flag=value` is the same flag with its argument attached.
	if flag, value, found := strings.Cut(head, "="); found {
		if verb, known := legacyFlags[flag]; known {
			return append([]string{verb, value}, rest...)
		}
	}
	if verb, known := legacyFlags[head]; known {
		return append([]string{verb}, rest...)
	}
	return args
}
