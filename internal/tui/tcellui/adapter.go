// Package tcellui puts a tui.Buffer on a real terminal through tcell. The
// widget layer draws into a Buffer and never names a terminal; this package is
// the one place that knows about escape sequences, raw mode and input, all of
// which tcell already does well, so none of it is reimplemented here.
//
// The rest of the program sees only the narrow Screen interface, which keeps
// the terminal out of tests: a tcell simulation screen stands in for it.
package tcellui

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

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
	// EventMouse is a mouse press, release, drag, move or wheel step.
	EventMouse
	// EventQuit means the screen is gone (finalized or its input failed); no
	// further events follow.
	EventQuit
)

// MouseButton names a mouse button or wheel direction.
type MouseButton uint8

const (
	MouseNone MouseButton = iota
	MouseLeft
	MouseRight
	MouseMiddle
	MouseWheelUp
	MouseWheelDown
	MouseWheelLeft
	MouseWheelRight
)

// MouseAction says what the mouse did at (X, Y).
type MouseAction uint8

const (
	MouseMove MouseAction = iota
	MousePress
	MouseDrag
	MouseRelease
	MouseWheel
)

// ModMask is a bitmask of keyboard modifiers held during an input event.
type ModMask uint8

const (
	ModShift ModMask = 1 << iota
	ModCtrl
	ModAlt
	ModMeta
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
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyPageUp
	KeyPageDown
)

// Event is the terminal input the screens understand.
type Event struct {
	Kind      EventKind
	Key       Key
	Rune      rune
	W, H      int
	X, Y      int
	Button    MouseButton
	Action    MouseAction
	Modifiers ModMask
}

// Screen is what a screen model needs from a terminal: its size, a way to show
// a finished Buffer, the input stream, and a way to give the terminal back.
type Screen interface {
	// Size returns the terminal size in cells.
	Size() (w, h int)
	// Draw shows b, which should be Size() big; anything outside the terminal
	// is clipped and anything b does not cover is blank.
	Draw(b *tui.Buffer)
	// SetMouse turns terminal mouse reporting on or off. Turning it off lets
	// the terminal handle text selection directly without holding Shift.
	SetMouse(on bool)
	// Suspend gives the terminal back: it leaves the alternate screen, turns
	// mouse reporting off, shows the cursor and restores the terminal to
	// cooked mode, so a command started afterwards can prompt on it. Draw does
	// nothing until Resume. A second Suspend without a Resume changes nothing.
	Suspend() error
	// Resume takes the terminal over again after Suspend. The mouse setting
	// that was in force is applied again, and the next Draw repaints the whole
	// screen. A Resume with nothing suspended changes nothing. A Resume that
	// fails leaves the screen suspended, so Draw stays off the terminal and a
	// later Resume tries again.
	Resume() error
	// Events is the input stream. It ends (after an EventQuit) once the screen
	// is closed.
	Events() <-chan Event
	// WriteTerminal writes p to the terminal outside the frame buffer, for an
	// escape sequence that is not screen content such as an OSC 52 clipboard
	// set. It takes the same lock as Draw and resyncs the screen afterwards, so
	// a sequence written here never leaves the renderer's cursor or cells out
	// of step.
	WriteTerminal(p []byte) error
	// Close restores the terminal. It is safe to call more than once and from
	// any goroutine, including a panic path.
	Close()
}

type suspendDiscard struct{ ch chan struct{} }
type suspendBarrier struct{ ack chan struct{} }

func (*suspendBarrier) When() time.Time { return time.Time{} }

type keychanBarrier struct {
	token   []byte
	ack     chan struct{}
	ackOnce sync.Once
}

// keychanSentinelBytes builds an OSC 52 clipboard sequence preceded by two ST
// terminators so tcell's InputProcessor returns to its initial state from any
// partial escape or UTF-8 sequence before parsing the sentinel. Because
// keychan, mainLoop and eventQ are FIFO, receiving the resulting EventClipboard
// proves every prior raw chunk has left keychan and eventQ.
func keychanSentinelBytes(token []byte) []byte {
	enc := base64.StdEncoding.EncodeToString(token)
	out := make([]byte, 0, len(enc)+13)
	out = append(out, '\x1b', '\\', '\x1b', '\\', '\x1b', ']', '5', '2', ';', 'c', ';')
	out = append(out, enc...)
	out = append(out, '\x1b', '\\')
	return out
}

