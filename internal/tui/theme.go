package tui

import (
	"os"
	"strings"
)

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
	// IconOK, IconWarn and IconBad are the symbols painted beside state, so
	// no theme is read by color alone: a row that says ok, warn or bad always
	// differs by its symbol too. Zero means the theme has no symbol and the
	// caller falls back to its own.
	IconOK, IconWarn, IconBad rune
	// SeriesBytesFG, SeriesPacketsFG, SeriesV6FG and SeriesH1FG are the
	// graph curve colours: green for byte traffic, pink for packets, blue for
	// IPv6 and H1+. Zero means the theme has no fixed palette and callers fall
	// back to their own colours.
	SeriesBytesFG, SeriesPacketsFG, SeriesV6FG, SeriesH1FG Color
}

// DefaultTheme uses the 16 base colors, which every color terminal maps to
// the user's own palette, rather than fixed RGB values that could clash with
// a light or unusual background.
func DefaultTheme() Theme {
	return Theme{
		Name:     "default",
		OK:       Style{FG: Ansi(2)},
		Warn:     Style{FG: Ansi(3)},
		Bad:      Style{FG: Ansi(1)}.With(AttrBold),
		Dim:      Style{FG: Ansi(8)},
		Accent:   Style{FG: Ansi(6)}.With(AttrBold),
		Graph:    Style{FG: Ansi(4)},
		Border:   Style{FG: Ansi(8)},
		Frame:    BorderRounded,
		IconOK:   '•',
		IconWarn: '!',
		IconBad:  '✗',
	}
}

// MonoTheme carries no color and no attributes, and draws with ASCII only.
// It is for NO_COLOR and TERM=dumb, where even bold may be dropped or echoed.
func MonoTheme() Theme {
	return Theme{Name: "mono", Frame: BorderASCII, ASCII: true, IconOK: 'i', IconWarn: '!', IconBad: 'x'}
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
		Name:     name,
		OK:       Style{FG: hex(ok)},
		Warn:     Style{FG: hex(warn)},
		Bad:      Style{FG: hex(bad)}.With(AttrBold),
		Dim:      Style{FG: hex(dim)},
		Accent:   Style{FG: hex(accent)}.With(AttrBold),
		Graph:    Style{FG: hex(graph)},
		Border:   Style{FG: hex(border)},
		Frame:    BorderRounded,
		IconOK:   '•',
		IconWarn: '!',
		IconBad:  '✗',
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
			Name:     "high-contrast",
			OK:       Style{FG: Ansi(10)},
			Warn:     Style{FG: Ansi(11)},
			Bad:      Style{FG: Ansi(9)}.With(AttrBold),
			Dim:      Style{FG: Ansi(7)},
			Accent:   Style{FG: Ansi(14)}.With(AttrBold),
			Graph:    Style{FG: Ansi(12)},
			Border:   Style{FG: Ansi(15)},
			Frame:    BorderSingle,
			IconOK:   '•',
			IconWarn: '!',
			IconBad:  '✗',
		}
	}},
	// colorblind uses the Okabe-Ito palette: the roles are blue, orange and
	// yellow, never red against green, so they stay apart for the common
	// forms of red-green color blindness. The state icons carry the meaning
	// too, so nothing hangs on the color alone.
	{"colorblind", func() Theme {
		return Theme{
			Name:     "colorblind",
			OK:       Style{FG: hex("#0072b2")},                // blue
			Warn:     Style{FG: hex("#e69f00")},                // orange
			Bad:      Style{FG: hex("#f0e442")}.With(AttrBold), // yellow
			Dim:      Style{FG: hex("#999999")},
			Accent:   Style{FG: hex("#56b4e9")}.With(AttrBold), // sky blue
			Graph:    Style{FG: hex("#0072b2")},
			Border:   Style{FG: hex("#999999")},
			Frame:    BorderRounded,
			IconOK:   '•',
			IconWarn: '!',
			IconBad:  '✗',
		}
	}},
	{"neon", func() Theme {
		return NeonThemeFromEnv(os.Getenv)
	}},
	{"mono", MonoTheme},
}

// Depth is the colour depth of the terminal.
type Depth int

