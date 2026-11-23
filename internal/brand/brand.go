// Package brand renders the terminal artwork used by urnet-tools.
package brand

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

type Depth int

const (
	Mono Depth = iota
	Color16
	Color256
	TrueColor
)

type Capabilities struct {
	Depth         Depth
	UTF8, NoColor bool
}
type Asset struct {
	Name  string              `json:"-"`
	Kind  string              `json:"kind"`
	Cols  int                 `json:"cols"`
	Rows  int                 `json:"rows"`
	Cells [][]json.RawMessage `json:"cells"`
	Lines []string            `json:"lines"`
}

var Assets = loadAssets()

func loadAssets() map[string]*Asset {
	var assets map[string]*Asset
	if err := json.Unmarshal([]byte(assetJSON), &assets); err != nil {
		panic(err)
	}
	for name, a := range assets {
		a.Name = name
	}
	return assets
}

// Choose picks artwork that fits the available terminal dimensions.
func Choose(width, height int) *Asset {
	for _, candidate := range []struct {
		min  int
		name string
	}{{120, "wordmark-100x7"}, {100, "wordmark-80x6"}, {60, "icon-flat-12x6"}, {40, "icon-flat-8x4"}} {
		a := Assets[candidate.name]
		if width >= candidate.min && height >= a.Rows {
			return a
		}
	}
	return nil
}

// Render returns artwork with no trailing newline. Padding at the end of a
// row carries no meaning, so it is dropped from the returned text.
func Render(a *Asset, caps Capabilities) string {
	if a == nil {
		return ""
	}
	if !caps.UTF8 {
		return "URnetwork"
	}
	if a.Kind != "halfblock" {
		return trimRowEnds(strings.Join(a.Lines, "\n"))
	}
	var out strings.Builder
	for y, row := range Grid(a, caps) {
		if y > 0 {
			out.WriteByte('\n')
		}
		for _, c := range row {
			if c.FG != nil {
				color(&out, c.FG, false, caps.Depth)
			}
			if c.BG != nil {
				color(&out, c.BG, true, caps.Depth)
			}
			out.WriteRune(c.Glyph)
			if c.FG != nil || c.BG != nil {
				out.WriteString("\x1b[0m")
			}
		}
	}
	return trimRowEnds(out.String())
}

// Cell is one cell of an artwork, for a caller that paints cells itself, such
// as a full screen view. FG and BG are red, green and blue, or nil for the
// terminal's own color.
type Cell struct {
	Glyph  rune
	FG, BG []int
}

// Grid returns the artwork as rows of cells, with the same rules Render uses:
// nothing for a terminal without UTF-8, and no color on a 16 color or mono
// terminal, where a half block with a background becomes a full block.
func Grid(a *Asset, caps Capabilities) [][]Cell {
	if a == nil || !caps.UTF8 {
		return nil
	}
	if a.Kind != "halfblock" {
		rows := make([][]Cell, len(a.Lines))
		for y, line := range a.Lines {
			for _, r := range line {
				rows[y] = append(rows[y], Cell{Glyph: r})
			}
		}
		return rows
	}
	rows := make([][]Cell, len(a.Cells))
	for y, row := range a.Cells {
		for _, raw := range row {
			var fields []json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil || len(fields) != 3 {
				continue
			}
			var glyph string
			var fg, bg []int
			_ = json.Unmarshal(fields[0], &glyph)
			_ = json.Unmarshal(fields[1], &fg)
			_ = json.Unmarshal(fields[2], &bg)
			r, _ := utf8.DecodeRuneInString(glyph)
			if len(fg) != 3 {
				fg = nil
			}
			if len(bg) != 3 {
				bg = nil
			}
			if caps.NoColor || caps.Depth <= Color16 {
				if bg != nil && r == '▀' {
					r = '█'
				}
				fg, bg = nil, nil
			}
			rows[y] = append(rows[y], Cell{Glyph: r, FG: fg, BG: bg})
		}
	}
	return rows
}

// trimRowEnds drops the spaces at the end of each row of a multi row block.
func trimRowEnds(text string) string {
	rows := strings.Split(text, "\n")
	for i, row := range rows {
		rows[i] = strings.TrimRight(row, " ")
	}
	return strings.Join(rows, "\n")
}
func color(out *strings.Builder, rgb []int, bg bool, depth Depth) {
	code := 38
	if bg {
		code = 48
	}
	if depth == TrueColor {
		fmt.Fprintf(out, "\x1b[%d;2;%d;%d;%dm", code, rgb[0], rgb[1], rgb[2])
		return
	}
	fmt.Fprintf(out, "\x1b[%d;5;%dm", code, nearest(rgb))
}
func nearest(rgb []int) int {
	palette := [16][3]int{{0, 0, 0}, {128, 0, 0}, {0, 128, 0}, {128, 128, 0}, {0, 0, 128}, {128, 0, 128}, {0, 128, 128}, {192, 192, 192}, {128, 128, 128}, {255, 0, 0}, {0, 255, 0}, {255, 255, 0}, {0, 0, 255}, {255, 0, 255}, {0, 255, 255}, {255, 255, 255}}
	best, distance := 0, int(^uint(0)>>1)
	for i := 0; i < 256; i++ {
		var c [3]int
		switch {
		case i < 16:
			c = palette[i]
		case i < 232:
			n := i - 16
			vals := [6]int{0, 95, 135, 175, 215, 255}
			c = [3]int{vals[n/36], vals[(n/6)%6], vals[n%6]}
		default:
			v := 8 + (i-232)*10
			c = [3]int{v, v, v}
		}
		d := 0
		for j := 0; j < 3; j++ {
			x := rgb[j] - c[j]
			d += x * x
		}
		if d < distance {
			best, distance = i, d
		}
	}
	return best
}

// Beside places text after the icon, optionally centered vertically.
func Beside(icon string, width int, text string, centered bool) string {
	left, right := strings.Split(icon, "\n"), strings.Split(text, "\n")
	offset := 0
	if centered && len(left) > len(right) {
		offset = (len(left) - len(right)) / 2
	}
	count := len(left)
	if offset+len(right) > count {
		count = offset + len(right)
	}
	lines := make([]string, count)
	for i := range lines {
		l := ""
		if i < len(left) {
			l = left[i]
		}
		j := i - offset
		if j >= 0 && j < len(right) {
			padding := width - visibleWidth(l)
			if padding < 0 {
				padding = 0
			}
			l += strings.Repeat(" ", padding+2) + right[j]
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

// visibleWidth counts cells in the generated artwork, ignoring SGR sequences.
func visibleWidth(s string) int {
	width := 0
	for len(s) > 0 {
		if strings.HasPrefix(s, "\x1b[") {
			if end := strings.IndexByte(s, 'm'); end >= 0 {
				s = s[end+1:]
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(s)
		s = s[size:]
		width++
	}
	return width
}