// suspendTty wraps tcell's Tty so Suspend and Close can inject a sentinel into
// keychan and park inputLoop inside Read before tcell's disengage closes stopQ.
// In tcell v2.13.10, inputLoop sends each read chunk with an unconditional
// t.keychan <- chunk[:n] (capacity 10), while mainLoop selects between <-stopQ
// and <-t.keychan. If keychan has buffered chunks when stopQ closes, mainLoop
// can exit on <-stopQ while inputLoop blocks forever on t.keychan <- chunk[:n],
// hanging t.wg.Wait() in disengage, or leave stale chunks in keychan for Resume.
type suspendTty struct {
	tcell.Tty
	mu       sync.Mutex
	cond     *sync.Cond
	gating   bool
	inRead   bool
	entered  bool
	parked   bool
	drained  bool
	stopped  bool
	sentinel []byte
}

func wrapSuspendTty(tty tcell.Tty) *suspendTty {
	st := &suspendTty{Tty: tty}
	st.cond = sync.NewCond(&st.mu)
	return st
}

func (st *suspendTty) Start() error {
	st.mu.Lock()
	st.gating = false
	st.inRead = false
	st.entered = false
	st.parked = false
	st.drained = false
	st.stopped = false
	st.sentinel = nil
	st.cond.Broadcast()
	st.mu.Unlock()
	return st.Tty.Start()
}

func (st *suspendTty) Stop() error {
	st.mu.Lock()
	st.gating = false
	st.inRead = false
	st.entered = false
	st.parked = false
	st.stopped = true
	st.sentinel = nil
	st.cond.Broadcast()
	st.mu.Unlock()
	return st.Tty.Stop()
}

func (st *suspendTty) Drain() error {
	st.mu.Lock()
	st.stopped = true
	needDrain := !st.drained
	st.drained = true
	st.cond.Broadcast()
	st.mu.Unlock()
	if needDrain {
		return st.Tty.Drain()
	}
	return nil
}

func (st *suspendTty) Read(p []byte) (int, error) {
	st.mu.Lock()
	st.entered = true
	st.inRead = true
	st.cond.Broadcast()
	if st.gating {
		if len(st.sentinel) > 0 && !st.stopped {
			n := copy(p, st.sentinel)
			st.sentinel = nil
			st.inRead = false
			st.cond.Broadcast()
			st.mu.Unlock()
			return n, nil
		}
		st.parked = true
		st.cond.Broadcast()
		for st.gating && !st.stopped {
			st.cond.Wait()
		}
		st.parked = false
		if st.stopped {
			st.inRead = false
			st.cond.Broadcast()
			st.mu.Unlock()
			return 0, io.EOF
		}
	}
	if st.stopped {
		st.inRead = false
		st.cond.Broadcast()
		st.mu.Unlock()
		return 0, io.EOF
	}
	st.mu.Unlock()

	n, err := st.Tty.Read(p)

	st.mu.Lock()
	if st.gating {
		if len(st.sentinel) > 0 && !st.stopped {
			n = copy(p, st.sentinel)
			st.sentinel = nil
			st.inRead = false
			st.cond.Broadcast()
			st.mu.Unlock()
			return n, nil
		}
		if err != nil && !st.drained {
			st.stopped = true
		}
		st.parked = true
		st.cond.Broadcast()
		for st.gating && !st.stopped {
			st.cond.Wait()
		}
		st.parked = false
		stopped := st.stopped
		st.inRead = false
		st.cond.Broadcast()
		st.mu.Unlock()
		if stopped {
			return 0, io.EOF
		}
		return n, err
	}
	if err != nil {
		st.stopped = true
	}
	stopped := st.stopped
	st.inRead = false
	st.cond.Broadcast()
	st.mu.Unlock()

	if stopped && err == nil {
		return 0, io.EOF
	}
	return n, err
}

func (st *suspendTty) beginSuspend(sentinel []byte) bool {
	st.mu.Lock()
	st.gating = true
	if !st.entered || st.stopped {
		st.mu.Unlock()
		return false
	}
	st.sentinel = sentinel
	needDrain := st.inRead && !st.drained
	if needDrain {
		st.drained = true
	}
	st.mu.Unlock()

	if needDrain {
		_ = st.Tty.Drain()
	}

	st.mu.Lock()
	for !st.parked && !st.stopped {
		st.cond.Wait()
	}
	sent := st.sentinel == nil
	st.mu.Unlock()
	return sent
}

