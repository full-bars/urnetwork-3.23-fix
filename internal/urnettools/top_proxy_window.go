package urnettools

import (
	"fmt"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
)

// scrollProxyWin moves the vertical scroll offset in the proxy window by delta.
func (m *topModel) scrollProxyWin(delta int) {
	m.proxyWinScroll = min(max(0, m.proxyWinScroll+delta), m.proxyWinMaxScroll)
}

// drawProxyWindow renders the near full-screen proxy heatmap window.
// It fills the available terminal, shows larger detail (cell width 2 where it fits),
// scrolls or zooms when the grid exceeds the screen, supports hover/click popups,
// and closes with Esc or p.
func (m *topModel) drawProxyWindow(b *tui.Buffer) {
	th := m.theme
	w, h := b.Width(), b.Height()
	if w < 4 || h < 4 {
		return
	}

	winW, winH := w, h
	winX, winY := 0, 0
	if w >= 30 && h >= 10 {
		winW, winH = w-2, h-2
		winX, winY = 1, 1
	}
	r := tui.Rect{X: winX, Y: winY, W: winW, H: winH}
	m.proxyWinRect = r

	title := "Proxies"
	if len(m.proxyCells) > 0 {
		title = fmt.Sprintf("Proxies (%d)", len(m.proxyCells))
	}
	in := tui.DrawBox(b.Sub(r), title, th.Frame, th.Border, th.Accent, th.ASCII)
	in.Fill(in.Rect(), ' ', tui.Style{})
	if in.Height() < 2 || in.Width() < 4 {
		return
	}

	cells := m.proxyCells
	if len(cells) == 0 {
		in.Put("no proxies", 0, 0, th.Dim)
		in.Put("Esc or p closes", 0, in.Height()-1, th.Dim)
		return
	}

	legendY := in.Height() - 2
	gridH := in.Height() - 2
	if gridH < 1 {
		gridH = 1
	}
	gridW := in.Width()

	hasDirect := false
	var directCell proxyCell
	for _, c := range cells {
		if c.index == 0 {
			hasDirect = true
			directCell = c
			break
		}
	}
	numProxies := len(cells)
	if hasDirect {
		numProxies--
	}

	cols2 := max(1, gridW/2)
	cap2Sparse := cols2 * gridH
	if hasDirect {
		cap2Sparse--
	}
	cap2Dense := cols2 * (2 * gridH)
	if hasDirect {
		cap2Dense = 2 * (cols2*gridH - 1)
	}

	cellWidth := 1
	sparse := false
	if numProxies <= cap2Sparse {
		cellWidth = 2
		sparse = true
	} else if numProxies <= cap2Dense {
		cellWidth = 2
		sparse = false
	} else {
		cellWidth = 1
		sparse = false
	}

	m.proxyWinCellWidth = cellWidth

	cols := gridW
	if cellWidth == 2 {
		cols = cols2
	}

	var virtualH int
	var totalVirtualRows int
	group := 1

	if sparse {
		totalVirtualRows = (len(cells) + cols - 1) / cols
		if totalVirtualRows < gridH {
			totalVirtualRows = gridH
		}
		if m.proxyWinZoom {
			virtualH = gridH
			m.proxyWinMaxScroll = 0
			m.proxyWinScroll = 0
		} else {
			virtualH = totalVirtualRows
			m.proxyWinMaxScroll = max(0, totalVirtualRows-gridH)
			m.proxyWinScroll = min(max(0, m.proxyWinScroll), m.proxyWinMaxScroll)
		}
	} else {
		if hasDirect {
			totalVirtualRows = (numProxies + 2 + 2*cols - 1) / (2 * cols)
		} else {
			totalVirtualRows = (len(cells) + 2*cols - 1) / (2 * cols)
		}
		if totalVirtualRows < gridH {
			totalVirtualRows = gridH
		}

		if m.proxyWinZoom {
			virtualH = gridH
			layout := newProxyGridLayout(len(cells), cols, gridH, hasDirect)
			capacity := layout.proxyCapacity()
			if hasDirect {
				if capacity > 0 && numProxies > capacity {
					group = (numProxies + capacity - 1) / capacity
				}
			} else {
				if capacity > 0 && len(cells) > capacity {
					group = (len(cells) + capacity - 1) / capacity
				}
			}
			if group < 1 {
				group = 1
			}
			m.proxyWinMaxScroll = 0
			m.proxyWinScroll = 0
		} else {
			virtualH = totalVirtualRows
			m.proxyWinMaxScroll = max(0, totalVirtualRows-gridH)
			m.proxyWinScroll = min(max(0, m.proxyWinScroll), m.proxyWinMaxScroll)
		}
	}

	scroll := m.proxyWinScroll
	m.proxyGridW = cols
	m.proxyGridH = virtualH
	m.proxyPopupRect = in.Absolute(tui.Rect{X: 0, Y: 0, W: gridW, H: gridH})

	footerY := in.Height() - 1
	in.Put("Esc or p closes", 0, footerY, th.Dim)
	if in.Width() >= 36 {
		hint := "z zoom  wheel/arrows scroll"
		in.Put(hint, in.Width()-tui.StringWidth(hint), footerY, th.Dim)
	}

	modeStr := ""
	if m.proxyWinZoom {
		modeStr = "[fit]"
	} else if m.proxyWinMaxScroll > 0 {
		modeStr = fmt.Sprintf("[%d-%d/%d]", m.proxyWinScroll+1, min(m.proxyWinScroll+gridH, totalVirtualRows), totalVirtualRows)
	}
	if modeStr != "" && in.Width() >= 45 {
		midX := (in.Width() - tui.StringWidth(modeStr)) / 2
		in.Put(modeStr, midX, footerY, th.Dim)
	}

	layout := newProxyGridLayout(len(cells), cols, virtualH, hasDirect)
	capacity := layout.proxyCapacity()

	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	ascii := th.ASCII

	for y := 0; y < gridH; y++ {
		vy := y + scroll
		for col := 0; col < cols; col++ {
			sx0 := col
			sx1 := col
			if cellWidth == 2 {
				sx0 = 2 * col
				sx1 = 2*col + 1
			}

			if hasDirect && col == 0 && vy == 0 {
				glyph, st := m.directCellStar(directCell, now)
				in.Set(sx0, y, tui.Cell{Rune: glyph, Style: st})
				if cellWidth == 2 {
					in.Set(sx1, y, tui.Cell{Rune: ' ', Style: st})
				}
				continue
			}

			if sparse {
				idx, _ := layout.indices(col, vy)
				if idx < 0 || idx >= len(cells) {
					continue
				}
				c := cells[idx]
				if c.index == 0 {
					glyph, st := m.directCellStar(c, now)
					in.Set(sx0, y, tui.Cell{Rune: glyph, Style: st})
					if cellWidth == 2 {
						in.Set(sx1, y, tui.Cell{Rune: ' ', Style: st})
					}
					continue
				}
				if m.proxyHeatmapStyle == heatmapScan {
					if status, frac, flashing := m.proxyScanFlashState(c.index, now); flashing {
						glyph, st := m.proxyScanFlashStyle(status, frac, c.state)
						in.Set(sx0, y, tui.Cell{Rune: glyph, Style: st})
						if cellWidth == 2 {
							in.Set(sx1, y, tui.Cell{Rune: ' ', Style: st})
						}
						continue
					}
				}
				bucket := m.proxyGlowBucket(c, now)
				st := m.proxyCellStyle(c.state, bucket)
				if ascii && (th.Name != "mono" || layout.sparse) {
					glyph := proxyCellASCII(c.state)
					if c.state == connect.ProxyCellUp {
						glyph = []rune{'.', ':', '+', '*'}[min(int(bucket)/2, 3)]
					}
					in.Set(sx0, y, tui.Cell{Rune: glyph, Style: st})
					if cellWidth == 2 {
						in.Set(sx1, y, tui.Cell{Rune: ' ', Style: st})
					}
					continue
				}
				in.Set(sx0, y, tui.Cell{Rune: '∙', Style: st})
				if cellWidth == 2 {
					in.Set(sx1, y, tui.Cell{Rune: ' ', Style: st})
				}
				continue
			}

			upperIdx, lowerIdx := layout.indices(col, vy)
			var worst uint8 = connect.ProxyCellUp
			var worstBucket uint8
			upperState, lowerState := uint8(connect.ProxyCellUp), uint8(connect.ProxyCellUp)
			var upperBucket, lowerBucket uint8
			hasUpper, hasLower := false, false
			var upperFlashing, lowerFlashing bool
			var upperStatus, lowerStatus string
			var upperFrac, lowerFrac float64

			for k := 0; k < group; k++ {
				if ui := upperIdx + k*capacity; upperIdx >= 0 && ui < len(cells) {
					hasUpper = true
					if proxyCellSeverity(cells[ui].state) > proxyCellSeverity(upperState) {
						upperState = cells[ui].state
					}
					upperBucket = max(upperBucket, m.proxyGlowBucket(cells[ui], now))
					if proxyCellSeverity(cells[ui].state) > proxyCellSeverity(worst) {
						worst = cells[ui].state
					}
					if bucket := m.proxyGlowBucket(cells[ui], now); bucket > worstBucket {
						worstBucket = bucket
					}
					if m.proxyHeatmapStyle == heatmapScan {
						if st, fr, fl := m.proxyScanFlashState(cells[ui].index, now); fl {
							if !upperFlashing || fr > upperFrac {
								upperFlashing, upperStatus, upperFrac = true, st, fr
							}
						}
					}
				}
				if li := lowerIdx + k*capacity; lowerIdx >= 0 && li < len(cells) {
					hasLower = true
					if proxyCellSeverity(cells[li].state) > proxyCellSeverity(lowerState) {
						lowerState = cells[li].state
					}
					lowerBucket = max(lowerBucket, m.proxyGlowBucket(cells[li], now))
					if proxyCellSeverity(cells[li].state) > proxyCellSeverity(worst) {
						worst = cells[li].state
					}
					if bucket := m.proxyGlowBucket(cells[li], now); bucket > worstBucket {
						worstBucket = bucket
					}
					if m.proxyHeatmapStyle == heatmapScan {
						if st, fr, fl := m.proxyScanFlashState(cells[li].index, now); fl {
							if !lowerFlashing || fr > lowerFrac {
								lowerFlashing, lowerStatus, lowerFrac = true, st, fr
							}
						}
					}
				}
			}

			if !hasUpper && !hasLower {
				continue
			}

			st := m.proxyCellStyle(worst, worstBucket)
			if ascii && (th.Name != "mono" || layout.sparse) {
				glyph := proxyCellASCII(worst)
				if worst == connect.ProxyCellUp {
					glyph = []rune{'.', ':', '+', '*'}[min(int(worstBucket)/2, 3)]
				}
				in.Set(sx0, y, tui.Cell{Rune: glyph, Style: st})
				if cellWidth == 2 {
					in.Set(sx1, y, tui.Cell{Rune: ' ', Style: st})
				}
				continue
			}

			var upperStyle, lowerStyle tui.Style
			if upperFlashing {
				_, upperStyle = m.proxyScanFlashStyle(upperStatus, upperFrac, upperState)
			} else {
				upperStyle = m.proxyCellStyle(upperState, upperBucket)
			}
			if lowerFlashing {
				_, lowerStyle = m.proxyScanFlashStyle(lowerStatus, lowerFrac, lowerState)
			} else {
				lowerStyle = m.proxyCellStyle(lowerState, lowerBucket)
			}

			glyph := '▀'
			if !hasUpper {
				glyph = '▄'
				upperStyle = lowerStyle
			}
			if hasUpper && hasLower {
				if th.Name == "colorblind" || th.Name == "mono" || upperStyle.FG.Kind == tui.ColorDefault || lowerStyle.FG.Kind == tui.ColorDefault {
					glyph = '█'
					if upperFlashing && (!lowerFlashing || upperFrac >= lowerFrac) {
						_, upperStyle = m.proxyScanFlashStyle(upperStatus, upperFrac, worst)
					} else if lowerFlashing {
						_, upperStyle = m.proxyScanFlashStyle(lowerStatus, lowerFrac, worst)
					} else {
						upperStyle = st
					}
				} else {
					upperStyle.BG = lowerStyle.FG
				}
			}
			in.Set(sx0, y, tui.Cell{Rune: glyph, Style: upperStyle})
			if cellWidth == 2 {
				in.Set(sx1, y, tui.Cell{Rune: glyph, Style: upperStyle})
			}
		}
	}

	// Legend row
	counts := [5]int{}
	for _, c := range cells {
		if int(c.state) < len(counts) {
			if c.index != 0 {
				counts[c.state]++
			}
		}
	}
	names := []string{"up", "dropped", "connecting", "dead", "parked"}
	legend := fmt.Sprintf("%d up  %d dropped  %d connecting  %d dead  %d parked", counts[0], counts[1], counts[2], counts[3], counts[4])
	totalWidth := tui.StringWidth(legend)
	if hasDirect {
		star := "★"
		if th.ASCII {
			star = "*"
		}
		totalWidth += tui.StringWidth(star+" direct "+proxyCellStateName(directCell.state)) + 2
	}
	if totalWidth > in.Width() {
		names = []string{"up", "drop", "conn", "dead", "park"}
	}
	lx := 0
	for _, c := range cells {
		if c.index == 0 {
			lx = m.drawLegendDirect(in, lx, legendY, c)
			break
		}
	}
	for i, name := range names {
		count := counts[i]
		st := m.proxyCellStyle(uint8(i), 0)
		if count == 0 {
			st = th.Dim
		}
		if lx > 0 && lx < in.Width() {
			in.Put("  ", lx, legendY, tui.Style{})
			lx += 2
		}
		text := fmt.Sprintf("%d %s", count, name)
		if lx+tui.StringWidth(text) > in.Width() {
			break
		}
		lx = in.Put(text, lx, legendY, st)
	}
}
