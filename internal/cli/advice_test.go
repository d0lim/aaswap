package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// Nearly every message in this codebase that names a command is advice given at
// the moment something has already gone wrong: a switch refused, a credential
// unreadable, a mapping outlived its account. Advice that names a command the
// binary does not have leaves the person exactly where they were, and worse —
// they now believe the tool is broken rather than that they need a different
// command.
//
// Commands get renamed. `add` became `login`, `export` moved under `account`.
// Nothing failed when they did, because the strings that name them are prose to
// the compiler. This test makes them not-prose.

// commandReference matches an invocation named in a string: the binary, then up
// to two words of command path.
//
// The leading group requires a delimiter before the name, so an export file
// called backup.aaswap does not read as an invocation of everything after it.
// Go's regexp has no lookbehind, hence a group rather than an assertion.
var commandReference = regexp.MustCompile(
	`(^|[^\w.-])aaswap ((?:--[a-z-]+ )*[a-z][a-z-]*(?: [a-z][a-z-]*)?)`)

// referenceGroup is the submatch holding the command path.
const referenceGroup = 2

// proseAfterTheName is every word that follows "aaswap" in a sentence rather
// than in an invocation — "aaswap cannot log you in", "aaswap manages several".
//
// An allowlist rather than a heuristic: the failure this test exists to catch
// is a command name going stale, and a heuristic that guesses "that looked like
// prose" is precisely how a stale name would slip through. A word landing here
// is a deliberate act, visible in the diff.
var proseAfterTheName = []string{
	"already", "and", "backup", "backups", "binary", "can", "cannot",
	"classifies", "comfortably", "could", "created", "data", "defer",
	"deliberately", "dir", "does", "error", "exists", "falls", "from",
	"genuinely", "gets", "hand", "has", "holds", "is", "itself", "keeps",
	"knows", "made", "manages", "manipulates", "may", "mirrors", "mislabel",
	"must", "needs", "never", "notices", "now", "on", "once", "or", "owns",
	"preserved", "puts", "raises", "reads", "rewrote", "run", "ships", "simply",
	"since", "stays", "supports", "surface", "talks", "tells", "test", "that",
	"the", "to", "under", "wants", "was", "would", "wrote",
}

func TestEveryCommandNamedInAMessageExists(t *testing.T) {
	root := rootForInspection(t)
	stale := false

	for _, found := range scanStringLiterals(t) {
		words := strings.Fields(found.reference)
		if slices.Contains(proseAfterTheName, words[0]) {
			continue
		}
		// Resolved by cobra itself rather than against a list built here. It is
		// the resolver a real invocation goes through, so it strips flags the
		// same way and follows aliases the same way — and a check that models
		// that separately would eventually disagree with it.
		if resolved, _, err := root.Find(words); err == nil && resolved != root {
			continue
		}
		t.Errorf("%s tells the user to run `aaswap %s`, which this binary has no "+
			"command for", found.where, strings.Join(words, " "))
		stale = true
	}
	if stale {
		t.Logf("the commands that do exist: %s", strings.Join(invocablePaths(t), ", "))
	}
}

// A group is not a command anyone can run, so advice must never stop at one.
// "Run `aaswap account`" prints a usage screen and does nothing.
func TestAdviceNeverStopsAtACommandGroup(t *testing.T) {
	groups := map[string]bool{}
	walk(rootForInspection(t), "", func(path string, cmd *cobra.Command) {
		if cmd.HasSubCommands() && !cmd.Runnable() {
			groups[path] = true
		}
	})

	root := rootForInspection(t)
	for _, found := range scanStringLiterals(t) {
		words := strings.Fields(found.reference)
		if slices.Contains(proseAfterTheName, words[0]) {
			continue
		}
		resolved, rest, err := root.Find(words)
		if err != nil || resolved == root || !groups[commandPath(resolved)] || len(rest) > 0 {
			continue
		}
		t.Errorf("%s names the group `aaswap %s`, which runs nothing. Name the "+
			"subcommand the user is meant to run", found.where, commandPath(resolved))
	}
}

