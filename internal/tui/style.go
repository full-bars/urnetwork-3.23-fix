package tui

// ColorKind says how a Color is expressed. The renderer stores the richest
// form the caller asked for and leaves downgrading (truecolor to 256 to 16 to
// none) to the terminal backend, which is the only place that knows what the
// terminal supports.
type ColorKind uint8

const (
	// ColorDefault is the terminal's own foreground or background.
	ColorDefault ColorKind = iota
	// Color16 is an index 0..15 into the terminal's base palette.
	Color16
	// Color256 is an index 0..255 into the xterm 256 palette.
	Color256
	// ColorRGB is 0xRRGGBB.
	ColorRGB
)

// Color is an abstract color value. The zero value is the terminal default.
type Color struct {
	Kind  ColorKind
	Value uint32
}

// Default returns the terminal default color.
func Default() Color { return Color{} }

// Ansi returns a base-palette color (0..7 normal, 8..15 bright).
func Ansi(index uint8) Color { return Color{Kind: Color16, Value: uint32(index & 15)} }

// Xterm returns an xterm 256-palette color.
func Xterm(index uint8) Color { return Color{Kind: Color256, Value: uint32(index)} }

// RGB returns a truecolor value.
func RGB(r, g, b uint8) Color {
	return Color{Kind: ColorRGB, Value: uint32(r)<<16 | uint32(g)<<8 | uint32(b)}
}

// Components splits a ColorRGB value into its channels.
func (self Color) Components() (r, g, b uint8) {
	return uint8(self.Value >> 16), uint8(self.Value >> 8), uint8(self.Value)
}

// Attr is a set of text attributes.
type Attr uint8

const (
	AttrBold Attr = 1 << iota
	AttrDim
	AttrItalic
	AttrUnderline
	AttrReverse
)

// Style is the look of one cell. It is comparable, which Diff relies on.
type Style struct {
	FG    Color
	BG    Color
	Attrs Attr
}

// Fg returns a copy of the style with the foreground replaced.
func (self Style) Fg(c Color) Style { self.FG = c; return self }

// Bg returns a copy of the style with the background replaced.
func (self Style) Bg(c Color) Style { self.BG = c; return self }

// With returns a copy of the style with attributes added.
func (self Style) With(a Attr) Style { self.Attrs |= a; return self }
