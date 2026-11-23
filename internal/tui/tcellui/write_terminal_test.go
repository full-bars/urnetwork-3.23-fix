package tcellui

import (
	"bytes"
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

// A wrapped simulation screen has no terminal of its own, so a write outside
// the frame is refused instead of going to whatever stdout happens to be.
func TestWriteTerminalNeedsATerminal(t *testing.T) {
	scr, _ := newSim(t, 40, 10)
	if err := scr.WriteTerminal([]byte("\x1b]52;c;eA==\a")); err == nil {
		t.Fatal("a simulation screen should refuse a raw terminal write")
	}
}

// The write goes to the terminal the screen was opened on, and the frame after
// it is intact: the sequence consumes no cells.
func TestWriteTerminalWritesAndKeepsTheFrame(t *testing.T) {
	scr, sim := newSim(t, 40, 10)
	var out bytes.Buffer
	scr.(*screen).raw = &out

	b := tui.New(40, 10)
	b.Put("top", 0, 0, tui.Style{})
	scr.Draw(b)

	seq := "\x1b]52;c;eA==\a"
	if err := scr.WriteTerminal([]byte(seq)); err != nil {
		t.Fatal(err)
	}
	if out.String() != seq {
		t.Fatalf("terminal got %q, want %q", out.String(), seq)
	}
	cells, w, h := sim.GetContents()
	if w != 40 || h != 10 {
		t.Fatalf("screen size changed to %dx%d", w, h)
	}
	// The first row still reads top; the escape sequence did not become cells.
	var row strings.Builder
	for x := 0; x < 3; x++ {
		row.WriteString(string(cells[x].Runes))
	}
	if row.String() != "top" {
		t.Fatalf("the frame lost its first row: %q", row.String())
	}
	// The screen was resynced from tcell's back buffer, so a later draw still
	// works and the buffer is not left blank.
	scr.Draw(b)
	cells, _, _ = sim.GetContents()
	if string(cells[0].Runes) != "t" {
		t.Fatalf("the frame after a resync reads %q", string(cells[0].Runes))
	}
}

// A write after Close must not touch the terminal: the alternate screen is
// already gone by then.
func TestWriteTerminalAfterCloseIsRefused(t *testing.T) {
	scr, _ := newSim(t, 20, 5)
	var out bytes.Buffer
	scr.(*screen).raw = &out
	scr.Close()
	if err := scr.WriteTerminal([]byte("x")); err == nil {
		t.Fatal("a closed screen should refuse a write")
	}
	if out.Len() != 0 {
		t.Fatalf("the terminal got %q after close", out.String())
	}
}
