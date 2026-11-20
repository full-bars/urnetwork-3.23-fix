package urnettools

import (
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
)

type proxyScanFlash struct {
	index  int
	status string
	at     time.Time
}

// applyUDPScanEvents folds new UDP scan check events into the active flashes.
// It discards expired events and keeps the latest event per proxy index.
func (m *topModel) applyUDPScanEvents(events []ProxyUDPScanEvent) {
	if m.proxyHeatmapStyle != heatmapScan {
		m.proxyScanFlashes = nil
		return
	}
	if len(events) == 0 {
		return
	}
	if m.proxyScanFlashes == nil {
		m.proxyScanFlashes = make(map[int]proxyScanFlash, len(events))
	}
	now := m.now()
	seconds := m.proxyAfterglowSeconds
	if seconds <= 0 {
		seconds = defaultProxyAfterglowSeconds
	}
	ttl := time.Duration(seconds) * time.Second
	for _, ev := range events {
		if ev.At.IsZero() {
			ev.At = now
		}
		if now.Sub(ev.At) < ttl {
			if prev, ok := m.proxyScanFlashes[ev.Index]; !ok || !ev.At.Before(prev.at) {
				m.proxyScanFlashes[ev.Index] = proxyScanFlash{
					index:  ev.Index,
					status: ev.Status,
					at:     ev.At,
				}
			}
		}
	}
}

// proxyScanFlashState returns the active flash status and fade fraction (1.0 = peak, 0.0 = expired).
func (m *topModel) proxyScanFlashState(index int, now time.Time) (string, float64, bool) {
	if m.proxyHeatmapStyle != heatmapScan || len(m.proxyScanFlashes) == 0 {
		return "", 0, false
	}
	flash, ok := m.proxyScanFlashes[index]
	if !ok {
		return "", 0, false
	}
	seconds := m.proxyAfterglowSeconds
	if seconds <= 0 {
		seconds = defaultProxyAfterglowSeconds
	}
	age := now.Sub(flash.at).Seconds()
	if age < 0 {
		age = 0
	}
	if age >= float64(seconds) {
		return "", 0, false
	}
	fraction := 1.0 - (age / float64(seconds))
	return flash.status, fraction, true
}

// proxyScanFlashStyle maps the check status and flash fraction to a rune and style.
func (m *topModel) proxyScanFlashStyle(status string, frac float64, baseState uint8) (rune, tui.Style) {
	th := m.theme
	isMono := th.Name == "mono" || th.OK.FG.Kind == tui.ColorDefault
	isColorblind := th.Name == "colorblind"
	isASCII := th.ASCII

	switch status {
	case "no-relay":
		status = "no_relay"
	case "exit-mismatch":
		status = "mismatch"
	}

	var glyph rune
	switch {
	case isASCII || isMono:
		switch status {
		case "ok":
			if frac > 0.5 {
				glyph = '*'
			} else {
				glyph = '+'
			}
		case "no_relay":
			glyph = '!'
		case "mismatch":
			glyph = 'x'
		default:
			glyph = '?'
		}
	case isColorblind:
		switch status {
		case "ok":
			glyph = '●'
		case "no_relay":
			glyph = '▲'
		case "mismatch":
			glyph = '◆'
		default:
			glyph = '■'
		}
	default:
		if frac > 0.5 {
			glyph = '●'
		} else {
			glyph = '•'
		}
	}

	if isMono {
		if frac > 0.5 {
			return glyph, tui.Style{Attrs: tui.AttrBold}
		}
		return glyph, tui.Style{}
	}

	if isColorblind {
		switch status {
		case "ok":
			if frac > 0.5 {
				return glyph, th.OK.With(tui.AttrBold)
			}
			return glyph, th.OK
		case "no_relay":
			if frac > 0.5 {
				return glyph, th.Warn.With(tui.AttrBold)
			}
			return glyph, th.Warn
		case "mismatch":
			if frac > 0.5 {
				return glyph, th.Bad.With(tui.AttrBold)
			}
			return glyph, th.Bad
		default:
			if frac > 0.5 {
				return glyph, th.Dim.With(tui.AttrBold)
			}
			return glyph, th.Dim
		}
	}

	switch status {
	case "ok":
		switch {
		case frac >= 0.75:
			return glyph, tui.Style{FG: tui.Xterm(46), Attrs: tui.AttrBold}
		case frac >= 0.50:
			return glyph, tui.Style{FG: tui.Xterm(82)}
		case frac >= 0.25:
			return glyph, tui.Style{FG: tui.Xterm(34)}
		default:
			return glyph, tui.Style{FG: tui.Xterm(28)}
		}
	case "no_relay":
		switch {
		case frac >= 0.75:
			return glyph, tui.Style{FG: tui.Xterm(214), Attrs: tui.AttrBold}
		case frac >= 0.50:
			return glyph, tui.Style{FG: tui.Xterm(208)}
		case frac >= 0.25:
			return glyph, tui.Style{FG: tui.Xterm(202)}
		default:
			return glyph, tui.Style{FG: tui.Xterm(166)}
		}
	case "mismatch":
		switch {
		case frac >= 0.75:
			return glyph, tui.Style{FG: tui.Xterm(177), Attrs: tui.AttrBold}
		case frac >= 0.50:
			return glyph, tui.Style{FG: tui.Xterm(141)}
		case frac >= 0.25:
			return glyph, tui.Style{FG: tui.Xterm(135)}
		default:
			return glyph, tui.Style{FG: tui.Xterm(99)}
		}
	default:
		if frac >= 0.5 {
			return glyph, th.Dim.With(tui.AttrBold)
		}
		return glyph, th.Dim
	}
}

