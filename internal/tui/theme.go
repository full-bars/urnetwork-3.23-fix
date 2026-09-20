package tui

// BorderSet is the set of runes a Box draws its frame with.
type BorderSet struct {
	H, V, TL, TR, BL, BR rune
}

var (
	// BorderSingle is the plain box-drawing frame.
	BorderSingle = BorderSet{H: '─', V: '│', TL: '┌', TR: '┐', BL: '└', BR: '┘'}
	// BorderRounded softens the corners.
	BorderRounded = BorderSet{H: '─', V: '│', TL: '╭', TR: '╮', BL: '╰', BR: '╯'}
	// BorderASCII is the fallback for terminals that cannot draw box glyphs.
	BorderASCII = BorderSet{H: '-', V: '|', TL: '+', TR: '+', BL: '+', BR: '+'}
)

// Theme maps the semantic roles the screen speaks in (this is fine, this is
// worth a look, this is broken) onto concrete styles, so widgets and screens
// never name a color. Swapping the Theme is the whole of NO_COLOR and dumb
// terminal support.
type Theme struct {
	Name   string
	OK     Style
	Warn   Style
	Bad    Style
	Dim    Style
	Accent Style
	Graph  Style
	Border Style
	// Frame is the border rune set for boxes.
	Frame BorderSet
	// ASCII selects the ASCII fallbacks in the widgets that have them
	// (sparklines, bars, graphs, table ellipsis), for terminals that cannot
	// draw block or braille glyphs.
	ASCII bool
}

// DefaultTheme uses the 16 base colors, which every color terminal maps to
// the user's own palette, rather than fixed RGB values that could clash with
// a light or unusual background.
func DefaultTheme() Theme {
	return Theme{
		Name:   "default",
		OK:     Style{FG: Ansi(2)},
		Warn:   Style{FG: Ansi(3)},
		Bad:    Style{FG: Ansi(1)}.With(AttrBold),
		Dim:    Style{FG: Ansi(8)},
		Accent: Style{FG: Ansi(6)}.With(AttrBold),
		Graph:  Style{FG: Ansi(4)},
		Border: Style{FG: Ansi(8)},
		Frame:  BorderRounded,
	}
}

// MonoTheme carries no color and no attributes, and draws with ASCII only.
// It is for NO_COLOR and TERM=dumb, where even bold may be dropped or echoed.
func MonoTheme() Theme {
	return Theme{Name: "mono", Frame: BorderASCII, ASCII: true}
}

// ThemeFromEnv picks the theme for the environment: mono when NO_COLOR is set
// to anything non-empty (per no-color.org) or TERM is dumb or unset, otherwise
// the default. getenv is injected so the choice can be tested.
func ThemeFromEnv(getenv func(string) string) Theme {
	if getenv("NO_COLOR") != "" {
		return MonoTheme()
	}
	if term := getenv("TERM"); term == "" || term == "dumb" {
		return MonoTheme()
	}
	return DefaultTheme()
}

// Level picks OK, Warn or Bad for a load fraction against two thresholds.
func (self Theme) Level(frac, warnAt, badAt float64) Style {
	switch {
	case frac >= badAt:
		return self.Bad
	case frac >= warnAt:
		return self.Warn
	}
	return self.OK
}
