package tui

import "strings"

// Cell is one terminal cell. A wide rune occupies two: the first holds the
// rune and the second is a continuation cell (Rune 0) that the writer skips.
// Comb holds any combining marks that follow the rune.
type Cell struct {
	Rune  rune
	Comb  string
	Style Style
}

// Continuation reports the trailing half of a wide rune.
func (self Cell) Continuation() bool { return self.Rune == 0 }

func blankCell(st Style) Cell { return Cell{Rune: ' ', Style: st} }

// Buffer is a fixed grid of cells that widgets draw into. The terminal
// backend is not part of this package: a backend reads the finished Buffer
// (through Cell or Diff) and turns it into escape sequences, so the same
// widgets work under any backend and can be golden-tested as plain text.
//
// A Buffer made by New owns its storage. Sub returns a view that shares the
// parent's storage, uses coordinates relative to its own top-left corner, and
// clips every write to its own bounds; that is how a panel draws into its
// rectangle without being able to spill into a neighbour. Buffers are not
// safe for concurrent use.
type Buffer struct {
	cells  []Cell
	stride int // row length of the backing storage
	ox, oy int // origin of this view inside the backing storage
	w, h   int
	root   bool
}

// New returns a w x h buffer of blank cells. Negative sizes become zero.
func New(w, h int) *Buffer {
	w, h = max(w, 0), max(h, 0)
	self := &Buffer{cells: make([]Cell, w*h), stride: w, w: w, h: h, root: true}
	for i := range self.cells {
		self.cells[i] = blankCell(Style{})
	}
	return self
}

// Width and Height return the view size in cells.
func (self *Buffer) Width() int  { return self.w }
func (self *Buffer) Height() int { return self.h }

// Rect returns the view's own bounds, anchored at the origin.
func (self *Buffer) Rect() Rect { return Rect{W: self.w, H: self.h} }

func (self *Buffer) index(x, y int) int { return (self.oy+y)*self.stride + self.ox + x }

func (self *Buffer) inView(x, y int) bool { return x >= 0 && y >= 0 && x < self.w && y < self.h }

// Cell returns the cell at (x, y), or a blank cell when out of the view.
func (self *Buffer) Cell(x, y int) Cell {
	if !self.inView(x, y) {
		return blankCell(Style{})
	}
	return self.cells[self.index(x, y)]
}

// releaseWide blanks the other half of a wide rune that a write at (x, y) is
// about to break. It works on backing coordinates, not view coordinates: a
// wide rune drawn by a parent can straddle the edge of a child view, and the
// child overwriting its first or last column must still repair it.
func (self *Buffer) releaseWide(x, y int) {
	ax := self.ox + x
	row := (self.oy + y) * self.stride
	c := self.cells[row+ax]
	if c.Continuation() && ax > 0 {
		self.cells[row+ax-1] = blankCell(self.cells[row+ax-1].Style)
	}
	if ax+1 < self.stride && self.cells[row+ax+1].Continuation() && RuneWidth(c.Rune) == 2 {
		self.cells[row+ax+1] = blankCell(self.cells[row+ax+1].Style)
	}
}

// Set stores one cell, clipped to the view. A wide rune claims the next cell
// as well; if that cell is outside the view the rune is replaced by a blank
// so a half glyph is never emitted. Zero-width and control runes are ignored
// (Put attaches combining marks).
func (self *Buffer) Set(x, y int, c Cell) {
	if !self.inView(x, y) {
		return
	}
	switch RuneWidth(c.Rune) {
	case 0:
		return
	case 2:
		if !self.inView(x+1, y) {
			c = blankCell(c.Style)
			break
		}
		self.releaseWide(x, y)
		self.releaseWide(x+1, y)
		self.cells[self.index(x, y)] = c
		self.cells[self.index(x+1, y)] = Cell{Style: c.Style}
		return
	}
	self.releaseWide(x, y)
	self.cells[self.index(x, y)] = c
}

