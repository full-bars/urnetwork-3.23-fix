package urnettools

import (
	"math"
	"sort"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
)

// drawProxySonar maps index order to angles and throughput to distance.
func (m *topModel) drawProxySonar(b *tui.Buffer) {
	w, h := b.Width(), b.Height()
	if w < 1 || h < 2 || len(m.proxyCells) == 0 {
		return
	}
	m.proxyGridW, m.proxyGridH = 0, 0
	cells := append([]proxyCell(nil), m.proxyCells...)
	sort.Slice(cells, func(i, j int) bool { return cells[i].index < cells[j].index })
	cx, cy := float64(w-1)/2, float64(h-2)/2
	rx, ry := float64(w-1)/2, float64(h-2)/2
	if rx > ry*2 {
		rx = ry * 2
	}
	if ry > rx/2 {
		ry = rx / 2
	}
	put := func(x, y int, r rune, s tui.Style) {
		if x >= 0 && x < w && y >= 0 && y < h-1 {
			b.Set(x, y, tui.Cell{Rune: r, Style: s})
		}
	}
	ring := '·'
	if m.theme.ASCII {
		ring = '.'
	}
	for y := 0; y < h-1; y++ {
		for x := 0; x < w; x++ {
			if rx == 0 || ry == 0 {
				continue
			}
			d := math.Hypot((float64(x)-cx)/rx, (float64(y)-cy)/ry)
			for k := 1; k <= 3; k++ {
				if math.Abs(d-float64(k)/3) < 1/math.Max(rx, ry) {
					put(x, y, ring, m.theme.Dim)
				}
			}
		}
	}
	now := m.now()
	phase := float64(now.UnixNano()) / 1e9 * 0.65
	for i, c := range cells {
		a := float64(i) * 2 * math.Pi / float64(len(cells))
		r := 0.2 + 0.8*float64(c.bucket)/7
		x := int(math.Round(cx + math.Cos(a)*rx*r))
		y := int(math.Round(cy + math.Sin(a)*ry*r))
		age := math.Mod(phase-a, 2*math.Pi)
		if age < 0 {
			age += 2 * math.Pi
		}
		glyph := '•'
		if c.state == connect.ProxyCellDead {
			glyph = '○'
		} else if age >= 1 {
			glyph = '·'
		}
		if m.theme.ASCII || m.theme.Name == "mono" || m.theme.Name == "colorblind" {
			glyph = proxyCellASCII(c.state)
		}
		put(x, y, glyph, m.proxyCellStyle(c.state, c.bucket))
	}
	for r := 0.0; r <= 1; r += 0.025 {
		x := int(math.Round(cx + math.Cos(phase)*rx*r))
		y := int(math.Round(cy + math.Sin(phase)*ry*r))
		put(x, y, ring, m.theme.Dim)
	}
	b.Put("Sonar", 0, h-1, m.theme.Dim)
}
