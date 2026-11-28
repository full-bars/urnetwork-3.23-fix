package tcellui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// fullQueueCloseScreen models a parser that still posts key events after Fini
// closed its quit channel, more than the event queue holds, while nobody reads
// the queue because the reader is the goroutine calling Close.
type fullQueueCloseScreen struct {
	tcell.Screen
	quit     chan struct{}
	pending  chan tcell.Event
	finished chan struct{}
}

func (s *fullQueueCloseScreen) PollEvent() tcell.Event {
	<-s.quit
	select {
	case ev := <-s.pending:
		return ev
	case <-s.finished:
		return nil
	}
}

func (s *fullQueueCloseScreen) Fini() {
	close(s.quit)
	// tcell's Fini waits for its parser, and the parser waits until every
	// pending event has been taken by PollEvent.
	for len(s.pending) != 0 {
		time.Sleep(time.Millisecond)
	}
	s.Screen.Fini()
	close(s.finished)
}

func TestCloseDoesNotBlockOnAFullEventQueue(t *testing.T) {
	f := &fullQueueCloseScreen{
		Screen:   tcell.NewSimulationScreen("UTF-8"),
		quit:     make(chan struct{}),
		pending:  make(chan tcell.Event, 64),
		finished: make(chan struct{}),
	}
	for i := 0; i < cap(f.pending); i++ {
		f.pending <- tcell.NewEventKey(tcell.KeyRune, 'x', tcell.ModNone)
	}
	scr, err := Wrap(f)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { scr.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close hung: the event pump blocked on a full queue while the screen was finalizing")
	}
}
