package urnettools

import (
	"math"
	"sort"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
)

// drawProxyAurora paints waves across contiguous proxy index ranges.
func (m *topModel) drawProxyAurora(b *tui.Buffer) {
	w, h := b.Width(), b.Height()
	if w == 0 || h < 2 || len(m.proxyCells) == 0 {
		return
	}
	// The classic grid hit test does not describe these index ranges.
	m.proxyGridW, m.proxyGridH = 0, 0
	cells := append([]proxyCell(nil), m.proxyCells...)
	sort.Slice(cells, func(i, j int) bool { return cells[i].index < cells[j].index })
	groups := min(len(cells), max(1, w/4))
	type summary struct {
		state  uint8
		bucket uint8
	}
	sums := make([]summary, groups)
	now := m.now()
	for g := range sums {
		start, end := g*len(cells)/groups, (g+1)*len(cells)/groups
		dead, total := 0, 0
		for _, c := range cells[start:end] {
			if c.state == connect.ProxyCellDead {
				dead++
			}
			total += int(m.proxyGlowBucket(c, now))
			if proxyCellSeverity(c.state) > proxyCellSeverity(sums[g].state) {
				sums[g].state = c.state
			}
		}
		sums[g].bucket = uint8(total / (end - start))
		if dead*2 <= end-start && sums[g].state == connect.ProxyCellDead {
			sums[g].state = connect.ProxyCellUp
		}
	}
	phase := float64(now.UnixNano()) / 1e9 * 0.8
	for y := 0; y < h-1; y++ {
		for x := 0; x < w; x++ {
			if x == 0 && y == 0 {
				for _, c := range cells {
					if c.index == 0 {
						glyph, st := m.directCellStar(c, now)
						b.Set(0, 0, tui.Cell{Rune: glyph, Style: st})
						break
					}
				}
				if len(cells) > 0 && cells[0].index == 0 {
					continue
				}
			}
			s := sums[x*groups/w]
			wave := (math.Sin(float64(x)*0.17+float64(y)*0.42-phase) + 1) / 2
			level := min(3, int(wave*1.5+float64(s.bucket)/3))
			var glyph rune
			if m.theme.ASCII {
				glyph = []rune{'.', ':', '+', '#'}[level]
			} else if m.theme.Name == "mono" || m.theme.Name == "colorblind" {
				glyph = proxyCellASCII(s.state)
			} else {
				glyph = []rune{'░', '▒', '▓', '█'}[level]
			}
			b.Set(x, y, tui.Cell{Rune: glyph, Style: m.proxyCellStyle(s.state, uint8(level*2))})
		}
	}
	x := b.Put("Aurora", 0, h-1, m.theme.Dim)
	for _, c := range cells {
		if c.index == 0 {
			b.Put("    ", x, h-1, tui.Style{})
			m.drawLegendDirect(b, x+4, h-1, c)
			break
		}
	}
}
