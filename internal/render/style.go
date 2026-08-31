// Package render turns cswap's data into what a person reads in a terminal.
//
// Deliberately small and dependency-free. The styling is one warm accent, dim
// secondary text, and bold for structure — restrained, so the numbers stand
// out rather than the decoration.
package render

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// ANSI codes that carry structure rather than color, so they are
// theme-independent.
const (
	reset = "\033[0m"
	bold  = "\033[1m"
	dim   = "\033[2m"
)

// Theme names the palette a terminal's background calls for.
type Theme string

const (
	Dark  Theme = "dark"
	Light Theme = "light"
)

type palette struct {
	accent, muted, red, yellow string
}

var palettes = map[Theme]palette{
	Dark: {
		accent: "\033[38;5;173m", // warm terracotta
		muted:  "\033[38;5;250m", // quieter than normal, still readable
		red:    "\033[31m",
		yellow: "\033[33m",
	},
	Light: {
		// Darker throughout: the dark palette's mid-tones vanish on white.
		accent: "\033[38;2;149;76;42m",
		muted:  "\033[38;2;99;93;85m",
		red:    "\033[38;2;173;49;40m",
		yellow: "\033[38;2;121;89;17m",
	},
}

// Printer writes styled output to one stream.
//
// A value rather than a package global, so a test can capture output without
// racing another test's settings, and so stdout and stderr can differ about
// whether they are a terminal.
type Printer struct {
	Out io.Writer
	// Color is whether escape codes are emitted at all.
	Color bool
	Theme Theme
}

// New returns a Printer for a stream, deciding color from the environment.
func New(out io.Writer) *Printer {
	return &Printer{Out: out, Color: colorEnabled(out), Theme: Dark}
}

// colorEnabled decides whether to emit escape codes.
//
// The conventions are honored in the order that lets a user override a guess:
// NO_COLOR silences color anywhere, FORCE_COLOR asks for it anywhere (which is
// how a test or a CI log gets styled output), and otherwise a stream that is
// not a terminal gets none — piped output is read by programs, and escape codes
// there are corruption.
func colorEnabled(out io.Writer) bool {
	if _, set := os.LookupEnv("NO_COLOR"); set {
		return false
	}
	if _, set := os.LookupEnv("FORCE_COLOR"); set {
		return true
	}
	if os.Getenv("TERM") == "dumb" {
		return false
	}
	file, ok := out.(*os.File)
	if !ok {
		return false
	}
	info, err := file.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}

func (p *Printer) wrap(code, text string) string {
	if !p.Color || text == "" {
		return text
	}
	return code + text + reset
}

// Accent marks the one thing a line is about.
func (p *Printer) Accent(text string) string { return p.wrap(palettes[p.Theme].accent, text) }

// Muted is secondary information that should not compete.
func (p *Printer) Muted(text string) string { return p.wrap(palettes[p.Theme].muted, text) }

// Dimmed is quieter still: present, but not asking to be read.
func (p *Printer) Dimmed(text string) string { return p.wrap(dim, text) }

// Bold marks structure — a heading, a label.
func (p *Printer) Bold(text string) string { return p.wrap(bold, text) }

// Red marks a failure.
func (p *Printer) Red(text string) string { return p.wrap(palettes[p.Theme].red, text) }

// Yellow marks something that needs attention but is not a failure.
func (p *Printer) Yellow(text string) string { return p.wrap(palettes[p.Theme].yellow, text) }

// Printf writes a formatted line.
func (p *Printer) Printf(format string, args ...any) {
	_, _ = fmt.Fprintf(p.Out, format, args...)
}

// Println writes a line.
func (p *Printer) Println(parts ...string) {
	_, _ = io.WriteString(p.Out, strings.Join(parts, "")+"\n")
}

// Blank writes an empty line.
func (p *Printer) Blank() { _, _ = io.WriteString(p.Out, "\n") }

// Warning writes a line marked as needing attention.
func (p *Printer) Warning(text string) {
	p.Println(p.Yellow("Warning: "), text)
}

// Error writes a line marked as a failure.
func (p *Printer) Error(text string) {
	p.Println(p.Red("Error: "), text)
}
