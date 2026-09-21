package tcellui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/urnetwork/connect/internal/tui"
)

func newSim(t *testing.T, w, h int) (Screen, tcell.SimulationScreen) {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	scr, err := Wrap(sim)
	if err != nil {
		t.Fatal(err)
	}
	sim.SetSize(w, h)
	t.Cleanup(scr.Close)
	return scr, sim
}

func nextEvent(t *testing.T, scr Screen) Event {
	t.Helper()
	select {
	case ev, ok := <-scr.Events():
		if !ok {
			t.Fatal("event channel closed")
		}
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	return Event{}
}

func TestStyleConversion(t *testing.T) {
	cases := []struct {
		name string
		in   tui.Style
		want tcell.Style
	}{
		{"zero is the terminal default", tui.Style{}, tcell.StyleDefault},
		{"16 color", tui.Style{FG: tui.Ansi(2)}, tcell.StyleDefault.Foreground(tcell.PaletteColor(2))},
		{"bright 16 color", tui.Style{FG: tui.Ansi(8)}, tcell.StyleDefault.Foreground(tcell.PaletteColor(8))},
		{"256 color", tui.Style{BG: tui.Xterm(200)}, tcell.StyleDefault.Background(tcell.PaletteColor(200))},
		{"rgb", tui.Style{FG: tui.RGB(1, 2, 3)}, tcell.StyleDefault.Foreground(tcell.NewRGBColor(1, 2, 3))},
		{"attrs", tui.Style{}.With(tui.AttrBold | tui.AttrDim | tui.AttrItalic | tui.AttrUnderline | tui.AttrReverse),
			tcell.StyleDefault.Bold(true).Dim(true).Italic(true).Underline(true).Reverse(true)},
	}
	for _, c := range cases {
		if got := Style(c.in); got != c.want {
			t.Errorf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
}

func TestDrawPaintsCellsAndStyles(t *testing.T) {
	scr, sim := newSim(t, 10, 3)
	b := tui.New(10, 3)
	b.Put("hi", 0, 0, tui.Style{FG: tui.Ansi(1)})
	b.Put("世", 2, 0, tui.Style{})
	b.Put("é", 5, 1, tui.Style{}) // e plus a combining acute accent
	b.Put("é", 5, 1, tui.Style{})
	scr.Draw(b)

	cells, w, _ := sim.GetContents()
	if w != 10 {
		t.Fatalf("width = %d", w)
	}
	if got := string(cells[0].Runes); got != "h" {
		t.Fatalf("cell 0 = %q", got)
	}
	if fg, _, _ := cells[0].Style.Decompose(); fg != tcell.PaletteColor(1) {
		t.Fatalf("cell 0 fg = %v", fg)
	}
	if got := string(cells[2].Runes); got != "世" {
		t.Fatalf("wide cell = %q", got)
	}
	if got := string(cells[w+5].Runes); got != "é" {
		t.Fatalf("combining cell = %q", got)
	}
}

func TestDrawClipsABufferLargerThanTheScreen(t *testing.T) {
	scr, _ := newSim(t, 4, 2)
	b := tui.New(9, 5)
	b.Put("abcdefghi", 0, 0, tui.Style{})
	scr.Draw(b) // must not panic
}

func TestSizeFollowsTheTerminal(t *testing.T) {
	scr, sim := newSim(t, 30, 9)
	if w, h := scr.Size(); w != 30 || h != 9 {
		t.Fatalf("Size = %dx%d", w, h)
	}
	sim.SetSize(50, 12)
	if err := sim.PostEvent(tcell.NewEventResize(50, 12)); err != nil {
		t.Fatal(err)
	}
	ev := nextEvent(t, scr)
	if ev.Kind != EventResize || ev.W != 50 || ev.H != 12 {
		t.Fatalf("event = %+v", ev)
	}
	if w, h := scr.Size(); w != 50 || h != 12 {
		t.Fatalf("Size after resize = %dx%d", w, h)
	}
}

func TestKeyTranslation(t *testing.T) {
	scr, sim := newSim(t, 10, 3)
	cases := []struct {
		key  tcell.Key
		r    rune
		mod  tcell.ModMask
		want Event
	}{
		{tcell.KeyRune, 'q', 0, Event{Kind: EventRune, Rune: 'q'}},
		{tcell.KeyRune, '+', 0, Event{Kind: EventRune, Rune: '+'}},
		{tcell.KeyRune, '?', 0, Event{Kind: EventRune, Rune: '?'}},
		{tcell.KeyEsc, 0, 0, Event{Kind: EventKey, Key: KeyEsc}},
		{tcell.KeyTab, 0, 0, Event{Kind: EventKey, Key: KeyTab}},
		{tcell.KeyBacktab, 0, 0, Event{Kind: EventKey, Key: KeyBacktab}},
		{tcell.KeyEnter, 0, 0, Event{Kind: EventKey, Key: KeyEnter}},
		{tcell.KeyCtrlC, 0, 0, Event{Kind: EventKey, Key: KeyCtrlC}},
		{tcell.KeyF5, 0, 0, Event{Kind: EventKey, Key: KeyOther}},
	}
	for _, c := range cases {
		sim.InjectKey(c.key, c.r, c.mod)
		if got := nextEvent(t, scr); got != c.want {
			t.Errorf("key %v: got %+v want %+v", c.key, got, c.want)
		}
	}
}

func TestShiftTabIsBacktab(t *testing.T) {
	scr, sim := newSim(t, 10, 3)
	sim.InjectKey(tcell.KeyTab, 0, tcell.ModShift)
	if got := nextEvent(t, scr); got.Key != KeyBacktab {
		t.Fatalf("got %+v", got)
	}
}

// Close is called from a deferred cleanup and from the panic path, so it must
// tolerate being called twice, and it must end the event stream so a reader
// never blocks on a dead screen.
func TestCloseIsIdempotentAndEndsTheEventStream(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	scr, err := Wrap(sim)
	if err != nil {
		t.Fatal(err)
	}
	scr.Close()
	scr.Close()
	select {
	case _, ok := <-scr.Events():
		if ok {
			// The stream may deliver one final quit before closing.
			if _, ok := <-scr.Events(); ok {
				t.Fatal("event stream still open after Close")
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event stream did not end after Close")
	}
}

// An unbuffered reader that stops reading must not leak the pump goroutine
// past Close.
func TestCloseReleasesABlockedPump(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	scr, err := Wrap(sim)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		sim.InjectKey(tcell.KeyRune, 'x', 0)
	}
	done := make(chan struct{})
	go func() { scr.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked behind a full event queue")
	}
}
