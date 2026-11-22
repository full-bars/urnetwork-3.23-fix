package tcellui

import (
	"errors"
	"testing"

	"github.com/gdamore/tcell/v2"
	"github.com/urnetwork/connect/internal/tui"
)

// suspendCounter is a terminal that counts the suspend and resume calls top's
// adapter makes on it. It wraps a simulation screen, so the calls land on a
// real tcell screen.
type suspendCounter struct {
	tcell.Screen
	suspends, resumes int
	failResume        bool
}

func (c *suspendCounter) Suspend() error {
	c.suspends++
	return c.Screen.Suspend()
}

func (c *suspendCounter) Resume() error {
	c.resumes++
	if c.failResume {
		return errors.New("resume failed")
	}
	return c.Screen.Resume()
}

func TestScreenSuspendAndResumeReachTheTerminalOnce(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	fake := &suspendCounter{Screen: sim}
	scr, err := Wrap(fake)
	if err != nil {
		t.Fatal(err)
	}
	defer scr.Close()
	sim.SetSize(40, 10)

	if err := scr.Suspend(); err != nil {
		t.Fatalf("Suspend: %v", err)
	}
	if fake.suspends != 1 {
		t.Fatalf("terminal suspends = %d, want 1", fake.suspends)
	}
	// A second Suspend is not a second disengage: the terminal is already back
	// with the shell and tcell would refuse it.
	if err := scr.Suspend(); err != nil {
		t.Fatalf("the second Suspend: %v", err)
	}
	if fake.suspends != 1 {
		t.Fatalf("terminal suspends after a second call = %d, want 1", fake.suspends)
	}

	if err := scr.Resume(); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if fake.resumes != 1 {
		t.Fatalf("terminal resumes = %d, want 1", fake.resumes)
	}
	// And a Resume with nothing suspended changes nothing.
	if err := scr.Resume(); err != nil {
		t.Fatalf("the second Resume: %v", err)
	}
	if fake.resumes != 1 {
		t.Fatalf("terminal resumes after a second call = %d, want 1", fake.resumes)
	}

	// The screen draws again once it is back.
	b := tui.New(20, 3)
	b.Put("back", 0, 0, tui.Style{})
	scr.Draw(b)
}

func TestScreenDrawIsHeldWhileSuspended(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	fake := &suspendCounter{Screen: sim}
	scr, err := Wrap(fake)
	if err != nil {
		t.Fatal(err)
	}
	defer scr.Close()
	sim.SetSize(20, 3)

	if err := scr.Suspend(); err != nil {
		t.Fatal(err)
	}
	b := tui.New(20, 3)
	b.Put("suspend", 0, 0, tui.Style{})
	scr.Draw(b)
	// The frame must not reach the terminal while the shell (or sudo) owns it.
	if got := simText(sim); got != "" {
		t.Fatalf("a draw while suspended reached the terminal:\n%q", got)
	}
}

func TestScreenResumeReportsAFailedResume(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	fake := &suspendCounter{Screen: sim, failResume: true}
	scr, err := Wrap(fake)
	if err != nil {
		t.Fatal(err)
	}
	defer scr.Close()
	if err := scr.Suspend(); err != nil {
		t.Fatal(err)
	}
	if err := scr.Resume(); err == nil {
		t.Fatal("a failed resume should report the error")
	}
	// A failed resume still clears the suspended state, so the screen is not
	// stuck refusing draws for the rest of the session.
	b := tui.New(10, 2)
	b.Put("x", 0, 0, tui.Style{})
	scr.Draw(b)
}

// simText reads the simulated terminal as text, one line per row.
func simText(sim tcell.SimulationScreen) string {
	cells, w, h := sim.GetContents()
	out := make([]rune, 0, w*h)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) == 0 {
				out = append(out, ' ')
				continue
			}
			out = append(out, c.Runes[0])
		}
	}
	blank := true
	for _, r := range out {
		if r != ' ' {
			blank = false
			break
		}
	}
	if blank {
		return ""
	}
	return string(out)
}
