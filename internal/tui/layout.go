package tui

// Track is one row or column in a split: a fixed number of cells, or a
// weighted share of whatever the fixed tracks leave over. It is a tiny
// flexbox, enough to lay out the panels of a screen and nothing more.
type Track struct {
	fixed  int
	weight int
}

// Fixed is a track of exactly n cells (less if the space runs out).
func Fixed(n int) Track { return Track{fixed: max(n, 0)} }

// Flex is a track that takes a share of the leftover space in proportion to
// weight. A weight of zero or less takes nothing.
func Flex(weight int) Track { return Track{weight: max(weight, 0)} }

// split sizes the tracks along an axis of length total. Fixed tracks are served
// first, in order, so a screen that is too small starves the flexible panels
// before the fixed header and footer. The leftover is shared out by cumulative
// rounding, so the sizes always add up exactly with no drift onto the last
// track. Without any flexible track the leftover is simply unused.
func split(total int, tracks []Track) []int {
	sizes := make([]int, len(tracks))
	left := max(total, 0)
	weights := 0
	for i, t := range tracks {
		if t.weight > 0 {
			weights += t.weight
			continue
		}
		sizes[i] = min(t.fixed, left)
		left -= sizes[i]
	}
	if weights == 0 {
		return sizes
	}
	cum, prev := 0, 0
	for i, t := range tracks {
		if t.weight <= 0 {
			continue
		}
		cum += t.weight
		edge := left * cum / weights
		sizes[i] = edge - prev
		prev = edge
	}
	return sizes
}

// SplitRows cuts r into horizontal bands, top to bottom, one per track.
func SplitRows(r Rect, tracks ...Track) []Rect {
	out := make([]Rect, len(tracks))
	y := r.Y
	for i, h := range split(r.H, tracks) {
		out[i] = Rect{X: r.X, Y: y, W: max(r.W, 0), H: h}
		y += h
	}
	return out
}

// SplitCols cuts r into vertical bands, left to right, one per track.
func SplitCols(r Rect, tracks ...Track) []Rect {
	out := make([]Rect, len(tracks))
	x := r.X
	for i, w := range split(r.W, tracks) {
		out[i] = Rect{X: x, Y: r.Y, W: w, H: max(r.H, 0)}
		x += w
	}
	return out
}

// MinSize is the smallest terminal a full layout is designed for. A caller
// checks Fits before laying out and draws a compact single-panel view instead
// when it is false, so a small window degrades to something readable rather
// than a screen of clipped borders.
type MinSize struct {
	W, H int
}

// Fits reports whether a w x h area is at least the minimum in both axes.
func (self MinSize) Fits(w, h int) bool { return w >= self.W && h >= self.H }

// FitsRect is Fits for a rectangle.
func (self MinSize) FitsRect(r Rect) bool { return self.Fits(r.W, r.H) }
