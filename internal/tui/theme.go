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

// hex parses "#rrggbb" into a truecolor. It is only fed the literals below, so
// a malformed one is a programming error and panics at start-up, in the test.
func hex(s string) Color {
	if len(s) != 7 || s[0] != '#' {
		panic("tui: bad color literal " + s)
	}
	var v uint32
	for _, c := range s[1:] {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= uint32(c - '0')
		case c >= 'a' && c <= 'f':
			v |= uint32(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v |= uint32(c-'A') + 10
		default:
			panic("tui: bad color literal " + s)
		}
	}
	return Color{Kind: ColorRGB, Value: v}
}

// paletteTheme builds a truecolor theme from one color per role. The terminal
// backend downgrades to 256 or 16 colors where truecolor is not available.
func paletteTheme(name, ok, warn, bad, dim, accent, graph, border string) Theme {
	return Theme{
		Name:   name,
		OK:     Style{FG: hex(ok)},
		Warn:   Style{FG: hex(warn)},
		Bad:    Style{FG: hex(bad)}.With(AttrBold),
		Dim:    Style{FG: hex(dim)},
		Accent: Style{FG: hex(accent)}.With(AttrBold),
		Graph:  Style{FG: hex(graph)},
		Border: Style{FG: hex(border)},
		Frame:  BorderRounded,
	}
}

// themeBuilders are the built-in themes in menu order. default follows the
// terminal's own palette; mono carries no color at all.
var themeBuilders = []struct {
	name  string
	build func() Theme
}{
	{"default", DefaultTheme},
	{"nord", func() Theme {
		return paletteTheme("nord", "#a3be8c", "#ebcb8b", "#bf616a", "#616e88", "#88c0d0", "#81a1c1", "#4c566a")
	}},
	{"gruvbox", func() Theme {
		return paletteTheme("gruvbox", "#b8bb26", "#fabd2f", "#fb4934", "#928374", "#8ec07c", "#83a598", "#665c54")
	}},
	{"dracula", func() Theme {
		return paletteTheme("dracula", "#50fa7b", "#f1fa8c", "#ff5555", "#6272a4", "#bd93f9", "#8be9fd", "#44475a")
	}},
	{"solarized-dark", func() Theme {
		return paletteTheme("solarized-dark", "#859900", "#b58900", "#dc322f", "#586e75", "#2aa198", "#268bd2", "#586e75")
	}},
	{"tokyo-night", func() Theme {
		return paletteTheme("tokyo-night", "#9ece6a", "#e0af68", "#f7768e", "#565f89", "#7dcfff", "#7aa2f7", "#414868")
	}},
	{"high-contrast", func() Theme {
		return Theme{
			Name:   "high-contrast",
			OK:     Style{FG: Ansi(10)},
			Warn:   Style{FG: Ansi(11)},
			Bad:    Style{FG: Ansi(9)}.With(AttrBold),
			Dim:    Style{FG: Ansi(7)},
			Accent: Style{FG: Ansi(14)}.With(AttrBold),
			Graph:  Style{FG: Ansi(12)},
			Border: Style{FG: Ansi(15)},
			Frame:  BorderSingle,
		}
	}},
	{"mono", MonoTheme},
}

// ThemeNames lists the built-in themes in the order the menu cycles them.
func ThemeNames() []string {
	out := make([]string, len(themeBuilders))
	for i, t := range themeBuilders {
		out[i] = t.name
	}
	return out
}

// ThemeByName returns the built-in theme called name; ok is false for an
// unknown name.
func ThemeByName(name string) (Theme, bool) {
	for _, t := range themeBuilders {
		if t.name == name {
			return t.build(), true
		}
	}
	return Theme{}, false
}