func (st *suspendTty) endSuspend() {
	st.mu.Lock()
	st.gating = false
	st.cond.Broadcast()
	st.mu.Unlock()
}

type screen struct {
	discard       atomic.Pointer[suspendDiscard]
	keyBarrier    atomic.Pointer[keychanBarrier]
	keyBarrierSeq atomic.Uint64
	pumpDone      chan struct{}
	finiDone      chan struct{}
	closing       atomic.Bool
	s             tcell.Screen
	tty           *suspendTty
	events        chan Event
	done          chan struct{}
	once          sync.Once
	// raw is where a sequence outside the frame is written: the terminal the
	// screen was opened on. It is nil for a wrapped simulation screen, which
	// has no terminal to write to.
	raw io.Writer
	// mu serializes Draw, SetMouse and Close. tcell's screen state is not
	// goroutine-safe: a Close (Fini) racing an in-flight Draw (Show/resize)
	// is a data race (caught on TestTopLoopEndsWhenTheScreenGoesAway), so the
	// drawing loop and the teardown must never touch tcell concurrently.
	mu sync.Mutex
	// resized is set by the input goroutine and consumed by Draw, so the
	// repaint that tcell wants after a resize happens on the drawing goroutine
	// rather than racing it.
	resized atomic.Bool
	// Mouse mode changes are published to the pump without writing held from
	// the rendering goroutine. Repeated SetMouse calls do not break a drag.
	mouseDisabled  bool // protected by mu
	mouseEpoch     atomic.Uint64
	seenMouseEpoch uint64 // pump goroutine only
	// held is the button currently pressed, tracked on the pump goroutine so
	// motion with a button held translates as MouseDrag and button release as
	// MouseRelease.
	held MouseButton
	// suspended is set while a child owns the terminal (for example a sudo
	// password prompt). Draw is ignored until Resume clears it again.
	suspended bool
}

// Open takes over the controlling terminal with the alternate screen and raw
// input. It fails when there is no usable terminal.
func Open() (Screen, error) {
	var st *suspendTty
	var s tcell.Screen
	var err error
	if tty, ttyErr := tcell.NewDevTty(); ttyErr == nil {
		st = wrapSuspendTty(tty)
		s, err = tcell.NewTerminfoScreenFromTty(st)
		if err != nil {
			_ = tty.Close()
		}
	} else {
		s, err = tcell.NewScreen()
	}
	if err != nil {
		return nil, err
	}
	scr, err := Wrap(s)
	if err != nil {
		if st != nil {
			_ = st.Close()
		}
		return nil, err
	}
	if st != nil {
		scr.(*screen).tty = st
	}
	scr.(*screen).raw = os.Stdout
	return scr, nil
}

// Wrap initializes s and returns it as a Screen. It exists so tests can pass a
// tcell simulation screen.
func Wrap(s tcell.Screen) (Screen, error) {
	if err := s.Init(); err != nil {
		return nil, err
	}
	s.EnableMouse()
	self := &screen{s: s, events: make(chan Event, 16), done: make(chan struct{}), pumpDone: make(chan struct{}), finiDone: make(chan struct{})}
	if tty, ok := s.Tty(); ok {
		if st, ok := tty.(*suspendTty); ok {
			self.tty = st
		}
	}
	self.discard.Store(&suspendDiscard{ch: make(chan struct{})})
	go self.pump()
	return self, nil
}

func (self *screen) Size() (int, int) { return self.s.Size() }

func (self *screen) Events() <-chan Event { return self.events }

func (self *screen) SetMouse(on bool) {
	self.mu.Lock()
	defer self.mu.Unlock()
	if self.mouseDisabled == on {
		self.mouseDisabled = !on
		self.mouseEpoch.Add(1)
	}
	if self.suspended {
		// The shell owns the terminal. The flag above is what Resume applies,
		// so an escape sequence is never sent to a terminal mid-prompt.
		return
	}
	if on {
		self.s.EnableMouse()
	} else {
		self.s.DisableMouse()
	}
}

