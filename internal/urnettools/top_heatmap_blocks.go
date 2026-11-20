package urnettools

import (
	"fmt"
	"math"
	"sort"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
)

// drawProxyBlocks lays contiguous index ranges out as health rectangles.
func (m *topModel) drawProxyBlocks(b *tui.Buffer) {
	w, h := b.Width(), b.Height()-1
	if w < 1 || h < 1 || len(m.proxyCells) == 0 {
		return
	}
	m.proxyGridW, m.proxyGridH = 0, 0
	cells := append([]proxyCell(nil), m.proxyCells...)
	sort.Slice(cells, func(i, j int) bool { return cells[i].index < cells[j].index })
	cols := min(len(cells), max(1, int(math.Sqrt(float64(len(cells))*float64(w)/float64(h*2)))))
	cols = min(cols, max(1, w/8))
	rows := min((len(cells)+cols-1)/cols, max(1, h/3))
	groups := min(len(cells), cols*rows)
	now := m.now()
	for g := 0; g < groups; g++ {
		x0, x1 := (g%cols)*w/cols, (g%cols+1)*w/cols
		y0, y1 := (g/cols)*h/rows, (g/cols+1)*h/rows
		start, end := g*len(cells)/groups, (g+1)*len(cells)/groups
		up := 0
		for _, c := range cells[start:end] {
			if c.state == connect.ProxyCellUp {
				up++
			}
		}
		label := fmt.Sprintf("%d-%d %d%%", cells[start].index, cells[end-1].index, up*100/(end-start))
		for x := x0; x < x1; x++ {
			b.Set(x, y0, tui.Cell{Rune: '-', Style: m.theme.Dim})
		}
		for i, r := range label {
			if x0+i >= x1 {
				break
			}
			b.Set(x0+i, y0, tui.Cell{Rune: r, Style: m.theme.Dim})
		}
		area := (x1 - x0) * (y1 - y0 - 1)
		for p := 0; p < area; p++ {
			c := cells[start+p*(end-start)/area]
			bucket := m.proxyGlowBucket(c, now)
			glyph := []rune{'░', '▒', '▓', '█'}[min(3, int(bucket)/2)]
			if m.theme.ASCII {
				glyph = []rune{'.', ':', '+', '#'}[min(3, int(bucket)/2)]
			}
			if m.theme.Name == "mono" || m.theme.Name == "colorblind" {
				glyph = proxyCellASCII(c.state)
			}
			b.Set(x0+p%(x1-x0), y0+1+p/(x1-x0), tui.Cell{Rune: glyph, Style: m.proxyCellStyle(c.state, bucket)})
		}
	}
	b.Put("Blocks", 0, h, m.theme.Dim)
}
