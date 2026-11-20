package tui

import (
	"math"
	"sort"
)

type peakCandidate struct {
	idx    int
	v      float64
	isPeak bool
}

// drawPeakLabels places a bounded number of labels above completed local peaks
// and below local valleys when there is room. It leaves a cell of clearance
// around the plot edges and between labels, and prioritizes larger extrema so
// labels remain stable as the graph scrolls.
func drawPeakLabels(b *Buffer, cols []float64, perCell int, top float64, format func(float64) string, style Style) {
	if b.Height() < 3 || b.Width() < 4 || top <= 0 || perCell < 1 || len(cols) < 3 {
		return
	}

	var candidates []peakCandidate
	for i := 1; i+1 < len(cols); i++ {
		v := cols[i]
		if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
			continue
		}
		prev, next := cols[i-1], cols[i+1]
		if math.IsNaN(prev) || math.IsNaN(next) {
			continue
		}
		if v > prev && v > next {
			candidates = append(candidates, peakCandidate{idx: i, v: v, isPeak: true})
		} else if v < prev && v < next && v > 0 {
			candidates = append(candidates, peakCandidate{idx: i, v: v, isPeak: false})
		}
	}

	if len(candidates) == 0 {
		return
	}

	var peaks, valleys []peakCandidate
	for _, c := range candidates {
		if c.isPeak {
			peaks = append(peaks, c)
		} else {
			valleys = append(valleys, c)
		}
	}

	sort.SliceStable(peaks, func(i, j int) bool {
		return peaks[i].v > peaks[j].v
	})
	sort.SliceStable(valleys, func(i, j int) bool {
		return valleys[i].v < valleys[j].v
	})

	var prioritized []peakCandidate
	for i := 0; i < len(peaks) && len(prioritized) < 2; i++ {
		prioritized = append(prioritized, peaks[i])
	}
	if len(valleys) > 0 {
		prioritized = append(prioritized, valleys[0])
	}
	if len(prioritized) < 3 && len(peaks) > 2 {
		prioritized = append(prioritized, peaks[2])
	}

	type box struct{ x, y, end int }
	var placed []box

	h := b.Height()
	for _, c := range prioritized {
		label := format(c.v)
		width := StringWidth(label)
		if width == 0 {
			continue
		}

		x := c.idx/perCell - width/2
		if x < 1 || x+width >= b.Width() {
			continue
		}

		yPoint := h - 1 - int(math.Round(math.Min(c.v/top, 1)*float64(h-1)))
		var y int
		if c.isPeak {
			y = yPoint - 1
			if y < 0 {
				y = 0
			}
		} else {
			y = yPoint + 1
			if y >= h {
				y = yPoint - 1
			}
		}

		if y < 0 || y >= h {
			continue
		}

		overlap := false
		for _, p := range placed {
			if y == p.y && x-1 <= p.end && x+width >= p.x-1 {
				overlap = true
				break
			}
		}

		if !overlap {
			b.Put(label, x, y, style)
			placed = append(placed, box{x: x, y: y, end: x + width})
		}
	}
}
