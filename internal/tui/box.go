package tui

// DrawBox draws a frame around all of b, with title set into the top border
// (┌ Title ───┐), and returns the interior as a clipped view for the caller to
// draw the panel contents into. A buffer under 2x2 has no interior, so
// nothing is drawn and an empty view comes back. A title too long for the top
// border is truncated with an ellipsis rather than overrunning the corners.
func DrawBox(b *Buffer, title string, set BorderSet, border, titleStyle Style, ascii bool) *Buffer {
	w, h := b.Width(), b.Height()
	if w < 2 || h < 2 {
		return b.Sub(Rect{})
	}
	b.Fill(Rect{X: 1, Y: 0, W: w - 2, H: 1}, set.H, border)
	b.Fill(Rect{X: 1, Y: h - 1, W: w - 2, H: 1}, set.H, border)
	b.Fill(Rect{X: 0, Y: 1, W: 1, H: h - 2}, set.V, border)
	b.Fill(Rect{X: w - 1, Y: 1, W: 1, H: h - 2}, set.V, border)
	b.Set(0, 0, Cell{Rune: set.TL, Style: border})
	b.Set(w-1, 0, Cell{Rune: set.TR, Style: border})
	b.Set(0, h-1, Cell{Rune: set.BL, Style: border})
	b.Set(w-1, h-1, Cell{Rune: set.BR, Style: border})
	// Room for the title is the top edge less both corners and a space either side.
	if room := w - 4; title != "" && room > 0 {
		t := Truncate(title, room, ascii)
		b.Put(" ", 1, 0, border)
		x := b.Put(t, 2, 0, titleStyle)
		b.Put(" ", x, 0, border)
	}
	return b.Sub(Rect{X: 1, Y: 1, W: w - 2, H: h - 2})
}