// Suspend hands the terminal to the shell: mouse reporting off, cursor shown,
// cooked mode, and tcell disengaged from the tty. Draw is ignored until Resume
// so a frame written while a child owns the terminal is never sent.
func (self *screen) Suspend() error {
	self.mu.Lock()
	defer self.mu.Unlock()
	select {
	case <-self.done:
		return errors.New("the screen is closed")
	default:
	}
	if self.suspended {
		return nil
	}
	// Keep PollEvent moving while tcell stops its input goroutine.
	close(self.discard.Load().ch)
	if self.tty != nil {
		self.drainBeforeDisengage()
		defer self.tty.endSuspend()
	}
	err := self.s.Suspend()
	// The marker follows all input queued before suspension. Once the pump
	// reaches it, discard the adapter queue before allowing events again.
	barrier := &suspendBarrier{ack: make(chan struct{})}
	self.s.PostEventWait(barrier)
	select {
	case <-barrier.ack:
	case <-self.pumpDone:
	}
	for {
		select {
		case _, ok := <-self.events:
			if !ok {
				self.discard.Store(&suspendDiscard{ch: make(chan struct{})})
				return err
			}
		default:
			self.discard.Store(&suspendDiscard{ch: make(chan struct{})})
			if err != nil {
				return err
			}
			self.suspended = true
			return nil
		}
	}
}

// Resume takes the terminal back and reapplies the mouse setting that was in
// force, so a session that was in Keyboard Mode stays in it. A failed resume
// leaves the screen suspended: the terminal never came back, so Draw keeps the
// frames off it and a later Resume tries again.
func (self *screen) Resume() error {
	self.mu.Lock()
	defer self.mu.Unlock()
	select {
	case <-self.done:
		return errors.New("the screen is closed")
	default:
	}
	if !self.suspended {
		return nil
	}
	if err := self.s.Resume(); err != nil {
		return err
	}
	self.suspended = false
	if self.mouseDisabled {
		self.s.DisableMouse()
	} else {
		self.s.EnableMouse()
	}
	self.resized.Store(true)
	// The mouse epoch moves so the pump re-reads the setting: a suspension can
	// change what the terminal reports even when top's own mode did not change.
	self.mouseEpoch.Add(1)
	return nil
}

// drainBeforeDisengage injects an OSC 52 sentinel chunk at the tail of tcell's
// keychan, parks inputLoop inside Read so no further chunks can be queued, and
// waits until pump observes the sentinel EventClipboard. Because keychan,
// mainLoop and eventQ are FIFO, once the sentinel arrives keychan is empty and
// disengage cannot deadlock or leave pre-suspend chunks behind for Resume.
func (self *screen) drainBeforeDisengage() {
	token := []byte("ur-drain-" + strconv.FormatUint(self.keyBarrierSeq.Add(1), 10))
	kb := &keychanBarrier{token: token, ack: make(chan struct{})}
	self.keyBarrier.Store(kb)
	defer self.keyBarrier.Store(nil)

	if !self.tty.beginSuspend(keychanSentinelBytes(token)) {
		return
	}
	select {
	case <-kb.ack:
	case <-self.pumpDone:
	}
}

// Close finalizes the terminal exactly once. tcell's Fini unblocks PollEvent,
// which ends the pump.
func (self *screen) Close() {
	self.once.Do(func() {
		self.mu.Lock()
		defer self.mu.Unlock()
		if self.tty != nil && !self.suspended {
			close(self.discard.Load().ch)
			self.drainBeforeDisengage()
			defer self.tty.endSuspend()
		}
		self.closing.Store(true)
		self.s.Fini()
		close(self.finiDone)
		close(self.done)
	})
}

// WriteTerminal writes p to the terminal under the drawing lock and resyncs the
// screen, so a sequence that is not frame content leaves no cell behind it out
// of date. A wrapped simulation screen has no terminal to write to and says so.
func (self *screen) WriteTerminal(p []byte) error {
	self.mu.Lock()
	defer self.mu.Unlock()
	select {
	case <-self.done:
		return errors.New("the screen is closed")
	default:
	}
	if self.raw == nil {
		return errors.New("no terminal to write to")
	}
	if _, err := self.raw.Write(p); err != nil {
		return err
	}
	self.s.Sync()
	return nil
}

