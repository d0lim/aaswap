package cli

import (
	"context"
	"io"
	"log/slog"
	"strings"

	"github.com/d0lim/ccswap/internal/render"
)

// cliHandler writes log records the way a command-line tool should.
//
// Not the default text handler: its `time level key=value` shape is right for a
// log file and wrong for a terminal, where it competes with the output the user
// actually asked for. Here a warning is one plain line on stderr, and
// everything below warning is silent unless --debug asks for it.
type cliHandler struct {
	printer *render.Printer
	level   slog.Level
	attrs   []slog.Attr
}

func (h *cliHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *cliHandler) Handle(_ context.Context, record slog.Record) error {
	var b strings.Builder
	b.WriteString(record.Message)

	appendAttr := func(attr slog.Attr) bool {
		b.WriteString(" ")
		b.WriteString(attr.Key)
		b.WriteString("=")
		b.WriteString(attr.Value.String())
		return true
	}
	for _, attr := range h.attrs {
		appendAttr(attr)
	}
	record.Attrs(appendAttr)

	text := b.String()
	switch {
	case record.Level >= slog.LevelError:
		h.printer.Println(h.printer.Red("Error: "), text)
	case record.Level >= slog.LevelWarn:
		h.printer.Println(h.printer.Yellow("Warning: "), text)
	default:
		h.printer.Println(h.printer.Dimmed(text))
	}
	return nil
}

func (h *cliHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &cliHandler{printer: h.printer, level: h.level, attrs: append(clone(h.attrs), attrs...)}
}

// WithGroup is a no-op: this handler renders a flat line, and grouping would
// only add punctuation to something a person reads once and acts on.
func (h *cliHandler) WithGroup(string) slog.Handler { return h }

// clone copies the attributes, so a derived handler cannot append into the
// parent's backing array.
func clone(attrs []slog.Attr) []slog.Attr {
	return append([]slog.Attr(nil), attrs...)
}

// configureLogging points the default logger at this invocation's error stream.
//
// Warnings matter — a failed persist, a stashed credential — and they belong on
// stderr where they will not corrupt a piped payload. Everything quieter is
// discarded unless --debug asks for it, because a tool that narrates itself is
// harder to use than one that does not.
func (a *App) configureLogging(debug, jsonMode bool) {
	if jsonMode && !debug {
		// A machine is reading stdout, and a human may not be watching stderr
		// at all. The payload already carries what went wrong.
		slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
		return
	}
	level := slog.LevelWarn
	if debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(&cliHandler{printer: a.errs, level: level}))
}
