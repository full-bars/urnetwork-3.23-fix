package tui

// Rect is a rectangle of cells. Width and height are never negative once a
// Rect has passed through Intersect or the layout helpers.
type Rect struct {
	X, Y, W, H int
}

// Empty reports a rectangle with no cells.
func (self Rect) Empty() bool { return self.W <= 0 || self.H <= 0 }

// Intersect returns the overlap of two rectangles, or the zero Rect.
func (self Rect) Intersect(o Rect) Rect {
	x0, y0 := max(self.X, o.X), max(self.Y, o.Y)
	x1, y1 := min(self.X+self.W, o.X+o.W), min(self.Y+self.H, o.Y+o.H)
	if x1 <= x0 || y1 <= y0 {
		return Rect{}
	}
	return Rect{X: x0, Y: y0, W: x1 - x0, H: y1 - y0}
}

// Inset shrinks the rectangle by n cells on every side, bottoming out at empty.
func (self Rect) Inset(n int) Rect {
	r := Rect{X: self.X + n, Y: self.Y + n, W: self.W - 2*n, H: self.H - 2*n}
	if r.Empty() {
		return Rect{}
	}
	return r
}
