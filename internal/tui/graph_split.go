package tui

import (
	"fmt"
	"math"
)

var (
	sparkASCIIDown  = []rune("~-._=+*#")
	sparkBlocksDown = []rune{'▔', '▔', '▀', '▀', '█', '█', '█', '█'}
)

// SplitGraph configures a mirrored bidirectional rate graph (like btop):
// download (Rx) is drawn above the center line, upload (Tx) is mirrored below it.
type SplitGraph struct {
	RxSamples  []float64
	TxSamples  []float64
	Max        float64
	MaxRx      float64
	MaxTx      float64
	Binary     bool
	Format     func(float64) string
	StyleRx    Style
	StyleTx    Style
	AxisStyle  Style
	Anchor     int64
	Capacity   int
	TailRx     float64
	TailTx     float64
	HasTail    bool
	Symbols    GraphSymbols
	PeakLabels bool
}

// DrawSplitGraph renders g into b with Download above the center and Upload below it.
// It returns the scales (shownRx, shownTx).
func DrawSplitGraph(b *Buffer, g SplitGraph, ascii bool) (float64, float64) {
	w, h := b.Width(), b.Height()
	if w <= 0 || h <= 0 {
		return 0, 0
	}
	format := g.Format
	if format == nil {
		format = func(v float64) string { return fmt.Sprintf("%g", v) }
	}

	clean := func(samples []float64) ([]float64, float64) {
		out := make([]float64, len(samples))
		peak := 0.0
		for i, v := range samples {
			if math.IsNaN(v) {
				out[i] = v
				continue
			}
			if math.IsInf(v, 0) || v < 0 {
				v = 0
			}
			out[i] = v
			if v > peak {
				peak = v
			}
		}
		return out, peak
	}
	rxSamples, peakRx := clean(g.RxSamples)
	txSamples, peakTx := clean(g.TxSamples)
	if g.HasTail {
		if !math.IsNaN(g.TailRx) && g.TailRx > peakRx {
			peakRx = g.TailRx
		}
		if !math.IsNaN(g.TailTx) && g.TailTx > peakTx {
			peakTx = g.TailTx
		}
	}

	topRx, topTx := g.Max, g.Max
	if g.Max <= 0 {
		if g.MaxRx > 0 {
			topRx = g.MaxRx
		} else {
			topRx = niceCeil(peakRx, g.Binary)
		}
		if g.MaxTx > 0 {
			topTx = g.MaxTx
		} else {
			topTx = niceCeil(peakTx, g.Binary)
		}
	}
	flatRx, flatTx := topRx <= 0, topTx <= 0
	if flatRx {
		topRx = 1
	}
	if flatTx {
		topTx = 1
	}
	shownRx, shownTx := topRx, topTx
	if flatRx {
		shownRx = 0
	}
	if flatTx {
		shownTx = 0
	}

	hRx := h / 2
	if hRx < 1 {
		hRx = 1
	}
	hTx := h - hRx
	if hTx < 1 {
		hTx = 1
	}

	labels := make(map[int]string)
	labels[0] = format(shownRx)
	centerRow := hRx - 1
	if h <= 2 {
		centerRow = 0
	}
	labels[centerRow] = format(0)
	labels[h-1] = format(shownTx)

	if hRx >= 4 && shownRx > 0 {
		labels[hRx/2] = format(shownRx / 2)
	}
	if hTx >= 4 && shownTx > 0 {
		labels[hRx+hTx/2] = format(shownTx / 2)
	}

	axis := 0
	for _, l := range labels {
		axis = max(axis, StringWidth(l))
	}
	x0 := axis + 1
	if w-x0 < 1 {
		axis, x0, labels = 0, 0, nil
	}

	for y := 0; y < h; y++ {
		if x0 == 0 {
			break
		}
		tick := '│'
		if ascii {
			tick = '|'
		}
		if l, ok := labels[y]; ok {
			tick = '┤'
			if ascii {
				tick = '+'
			}
			b.Put(l, axis-StringWidth(l), y, g.AxisStyle)
		}
		b.Set(axis, y, Cell{Rune: tick, Style: g.AxisStyle})
	}

	plotW := w - x0
	if plotW < 1 {
		return shownRx, shownTx
	}

	perCell := 2
	if g.Symbols == GraphBlock && !ascii {
		perCell = 1
	}

	sampleColumns := func(samples []float64, tail float64, top float64) []float64 {
		if g.HasTail {
			histCols, tailCols := perCell*plotW-1, 1
			if ascii || g.Symbols == GraphTTY {
				histCols, tailCols = 2*plotW-2, 2
			}
			var cols []float64
			if histCols > 0 {
				cols = plotColumnsAnchored(samples, histCols, g.Anchor, g.Capacity)
			}
			tVal := tail
			if math.IsNaN(tVal) || math.IsInf(tVal, 0) || tVal < 0 {
				tVal = 0
			}
			for i := 0; i < tailCols; i++ {
				cols = append(cols, math.Min(tVal, top))
			}
			return cols
		}
		return plotColumnsAnchored(samples, perCell*plotW, g.Anchor, g.Capacity)
	}

	colsRx := sampleColumns(rxSamples, g.TailRx, topRx)
	colsTx := sampleColumns(txSamples, g.TailTx, topTx)

	plotBuf := b.Sub(Rect{X: x0, Y: 0, W: plotW, H: h})
	if g.PeakLabels && h >= 4 {
		defer func() {
			drawPeakLabels(b.Sub(Rect{X: x0, Y: 0, W: plotW, H: hRx}), colsRx, perCell, topRx, format, g.AxisStyle)
			drawPeakLabels(b.Sub(Rect{X: x0, Y: hRx, W: plotW, H: hTx}), colsTx, perCell, topTx, format, g.AxisStyle)
		}()
	}

	if ascii || g.Symbols == GraphTTY {
		drawGraphCells(b.Sub(Rect{X: x0, Y: 0, W: plotW, H: hRx}), colsRx, 2, topRx, g.StyleRx, sparkASCII)
		drawGraphCellsDown(b.Sub(Rect{X: x0, Y: hRx, W: plotW, H: hTx}), colsTx, 2, topTx, g.StyleTx, sparkASCIIDown)
		return shownRx, shownTx
	}

	if g.Symbols == GraphBlock {
		drawGraphCells(b.Sub(Rect{X: x0, Y: 0, W: plotW, H: hRx}), colsRx, 1, topRx, g.StyleRx, sparkBlocks)
		drawGraphCellsDown(b.Sub(Rect{X: x0, Y: hRx, W: plotW, H: hTx}), colsTx, 1, topTx, g.StyleTx, sparkBlocksDown)
		return shownRx, shownTx
	}

	dotsTotal := 4 * h
	dotsCenter := 4 * hRx
	dotsRx := dotsCenter
	dotsTx := dotsTotal - dotsCenter
	if dotsRx < 1 {
		dotsRx = 1
	}
	if dotsTx < 1 {
		dotsTx = 1
	}

	heightsRx := make([]int, len(colsRx))
	for i, v := range colsRx {
		heightsRx[i] = -1
		if !math.IsNaN(v) {
			heightsRx[i] = min(max(int(math.Round(v/topRx*float64(dotsRx))), 1), dotsRx)
		}
	}
	heightsTx := make([]int, len(colsTx))
	for i, v := range colsTx {
		heightsTx[i] = -1
		if !math.IsNaN(v) {
			heightsTx[i] = min(max(int(math.Round(v/topTx*float64(dotsTx))), 1), dotsTx)
		}
	}

	for cx := 0; cx < plotW; cx++ {
		for row := 0; row < h; row++ {
			var mask rune
			rowBase := row * 4
			st := g.StyleRx
			if row >= hRx {
				st = g.StyleTx
			}
			for k := 0; k < 4; k++ {
				dotPos := rowBase + k
				leftLit := false
				if dotPos < dotsCenter {
					dist := dotsCenter - 1 - dotPos
					if 2*cx < len(heightsRx) && heightsRx[2*cx] > dist {
						leftLit = true
					}
				} else {
					dist := dotPos - dotsCenter
					if 2*cx < len(heightsTx) && heightsTx[2*cx] > dist {
						leftLit = true
					}
				}
				if leftLit {
					mask |= brailleLeft[k]
				}

				rightLit := false
				if dotPos < dotsCenter {
					dist := dotsCenter - 1 - dotPos
					if 2*cx+1 < len(heightsRx) && heightsRx[2*cx+1] > dist {
						rightLit = true
					}
				} else {
					dist := dotPos - dotsCenter
					if 2*cx+1 < len(heightsTx) && heightsTx[2*cx+1] > dist {
						rightLit = true
					}
				}
				if rightLit {
					mask |= brailleRight[k]
				}
			}
			if mask != 0 {
				plotBuf.Set(cx, row, Cell{Rune: 0x2800 + mask, Style: st})
			}
		}
	}

	return shownRx, shownTx
}

func drawGraphCellsDown(b *Buffer, cols []float64, perCell int, top float64, st Style, ramp []rune) {
	h := b.Height()
	levels := len(ramp)
	for cx := 0; cx < b.Width(); cx++ {
		if perCell*cx >= len(cols) {
			break
		}
		end := min(perCell*cx+perCell, len(cols))
		sum, n := 0.0, 0
		for _, v := range cols[perCell*cx : end] {
			if !math.IsNaN(v) {
				sum += v
				n++
			}
		}
		if n == 0 {
			continue
		}
		rem := min(max(int(math.Round(sum/float64(n)/top*float64(h*levels))), 1), h*levels)
		for row := 0; row < h && rem > 0; row++ {
			take := min(rem, levels)
			b.Set(cx, row, Cell{Rune: ramp[take-1], Style: st})
			rem -= take
		}
	}
}