// Draw copies the buffer cell by cell. tcell keeps its own back buffer and
// sends only what changed, so there is no need to diff here.
func (self *screen) Draw(b *tui.Buffer) {
	self.mu.Lock()
	defer self.mu.Unlock()
	// While the terminal is borrowed (for example a sudo password prompt is
	// up) the shell owns it, so the frame must not reach it. A panicked
	// handler that resumes afterwards still gets a fresh draw.
	if self.suspended {
		return
	}
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
	defer close(self.pumpDone)
	defer close(self.events)
	for {
		ev := self.s.PollEvent()
		if ev == nil {
			if self.closing.Load() {
				select {
				case <-self.finiDone:
				default:
					continue
				}
			}
			self.send(Event{Kind: EventQuit})
			return
		}
		if barrier, ok := ev.(*suspendBarrier); ok {
			close(barrier.ack)
			continue
		}
		if clip, ok := ev.(*tcell.EventClipboard); ok {
			if kb := self.keyBarrier.Load(); kb != nil && bytes.Equal(clip.Data(), kb.token) {
				kb.ackOnce.Do(func() { close(kb.ack) })
				continue
			}
		}
		out, ok := self.translate(ev)
		if !ok {
			continue
		}
		if self.closing.Load() {
			// Close is finalizing the screen and its caller is usually the
			// reader, so a full queue would never drain. Keep taking events so
			// tcell's parser can finish, and drop them.
			continue
		}
		if !self.send(out) {
			return
		}
	}
}

func (self *screen) send(ev Event) bool {
	var discard <-chan struct{}
	if d := self.discard.Load(); d != nil {
		discard = d.ch
	}
	select {
	case <-discard:
		return true
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
	case *tcell.EventMouse:
		return self.translateMouse(ev)
	case *tcell.EventError:
		return Event{Kind: EventQuit}, true
	}
	return Event{}, false
}

func translateMods(m tcell.ModMask) ModMask {
	var out ModMask
	if m&tcell.ModShift != 0 {
		out |= ModShift
	}
	if m&tcell.ModCtrl != 0 {
		out |= ModCtrl
	}
	if m&tcell.ModAlt != 0 {
		out |= ModAlt
	}
	if m&tcell.ModMeta != 0 {
		out |= ModMeta
	}
	return out
}

func (self *screen) translateMouse(ev *tcell.EventMouse) (Event, bool) {
	if epoch := self.mouseEpoch.Load(); epoch != self.seenMouseEpoch {
		self.held = MouseNone
		self.seenMouseEpoch = epoch
	}
	x, y := ev.Position()
	mods := translateMods(ev.Modifiers())
	btns := ev.Buttons()
	switch {
	case btns&tcell.WheelUp != 0:
		return Event{Kind: EventMouse, X: x, Y: y, Button: MouseWheelUp, Action: MouseWheel, Modifiers: mods}, true
	case btns&tcell.WheelDown != 0:
		return Event{Kind: EventMouse, X: x, Y: y, Button: MouseWheelDown, Action: MouseWheel, Modifiers: mods}, true
	case btns&tcell.WheelLeft != 0:
		return Event{Kind: EventMouse, X: x, Y: y, Button: MouseWheelLeft, Action: MouseWheel, Modifiers: mods}, true
	case btns&tcell.WheelRight != 0:
		return Event{Kind: EventMouse, X: x, Y: y, Button: MouseWheelRight, Action: MouseWheel, Modifiers: mods}, true
	}
	var btn MouseButton
	switch {
	case btns&tcell.Button1 != 0:
		btn = MouseLeft
	case btns&tcell.Button2 != 0:
		btn = MouseRight
	case btns&tcell.Button3 != 0:
		btn = MouseMiddle
	case btns == tcell.ButtonNone:
		btn = MouseNone
	default:
		return Event{}, false
	}
	if btn != MouseNone {
		action := MousePress
		if self.held == btn {
			action = MouseDrag
		}
		self.held = btn
		return Event{Kind: EventMouse, X: x, Y: y, Button: btn, Action: action, Modifiers: mods}, true
	}
	if self.held != MouseNone {
		rel := self.held
		self.held = MouseNone
		return Event{Kind: EventMouse, X: x, Y: y, Button: rel, Action: MouseRelease, Modifiers: mods}, true
	}
	return Event{Kind: EventMouse, X: x, Y: y, Button: MouseNone, Action: MouseMove, Modifiers: mods}, true
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
	case tcell.KeyUp:
		return Event{Kind: EventKey, Key: KeyUp}
	case tcell.KeyDown:
		return Event{Kind: EventKey, Key: KeyDown}
	case tcell.KeyLeft:
		return Event{Kind: EventKey, Key: KeyLeft}
	case tcell.KeyRight:
		return Event{Kind: EventKey, Key: KeyRight}
	case tcell.KeyPgUp:
		return Event{Kind: EventKey, Key: KeyPageUp}
	case tcell.KeyPgDn:
		return Event{Kind: EventKey, Key: KeyPageDown}
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
