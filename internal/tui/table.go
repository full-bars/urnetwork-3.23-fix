package tui

// Align is the horizontal placement of text inside a table column.
type Align uint8

const (
	AlignLeft Align = iota
	AlignRight
	AlignCenter
)

// Column describes one table column. A Width of zero makes the column flexible:
// flexible columns share whatever the fixed ones leave, equally.
type Column struct {
	Title string
	Width int
	Align Align
}

// Row is one table row. Missing cells read as empty; extra cells are ignored.
type Row struct {
	Cells []string
	Style Style
}

// Table is columns of text, one row per line, with columns one space apart.
type Table struct {
	Columns     []Column
	Rows        []Row
	Header      bool
	HeaderStyle Style
}

// DrawTable draws t into b from the top and returns how many data rows fit and
// were drawn (the header is not counted), so a caller can show "+N more".
// Text wider than its column is cut with an ellipsis on a rune boundary, and a
// wide rune is never split. When the columns do not fit, fixed columns are
// served left to right and the flexible ones shrink first.
func DrawTable(b *Buffer, t Table, ascii bool) int {
	w, h := b.Width(), b.Height()
	if w <= 0 || h <= 0 || len(t.Columns) == 0 {
		return 0
	}
	tracks := make([]Track, len(t.Columns))
	for i, c := range t.Columns {
		tracks[i] = Fixed(c.Width)
		if c.Width <= 0 {
			tracks[i] = Flex(1)
		}
	}
	widths := split(w-(len(t.Columns)-1), tracks)

	drawRow := func(y int, cells func(i int) string, st Style) {
		b.Fill(Rect{X: 0, Y: y, W: w, H: 1}, ' ', st)
		x := 0
		for i, c := range t.Columns {
			if widths[i] > 0 {
				text := Truncate(cells(i), widths[i], ascii)
				pad := widths[i] - StringWidth(text)
				switch c.Align {
				case AlignRight:
					b.Put(text, x+pad, y, st)
				case AlignCenter:
					b.Put(text, x+pad/2, y, st)
				default:
					b.Put(text, x, y, st)
				}
			}
			x += widths[i] + 1
		}
	}

	y := 0
	if t.Header {
		drawRow(y, func(i int) string { return t.Columns[i].Title }, t.HeaderStyle)
		y++
	}
	drawn := 0
	for _, r := range t.Rows {
		if y >= h {
			break
		}
		drawRow(y, func(i int) string {
			if i < len(r.Cells) {
				return r.Cells[i]
			}
			return ""
		}, r.Style)
		y++
		drawn++
	}
	return drawn
}
