// Package tui holds the small, dependency-free drawing helpers shared by
// `urnet-tools status` and `urnet-tools top`.
package tui

import "strings"

var sparkBlocks = []rune("▁▂▃▄▅▆▇█")

// sparkASCII is the fallback for terminals that cannot draw block glyphs
// (NO_COLOR is not the trigger; TERM=dumb and non-UTF-8 locales are).
var sparkASCII = []rune("_.-~=+*#")

// Spark renders values as a one-line sparkline exactly width cells wide.
// Values are scaled to the largest value in the input (zero is the floor),
// so an all-zero series draws as a flat baseline. When there are more values
// than cells the series is averaged into width buckets; when there are fewer,
// it is right-aligned and left-padded with spaces so the newest sample is
// always the last cell.
func Spark(values []uint64, width int, ascii bool) string {
	if width <= 0 {
		return ""
	}
	glyphs := sparkBlocks
	if ascii {
		glyphs = sparkASCII
	}
	if len(values) == 0 {
		return strings.Repeat(" ", width)
	}
	cells := values
	if len(values) > width {
		cells = make([]uint64, width)
		for i := 0; i < width; i++ {
			lo := i * len(values) / width
			hi := (i + 1) * len(values) / width
			if hi <= lo {
				hi = lo + 1
			}
			var avg float64
			count := float64(hi - lo)
			for _, v := range values[lo:hi] {
				avg += float64(v) / count
			}
			cells[i] = uint64(avg)
		}
	}
	var max uint64
	for _, v := range cells {
		if v > max {
			max = v
		}
	}
	var b strings.Builder
	b.WriteString(strings.Repeat(" ", width-len(cells)))
	for _, v := range cells {
		idx := 0
		if max > 0 {
			if v > max {
				v = max
			}
			idx = int((float64(v) / float64(max)) * float64(len(glyphs)-1))
			if idx >= len(glyphs) {
				idx = len(glyphs) - 1
			}
		}
		b.WriteRune(glyphs[idx])
	}
	return b.String()
}
