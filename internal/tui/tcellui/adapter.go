// Package tcellui puts a tui.Buffer on a real terminal through tcell. The
// widget layer draws into a Buffer and never names a terminal; this package is
// the one place that knows about escape sequences, raw mode and input, all of
// which tcell already does well, so none of it is reimplemented here.
//
// The rest of the program sees only the narrow Screen interface, which keeps
// the terminal out of tests: a tcell simulation screen stands in for it.
package tcellui

import (
	"sync"
	"sync/atomic"

	"github.com/gdamore/tcell/v2"
	"github.com/urnetwork/connect/internal/tui"
)

// EventKind says what an Event carries.
type EventKind uint8

const (
	// EventKey is a named key (Key is set).
	EventKey EventKind = iota
	// EventRune is a printable key (Rune is set).
	EventRune
	// EventResize is a new terminal size (W and H are set).
	EventResize
	// EventQuit means the screen is gone (finalized or its input failed); no
	// further events follow.
	EventQuit
)

// Key names the few non-printable keys the screens use. Anything else that is
// not a printable rune arrives as KeyOther so a caller can ignore it.
type Key uint8

const (
	KeyOther Key = iota
	KeyEsc
	KeyTab
	KeyBacktab
	KeyEnter
	KeyCtrlC
)

// Event is the terminal input the screens understand.
type Event struct {
	Kind EventKind
	Key  Key
	Rune rune
	W, H int
}

// Screen is what a screen model needs from a terminal: its size, a way to show
// a finished Buffer, the input stream, and a way to give the terminal back.
type Screen interface {
	// Size returns the terminal size in cells.
	Size() (w, h int)
	// Draw shows b, which should be Size() big; anything outside the terminal
	// is clipped and anything b does not cover is blank.
	Draw(b *tui.Buffer)
	// Events is the input stream. It ends (after an EventQuit) once the screen
	// is closed.
	Events() <-chan Event
	// Close restores the terminal. It is safe to call more than once and from
	// any goroutine, including a panic path.
	Close()
}

type screen struct {
	s      tcell.Screen
	events chan Event
	done   chan struct{}
	once   sync.Once
	// resized is set by the input goroutine and consumed by Draw, so the
	// repaint that tcell wants after a resize happens on the drawing goroutine
	// rather than racing it.
	resized atomic.Bool
}

// Open takes over the controlling terminal with the alternate screen and raw
// input. It fails when there is no usable terminal.
func Open() (Screen, error) {
	s, err := tcell.NewScreen()
	if err != nil {
		return nil, err
	}
	return Wrap(s)
}

// Wrap initializes s and returns it as a Screen. It exists so tests can pass a
// tcell simulation screen.
func Wrap(s tcell.Screen) (Screen, error) {
	if err := s.Init(); err != nil {
		return nil, err
	}
	self := &screen{s: s, events: make(chan Event, 16), done: make(chan struct{})}
	go self.pump()
	return self, nil
}

func (self *screen) Size() (int, int) { return self.s.Size() }

func (self *screen) Events() <-chan Event { return self.events }

// Close finalizes the terminal exactly once. tcell's Fini unblocks PollEvent,
// which ends the pump.
func (self *screen) Close() {
	self.once.Do(func() {
		close(self.done)
		self.s.Fini()
	})
}

// Draw copies the buffer cell by cell. tcell keeps its own back buffer and
// sends only what changed, so there is no need to diff here.
func (self *screen) Draw(b *tui.Buffer) {
	if self.resized.Swap(false) {
		self.s.Sync()
	}
	self.s.Clear()
	w, h := self.s.Size()
	for y := 0; y < min(b.Height(), h); y++ {
		for x := 0; x < min(b.Width(), w); x++ {
			c := b.Cell(x, y)
			if c.Continuation() {
				continue // the wide rune before it already claimed this cell
			}
			var comb []rune
			if c.Comb != "" {
				comb = []rune(c.Comb)
			}
			self.s.SetContent(x, y, c.Rune, comb, Style(c.Style))
		}
	}
	self.s.Show()
}

// pump turns tcell events into Events until the screen is finalized. Sends
// give up when Close runs so a reader that has stopped reading cannot pin the
// goroutine.
func (self *screen) pump() {
	defer close(self.events)
	for {
		ev := self.s.PollEvent()
		if ev == nil {
			self.send(Event{Kind: EventQuit})
			return
		}
		out, ok := self.translate(ev)
		if !ok {
			continue
		}
		if !self.send(out) {
			return
		}
	}
}

func (self *screen) send(ev Event) bool {
	select {
	case self.events <- ev:
		return true
	case <-self.done:
		return false
	}
}

func (self *screen) translate(ev tcell.Event) (Event, bool) {
	switch ev := ev.(type) {
	case *tcell.EventResize:
		// The terminal changed under tcell's back buffer; the next Draw resyncs
		// it and repaints everything at the new size.
		self.resized.Store(true)
		w, h := self.s.Size()
		return Event{Kind: EventResize, W: w, H: h}, true
	case *tcell.EventKey:
		return translateKey(ev), true
	case *tcell.EventError:
		return Event{Kind: EventQuit}, true
	}
	return Event{}, false
}

func translateKey(ev *tcell.EventKey) Event {
	switch ev.Key() {
	case tcell.KeyRune:
		return Event{Kind: EventRune, Rune: ev.Rune()}
	case tcell.KeyEsc:
		return Event{Kind: EventKey, Key: KeyEsc}
	case tcell.KeyTab:
		return Event{Kind: EventKey, Key: KeyTab}
	case tcell.KeyBacktab:
		return Event{Kind: EventKey, Key: KeyBacktab}
	case tcell.KeyEnter:
		return Event{Kind: EventKey, Key: KeyEnter}
	case tcell.KeyCtrlC:
		return Event{Kind: EventKey, Key: KeyCtrlC}
	}
	return Event{Kind: EventKey, Key: KeyOther}
}

// Style converts a tui.Style to a tcell.Style. Colors are passed through at
// the richest form the widget asked for; tcell downgrades them to what the
// terminal supports. The zero Style is the terminal default, which is all the
// mono theme ever uses.
func Style(st tui.Style) tcell.Style {
	out := tcell.StyleDefault
	if c, ok := color(st.FG); ok {
		out = out.Foreground(c)
	}
	if c, ok := color(st.BG); ok {
		out = out.Background(c)
	}
	if st.Attrs&tui.AttrBold != 0 {
		out = out.Bold(true)
	}
	if st.Attrs&tui.AttrDim != 0 {
		out = out.Dim(true)
	}
	if st.Attrs&tui.AttrItalic != 0 {
		out = out.Italic(true)
	}
	if st.Attrs&tui.AttrUnderline != 0 {
		out = out.Underline(true)
	}
	if st.Attrs&tui.AttrReverse != 0 {
		out = out.Reverse(true)
	}
	return out
}

func color(c tui.Color) (tcell.Color, bool) {
	switch c.Kind {
	case tui.Color16, tui.Color256:
		return tcell.PaletteColor(int(c.Value)), true
	case tui.ColorRGB:
		r, g, b := c.Components()
		return tcell.NewRGBColor(int32(r), int32(g), int32(b)), true
	}
	return tcell.ColorDefault, false
}
