package tui

import (
	"image/color"

	"charm.land/lipgloss/v2"

	"github.com/realiti4/claude-swap/internal/render"
)

// Palette is the TUI's colors for one terminal background.
//
// Lipgloss v2 dropped AdaptiveColor, so a palette is chosen once from the
// detected background rather than resolved per style. The values are the same
// ones internal/render uses for the non-interactive output: the two surfaces
// show the same account in the same color, and a change to one is a change to
// both.
type Palette struct {
	Accent    color.Color
	Muted     color.Color
	Text      color.Color
	Red       color.Color
	Yellow    color.Color
	Green     color.Color
	Border    color.Color
	Highlight color.Color
}

var palettes = map[render.Theme]Palette{
	render.Dark: {
		Accent:    lipgloss.Color("#c8794a"), // the warm terracotta render uses
		Muted:     lipgloss.Color("#9a9a9a"),
		Text:      lipgloss.Color("#e4e4e4"),
		Red:       lipgloss.Color("#d75f5f"),
		Yellow:    lipgloss.Color("#d7af5f"),
		Green:     lipgloss.Color("#87af87"),
		Border:    lipgloss.Color("#4e4e4e"),
		Highlight: lipgloss.Color("#3a3a3a"),
	},
	render.Light: {
		// Darker throughout: the dark palette's mid-tones vanish on white.
		Accent:    lipgloss.Color("#954c2a"),
		Muted:     lipgloss.Color("#635d55"),
		Text:      lipgloss.Color("#1c1c1c"),
		Red:       lipgloss.Color("#ad3128"),
		Yellow:    lipgloss.Color("#795911"),
		Green:     lipgloss.Color("#3f6b3f"),
		Border:    lipgloss.Color("#bcbcbc"),
		Highlight: lipgloss.Color("#e4e4e4"),
	},
}

// PaletteFor returns the palette for a theme, defaulting to dark for an
// unknown one — an unreadable dark-on-dark is the safer miss than
// invisible-on-light, since dark is what most terminals are.
func PaletteFor(theme render.Theme) Palette {
	if p, ok := palettes[theme]; ok {
		return p
	}
	return palettes[render.Dark]
}

// styles are the built lipgloss styles for one palette, made once per model
// rather than per frame: View runs on every keystroke and every tick.
type styles struct {
	title     lipgloss.Style
	frame     lipgloss.Style
	email     lipgloss.Style
	emailOn   lipgloss.Style
	slot      lipgloss.Style
	tag       lipgloss.Style
	muted     lipgloss.Style
	accent    lipgloss.Style
	red       lipgloss.Style
	yellow    lipgloss.Style
	green     lipgloss.Style
	selected  lipgloss.Style
	help      lipgloss.Style
	helpKey   lipgloss.Style
	modal     lipgloss.Style
	modalWarn lipgloss.Style
}

func newStyles(p Palette) styles {
	base := lipgloss.NewStyle()
	return styles{
		title:     base.Bold(true).Foreground(p.Accent),
		frame:     base.Border(lipgloss.RoundedBorder()).BorderForeground(p.Border).Padding(0, 1),
		email:     base.Foreground(p.Text),
		emailOn:   base.Bold(true).Foreground(p.Text),
		slot:      base.Foreground(p.Muted),
		tag:       base.Foreground(p.Muted),
		muted:     base.Foreground(p.Muted),
		accent:    base.Foreground(p.Accent),
		red:       base.Foreground(p.Red),
		yellow:    base.Foreground(p.Yellow),
		green:     base.Foreground(p.Green),
		selected:  base.Background(p.Highlight),
		help:      base.Foreground(p.Muted),
		helpKey:   base.Bold(true).Foreground(p.Accent),
		modal:     base.Border(lipgloss.RoundedBorder()).BorderForeground(p.Accent).Padding(1, 2),
		modalWarn: base.Border(lipgloss.RoundedBorder()).BorderForeground(p.Red).Padding(1, 2),
	}
}