// The README is where someone learns the commands in the first place, so a
// stale one there is worse than a stale one in an error: they have not even
// tried yet. `map` and `unmap` had moved under `dir`, and `disable` under
// `account`, and the README still taught the old spellings.
func TestEveryCommandNamedInTheDocsExists(t *testing.T) {
	root := rootForInspection(t)

	for _, found := range scanDocs(t) {
		words := strings.Fields(found.reference)
		if slices.Contains(proseAfterTheName, words[0]) {
			continue
		}
		if resolved, _, err := root.Find(words); err == nil && resolved != root {
			continue
		}
		t.Errorf("%s teaches `aaswap %s`, which this binary has no command for",
			found.where, strings.Join(words, " "))
	}
}

// --- the scan ---------------------------------------------------------------

type reference struct {
	reference string
	where     string
}

// scanStringLiterals finds every command named in a string the program can
// print. Comments are excluded deliberately: a comment naming an old command is
// a stale note, while a string naming one is advice a user acts on.
func scanStringLiterals(t *testing.T) []reference {
	t.Helper()
	var out []reference
	root := repoRoot(t)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		switch {
		case err != nil:
			return err
		case entry.IsDir():
			if entry.Name() == ".git" || entry.Name() == "testdata" {
				return fs.SkipDir
			}
			return nil
		case !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go"):
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			for _, match := range commandReference.FindAllStringSubmatch(literal.Value, -1) {
				out = append(out, reference{
					reference: match[referenceGroup],
					where:     relative + ":" + itoa(fset.Position(literal.Pos()).Line),
				})
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scanning the sources: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the scan found no command references at all, so it is proving nothing")
	}
	return out
}

// scanDocs finds every command named in the English documentation.
//
// Only the files that TEACH the commands. docs/ is a design record written
// against the code at a moment in time, and holding it to the current surface
// would turn every design note into a maintenance obligation.
func scanDocs(t *testing.T) []reference {
	t.Helper()
	var out []reference
	root := repoRoot(t)
	for _, name := range []string{"README.md", "AGENTS.md"} {
		data, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("reading %s: %v", name, err)
		}
		for number, line := range strings.Split(string(data), "\n") {
			for _, match := range commandReference.FindAllStringSubmatch(line, -1) {
				out = append(out, reference{
					reference: match[referenceGroup],
					where:     name + ":" + itoa(number+1),
				})
			}
		}
	}
	if len(out) == 0 {
		t.Fatal("the scan found no command references at all, so it is proving nothing")
	}
	return out
}

// invocablePaths is every command path a person can actually type.
func invocablePaths(t *testing.T) []string {
	t.Helper()
	var paths []string
	walk(rootForInspection(t), "", func(path string, cmd *cobra.Command) {
		paths = append(paths, path)
		for _, alias := range cmd.Aliases {
			parent := strings.TrimSuffix(path, cmd.Name())
			paths = append(paths, strings.TrimSpace(parent+alias))
		}
	})
	slices.Sort(paths)
	return paths
}

// walk visits every command BELOW the root. The root itself is skipped: it has
// no path anyone types, and giving it the empty one makes every child inherit it.
func walk(cmd *cobra.Command, prefix string, visit func(path string, cmd *cobra.Command)) {
	for _, child := range cmd.Commands() {
		path := strings.TrimSpace(prefix + " " + child.Name())
		visit(path, child)
		walk(child, path, visit)
	}
}

// commandPath is a resolved command's path without the binary name.
func commandPath(cmd *cobra.Command) string {
	return strings.TrimSpace(strings.TrimPrefix(cmd.CommandPath(), cmd.Root().Name()))
}

func rootForInspection(t *testing.T) *cobra.Command {
	t.Helper()
	return newHarness(t).app.rootCommand()
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test's own source file")
	}
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
