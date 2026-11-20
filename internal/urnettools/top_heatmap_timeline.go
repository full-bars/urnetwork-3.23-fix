package urnettools

import (
	"fmt"
	"sort"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

type proxyTimelineSample struct {
	minute int64
	cells  []proxyCell
}

// drawProxyTimeline records per-index-range activity once per minute while visible.
func (m *topModel) drawProxyTimeline(b *tui.Buffer) {
	w, h := b.Width(), b.Height()-1
	if w < 1 || h < 1 || len(m.proxyCells) == 0 {
		return
	}
	m.proxyGridW, m.proxyGridH = 0, 0
	groups := min(len(m.proxyCells), h)
	if groups != m.proxyTimelineGroups {
		m.proxyTimeline, m.proxyTimelineGroups = nil, groups
	}
	cells := append([]proxyCell(nil), m.proxyCells...)
	sort.Slice(cells, func(i, j int) bool { return cells[i].index < cells[j].index })
	minute := m.now().Unix() / int64(time.Minute/time.Second)
	if len(m.proxyTimeline) == 0 || m.proxyTimeline[len(m.proxyTimeline)-1].minute != minute {
		s := proxyTimelineSample{minute: minute, cells: make([]proxyCell, groups)}
		for g := 0; g < groups; g++ {
			start, end := g*len(cells)/groups, (g+1)*len(cells)/groups
			total := 0
			for _, c := range cells[start:end] {
				total += int(c.bucket)
				if proxyCellSeverity(c.state) > proxyCellSeverity(s.cells[g].state) {
					s.cells[g].state = c.state
				}
			}
			s.cells[g].bucket = uint8(total / (end - start))
		}
		m.proxyTimeline = append(m.proxyTimeline, s)
	}
	labelW := min(w/2, 12)
	capacity := max(1, w-labelW)
	if len(m.proxyTimeline) > capacity {
		m.proxyTimeline = append([]proxyTimelineSample(nil), m.proxyTimeline[len(m.proxyTimeline)-capacity:]...)
	}
	for g := 0; g < groups; g++ {
		start, end := g*len(cells)/groups, (g+1)*len(cells)/groups
		label := fmt.Sprintf("%d-%d", cells[start].index, cells[end-1].index)
		for x, r := range label {
			if x >= labelW {
				break
			}
			b.Set(x, g, tui.Cell{Rune: r, Style: m.theme.Dim})
		}
		for x, s := range m.proxyTimeline {
			c := s.cells[g]
			glyph := []rune{'░', '▒', '▓', '█'}[min(3, int(c.bucket)/2)]
			if m.theme.ASCII {
				glyph = []rune{'.', ':', '+', '#'}[min(3, int(c.bucket)/2)]
			}
			if m.theme.Name == "mono" || m.theme.Name == "colorblind" {
				glyph = proxyCellASCII(c.state)
			}
			b.Set(labelW+x, g, tui.Cell{Rune: glyph, Style: m.proxyCellStyle(c.state, c.bucket)})
		}
	}
	x := b.Put("Timeline", 0, h, m.theme.Dim)
	for _, c := range cells {
		if c.index == 0 {
			b.Put("    ", x, h, tui.Style{})
			m.drawLegendDirect(b, x+4, h, c)
			break
		}
	}
}
