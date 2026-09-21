package tui

import "unicode/utf8"

type runeRange struct{ lo, hi rune }

// zeroWidth lists combining marks and format characters that occupy no cell
// of their own. It is deliberately small: the strings we draw are metric
// names, proxy ids and event text, not arbitrary prose.
var zeroWidth = []runeRange{
	{0x0300, 0x036F}, {0x0483, 0x0489}, {0x0591, 0x05BD}, {0x05BF, 0x05BF},
	{0x05C1, 0x05C2}, {0x05C4, 0x05C5}, {0x05C7, 0x05C7}, {0x0610, 0x061A},
	{0x064B, 0x065F}, {0x0670, 0x0670}, {0x06D6, 0x06DC}, {0x06DF, 0x06E4},
	{0x0E31, 0x0E31}, {0x0E34, 0x0E3A}, {0x0E47, 0x0E4E}, {0x1AB0, 0x1AFF},
	{0x1DC0, 0x1DFF}, {0x200B, 0x200F}, {0x2028, 0x202E}, {0x2060, 0x2064},
	{0x20D0, 0x20FF}, {0xFE00, 0xFE0F}, {0xFE20, 0xFE2F}, {0xFEFF, 0xFEFF},
	{0xE0100, 0xE01EF},
}

// wide lists East Asian wide and fullwidth blocks plus the emoji planes.
// East Asian "ambiguous" runes (box drawing, blocks, braille) are treated as
// narrow, because that is how the terminals we target draw them.
var wide = []runeRange{
	{0x1100, 0x115F}, {0x231A, 0x231B}, {0x2329, 0x232A}, {0x23E9, 0x23EC},
	{0x23F0, 0x23F0}, {0x23F3, 0x23F3}, {0x25FD, 0x25FE}, {0x2614, 0x2615},
	{0x2648, 0x2653}, {0x267F, 0x267F}, {0x2693, 0x2693}, {0x26A1, 0x26A1},
	{0x26AA, 0x26AB}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5}, {0x26CE, 0x26CE},
	{0x26D4, 0x26D4}, {0x26EA, 0x26EA}, {0x26F2, 0x26F3}, {0x26F5, 0x26F5},
	{0x26FA, 0x26FA}, {0x26FD, 0x26FD}, {0x2705, 0x2705}, {0x270A, 0x270B},
	{0x2728, 0x2728}, {0x274C, 0x274C}, {0x274E, 0x274E}, {0x2753, 0x2755},
	{0x2757, 0x2757}, {0x2795, 0x2797}, {0x27B0, 0x27B0}, {0x27BF, 0x27BF},
	{0x2B1B, 0x2B1C}, {0x2B50, 0x2B50}, {0x2B55, 0x2B55}, {0x2E80, 0x303E},
	{0x3041, 0x33FF}, {0x3400, 0x4DBF}, {0x4E00, 0x9FFF}, {0xA000, 0xA4CF},
	{0xA960, 0xA97F}, {0xAC00, 0xD7A3}, {0xF900, 0xFAFF}, {0xFE10, 0xFE19},
	{0xFE30, 0xFE6F}, {0xFF00, 0xFF60}, {0xFFE0, 0xFFE6}, {0x1F300, 0x1F64F},
	{0x1F680, 0x1F6FF}, {0x1F900, 0x1F9FF}, {0x1FA70, 0x1FAFF},
	{0x20000, 0x2FFFD}, {0x30000, 0x3FFFD},
}

func inRanges(r rune, ranges []runeRange) bool {
	for _, x := range ranges {
		if r < x.lo {
			return false
		}
		if r <= x.hi {
			return true
		}
	}
	return false
}

// isControl reports C0 and C1 control characters. They are never drawn, since
// writing one to a terminal would move the cursor or start an escape.
func isControl(r rune) bool { return r < 0x20 || (r >= 0x7F && r < 0xA0) }

// RuneWidth returns the number of terminal cells a rune occupies: 0 for
// combining marks and controls, 2 for wide runes, otherwise 1.
func RuneWidth(r rune) int {
	switch {
	case r < 0x7F && r >= 0x20:
		return 1 // fast path for ASCII
	case isControl(r):
		return 0
	case inRanges(r, zeroWidth):
		return 0
	case inRanges(r, wide):
		return 2
	}
	return 1
}

// StringWidth returns the number of terminal cells a string occupies.
func StringWidth(s string) int {
	n := 0
	for _, r := range s {
		n += RuneWidth(r)
	}
	return n
}

// Truncate shortens s to at most width cells. When it has to cut, the last
// cell is an ellipsis (three dots when ascii is set and there is room), and a
// wide rune that would straddle the limit is dropped whole rather than split.
func Truncate(s string, width int, ascii bool) string {
	if width <= 0 {
		return ""
	}
	if StringWidth(s) <= width {
		return s
	}
	ell := "…"
	if ascii {
		ell = "."
		if width >= 4 {
			ell = "..."
		}
	}
	room := width - StringWidth(ell)
	if room < 0 {
		room = 0
	}
	used := 0
	end := 0
	for i, r := range s {
		w := RuneWidth(r)
		if used+w > room {
			break
		}
		used += w
		_, size := utf8.DecodeRuneInString(s[i:])
		end = i + size
	}
	// Trailing combining marks belong to the last kept rune; the loop above
	// already keeps them (width 0), so nothing further to attach.
	return s[:end] + ell
}