// drawProxyScan renders the UDP scan wave animation across the proxy grid.
func (m *topModel) drawProxyScan(b *tui.Buffer) {
	th := m.theme
	cells := m.proxyCells
	if len(cells) == 0 {
		b.Put("no proxies", 0, 0, th.Dim)
		return
	}
	w, h := b.Width(), b.Height()
	gridH := h - 1
	if gridH < 1 {
		gridH = 1
	}
	m.proxyGridW, m.proxyGridH = w, gridH
	hasDirect := false
	var directCell proxyCell
	for _, c := range cells {
		if c.index == 0 {
			hasDirect = true
			directCell = c
			break
		}
	}

	layout := newProxyGridLayout(len(cells), w, gridH, hasDirect)
	capacity := layout.proxyCapacity()
	group := 1
	if hasDirect {
		numProxies := len(cells) - 1
		if capacity > 0 && numProxies > capacity {
			group = (numProxies + capacity - 1) / capacity
		}
	} else if capacity > 0 && len(cells) > capacity {
		group = (len(cells) + capacity - 1) / capacity
	}
	if group < 1 {
		group = 1
	}

	now := m.now()
	ascii := th.ASCII

	for y := 0; y < gridH; y++ {
		for x := 0; x < w; x++ {
			if hasDirect && x == 0 && y == 0 {
				glyph, st := m.directCellStar(directCell, now)
				b.Set(0, 0, tui.Cell{Rune: glyph, Style: st})
				continue
			}
			if layout.sparse {
				idx, _ := layout.indices(x, y)
				if idx < 0 || idx >= len(cells) {
					continue
				}
				c := cells[idx]
				if c.index == 0 {
					glyph, st := m.directCellStar(c, now)
					b.Set(x, y, tui.Cell{Rune: glyph, Style: st})
					continue
				}
				if status, frac, flashing := m.proxyScanFlashState(c.index, now); flashing {
					glyph, st := m.proxyScanFlashStyle(status, frac, c.state)
					b.Set(x, y, tui.Cell{Rune: glyph, Style: st})
					continue
				}
				bucket := m.proxyGlowBucket(c, now)
				st := m.proxyCellStyle(c.state, bucket)
				if ascii && (th.Name != "mono" || layout.sparse) {
					glyph := proxyCellASCII(c.state)
					if c.state == connect.ProxyCellUp {
						glyph = []rune{'.', ':', '+', '*'}[min(int(bucket)/2, 3)]
					}
					b.Set(x, y, tui.Cell{Rune: glyph, Style: st})
					continue
				}
				b.Set(x, y, tui.Cell{Rune: '∙', Style: st})
				continue
			}

			upperIdx, lowerIdx := layout.indices(x, y)
			var upperFlashing, lowerFlashing bool
			var upperStatus, lowerStatus string
			var upperFrac, lowerFrac float64

			var worst uint8 = connect.ProxyCellUp
			var worstBucket uint8
			upperState, lowerState := uint8(connect.ProxyCellUp), uint8(connect.ProxyCellUp)
			var upperBucket, lowerBucket uint8
			hasUpper, hasLower := false, false

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
					if st, fr, fl := m.proxyScanFlashState(cells[ui].index, now); fl {
						if !upperFlashing || fr > upperFrac {
							upperFlashing, upperStatus, upperFrac = true, st, fr
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
					if st, fr, fl := m.proxyScanFlashState(cells[li].index, now); fl {
						if !lowerFlashing || fr > lowerFrac {
							lowerFlashing, lowerStatus, lowerFrac = true, st, fr
						}
					}
				}
			}

			if !hasUpper && !hasLower {
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
						upperStyle = m.proxyCellStyle(worst, worstBucket)
					}
				} else {
					upperStyle.BG = lowerStyle.FG
				}
			}
			b.Set(x, y, tui.Cell{Rune: glyph, Style: upperStyle})
		}
	}

	x := b.Put("Scan", 0, gridH, th.Dim)
	m.record("legend:style", b.Absolute(tui.Rect{X: 0, Y: gridH, W: 4, H: 1}))
	for _, c := range cells {
		if c.index == 0 {
			b.Put("    ", x, gridH, tui.Style{})
			m.drawLegendDirect(b, x+4, gridH, c)
			break
		}
	}
}
