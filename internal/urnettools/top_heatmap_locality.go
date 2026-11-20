package urnettools

import (
	"sort"

	"github.com/urnetwork/connect/internal/tui"
)

// heatmapHilbert maps an ordinal to a point in a square Hilbert curve.
func heatmapHilbert(side, ordinal int) (int, int) {
	x, y := 0, 0
	for s, t := 1, ordinal; s < side; s *= 2 {
		rx, ry := (t/2)&1, (t^(t/2))&1
		if ry == 0 {
			if rx == 1 {
				x, y = s-1-x, s-1-y
			}
			x, y = y, x
		}
		x += s * rx
		y += s * ry
		t /= 4
	}
	return x, y
}

// drawProxyLocality places adjacent proxy indices along a Hilbert curve.
func (m *topModel) drawProxyLocality(b *tui.Buffer) {
	w, h := b.Width(), b.Height()-1
	if w < 1 || h < 1 || len(m.proxyCells) == 0 {
		return
	}
	m.proxyGridW, m.proxyGridH = 0, 0
	cells := append([]proxyCell(nil), m.proxyCells...)
	sort.Slice(cells, func(i, j int) bool { return cells[i].index < cells[j].index })
	side := 1
	for side*side < len(cells) {
		side *= 2
	}
	now := m.now()
	for i, c := range cells {
		x, y := heatmapHilbert(side, i)
		x, y = x*w/side, y*h/side
		bucket := m.proxyGlowBucket(c, now)
		glyph := []rune{'░', '▒', '▓', '█'}[min(3, int(bucket)/2)]
		if m.theme.ASCII || m.theme.Name == "mono" || m.theme.Name == "colorblind" {
			glyph = proxyCellASCII(c.state)
		}
		b.Set(x, y, tui.Cell{Rune: glyph, Style: m.proxyCellStyle(c.state, bucket)})
	}
	b.Put("Locality", 0, h, m.theme.Dim)
}