const (
	// DepthTrue is 24-bit colour.
	DepthTrue Depth = iota
	// Depth256 is the xterm 256 colour palette.
	Depth256
	// Depth16 is the base ANSI palette.
	Depth16
)

// neonRole is one colour of the neon theme at every depth: the truecolor value
// the theme is designed in, the xterm 256 entry nearest to it (a cube or gray
// cell) and the base ANSI index nearest to it. The 256 entry is spelled out so
// a 256 colour terminal shows the intended hue rather than whatever automatic
// downsampling picks for it.
type neonRole struct {
	hex  string
	x256 uint8
	a16  uint8
}

// neonPalette is the URnetwork brand palette per role on the near black
// background the theme is designed for: green #87FB67, pale lime #EFF7BB,
// coral #FF6F7A, blue #007ACC and #0099FF, and two grays for dim text and
// borders. The green carries traffic, the coral packets, and the blues IPv6
// and H1+.
var neonPalette = struct {
	ok, warn, bad, dim, accent, graph, border neonRole
	bytes, packets, v6, h1                    neonRole
}{
	ok:      neonRole{"#87FB67", 119, 10},
	warn:    neonRole{"#EFF7BB", 229, 11},
	bad:     neonRole{"#FF6F7A", 204, 9},
	dim:     neonRole{"#8B949E", 246, 8},
	accent:  neonRole{"#0099FF", 33, 14},
	graph:   neonRole{"#87FB67", 119, 10},
	border:  neonRole{"#7D8A9E", 103, 8},
	bytes:   neonRole{"#87FB67", 119, 10},
	packets: neonRole{"#FF6F7A", 204, 13},
	v6:      neonRole{"#007ACC", 32, 6},
	h1:      neonRole{"#0099FF", 33, 14},
}

func (r neonRole) color(depth Depth) Color {
	switch depth {
	case Depth256:
		return Xterm(r.x256)
	case Depth16:
		return Ansi(r.a16)
	}
	return hex(r.hex)
}

// NeonTheme returns the URnetwork Neon theme at truecolor depth.
func NeonTheme() Theme { return NeonThemeDepth(DepthTrue) }

// NeonThemeDepth returns the neon theme in the best depth the terminal can
// show, so the accents stay the intended hue on a 256 or 16 colour terminal
// instead of being left to automatic downsampling.
func NeonThemeDepth(depth Depth) Theme {
	p := neonPalette
	return Theme{
		Name:            "neon",
		OK:              Style{FG: p.ok.color(depth)},
		Warn:            Style{FG: p.warn.color(depth)},
		Bad:             Style{FG: p.bad.color(depth)}.With(AttrBold),
		Dim:             Style{FG: p.dim.color(depth)},
		Accent:          Style{FG: p.accent.color(depth)}.With(AttrBold),
		Graph:           Style{FG: p.graph.color(depth)},
		Border:          Style{FG: p.border.color(depth)},
		Frame:           BorderRounded,
		IconOK:          '•',
		IconWarn:        '!',
		IconBad:         '✗',
		SeriesBytesFG:   p.bytes.color(depth),
		SeriesPacketsFG: p.packets.color(depth),
		SeriesV6FG:      p.v6.color(depth),
		SeriesH1FG:      p.h1.color(depth),
	}
}

// NeonThemeFromEnv returns the neon theme in the depth the terminal reports.
func NeonThemeFromEnv(getenv func(string) string) Theme {
	return NeonThemeDepth(DepthFromEnv(getenv))
}

// DepthFromEnv reads the depth a terminal advertises: COLORTERM truecolor or
// 24bit means truecolor, a TERM naming direct, truecolor, 24bit or 256 colours
// says the same, an empty TERM means mono, and any other TERM is assumed to
// have the 256 colour palette that the base terminals ship with.
func DepthFromEnv(getenv func(string) string) Depth {
	if v := strings.ToLower(getenv("COLORTERM")); v == "truecolor" || v == "24bit" {
		return DepthTrue
	}
	term := strings.ToLower(getenv("TERM"))
	switch {
	case strings.Contains(term, "direct"), strings.Contains(term, "truecolor"), strings.Contains(term, "24bit"):
		return DepthTrue
	case strings.Contains(term, "256"):
		return Depth256
	case term == "":
		return Depth16
	}
	return Depth256
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