// Put draws text starting at (x, y) in one style and returns the x just past
// the last cell it advanced, even when that is beyond the view, so callers can
// lay out a run of segments without measuring separately. Text is clipped at
// both edges, wide runes never draw a half glyph, combining marks join the
// preceding cell, and control characters are dropped.
func (self *Buffer) Put(text string, x, y int, st Style) int {
	cx := x
	last := -1 // column of the last cell drawn, target of combining marks
	for _, r := range text {
		if isControl(r) {
			continue
		}
		switch w := RuneWidth(r); w {
		case 0:
			if last >= 0 {
				i := self.index(last, y)
				self.cells[i].Comb += string(r)
			}
		case 1:
			if self.inView(cx, y) {
				self.Set(cx, y, Cell{Rune: r, Style: st})
				last = cx
			} else {
				last = -1
			}
			cx++
		default:
			last = -1
			switch {
			case !self.inView(cx, y) && !self.inView(cx+1, y):
			case self.inView(cx, y) && self.inView(cx+1, y):
				self.Set(cx, y, Cell{Rune: r, Style: st})
				last = cx
			default:
				// Half the glyph is clipped: blank the visible half.
				self.Set(max(cx, 0), y, blankCell(st))
			}
			cx += 2
		}
	}
	return cx
}

// Fill sets every cell of r (relative to the view, clipped) to ch. A ch that
// is not exactly one cell wide is replaced by a space.
func (self *Buffer) Fill(r Rect, ch rune, st Style) {
	if RuneWidth(ch) != 1 {
		ch = ' '
	}
	r = r.Intersect(self.Rect())
	for y := r.Y; y < r.Y+r.H; y++ {
		for x := r.X; x < r.X+r.W; x++ {
			self.Set(x, y, Cell{Rune: ch, Style: st})
		}
	}
}

// Sub returns a clipped view onto r. The view shares storage with the parent.
// A rectangle outside the parent gives an empty view.
func (self *Buffer) Sub(r Rect) *Buffer {
	r = r.Intersect(self.Rect())
	return &Buffer{cells: self.cells, stride: self.stride, ox: self.ox + r.X, oy: self.oy + r.Y, w: r.W, h: r.H}
}

// Resize changes the size of a buffer that owns its storage, keeping the
// overlapping top-left content and blanking the rest. A wide rune cut in half
// by the new right edge is blanked. It panics on a view, since resizing shared
// storage would invalidate the parent and its siblings.
func (self *Buffer) Resize(w, h int) {
	if !self.root {
		panic("tui: Resize on a Sub view")
	}
	w, h = max(w, 0), max(h, 0)
	if w == self.w && h == self.h {
		return
	}
	next := New(w, h)
	for y := 0; y < min(h, self.h); y++ {
		for x := 0; x < min(w, self.w); x++ {
			next.cells[y*w+x] = self.cells[y*self.stride+x]
		}
		if w < self.w && w > 0 && self.cells[y*self.stride+w].Continuation() {
			next.cells[y*w+w-1] = blankCell(next.cells[y*w+w-1].Style)
		}
	}
	self.cells, self.stride, self.w, self.h = next.cells, w, w, h
}

// Clone returns an independent root buffer holding a copy of this view. The
// backend keeps a clone of the last frame it wrote to feed Diff.
func (self *Buffer) Clone() *Buffer {
	out := New(self.w, self.h)
	for y := 0; y < self.h; y++ {
		copy(out.cells[y*self.w:(y+1)*self.w], self.cells[self.index(0, y):self.index(0, y)+self.w])
		// A wide rune whose right half falls outside the view edge leaves
		// no continuation in the clone: the left half would render as a
		// dangling widow. Blank it like Resize does.
		if self.w < self.stride && self.w > 0 && self.cells[self.index(self.w, y)].Continuation() {
			out.cells[y*self.w+self.w-1] = blankCell(out.cells[y*self.w+self.w-1].Style)
		}
	}
	return out
}

// String renders the view as plain text, one line per row, with no escape
// sequences, continuation cells skipped and trailing spaces trimmed so golden
// files are stable under editors that strip them. Styles are not represented.
func (self *Buffer) String() string {
	var b strings.Builder
	for y := 0; y < self.h; y++ {
		var row strings.Builder
		for x := 0; x < self.w; x++ {
			c := self.cells[self.index(x, y)]
			if c.Continuation() {
				continue
			}
			row.WriteRune(c.Rune)
			row.WriteString(c.Comb)
		}
		if y > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(strings.TrimRight(row.String(), " "))
	}
	return b.String()
}
