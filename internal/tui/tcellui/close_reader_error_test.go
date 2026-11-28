package tcellui

import (
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
)

// errorCloseScreen models a stopped reader and a parser blocked on an event
// until PollEvent resumes after observing the closed quit channel.
type errorCloseScreen struct {
	tcell.Screen
	quit     chan struct{}
	polled   chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (s *errorCloseScreen) PollEvent() tcell.Event {
	<-s.quit
	select {
	case <-s.polled:
		close(s.release)
		<-s.finished
		return nil
	default:
		close(s.polled)
		return nil
	}
}
func (s *errorCloseScreen) Fini() {
	close(s.quit)
	<-s.release
	s.Screen.Fini()
	close(s.finished)
}

func TestCloseDrainsAfterReaderError(t *testing.T) {
	f := &errorCloseScreen{Screen: tcell.NewSimulationScreen("UTF-8"), quit: make(chan struct{}), polled: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
	scr, err := Wrap(f)
	if err != nil {
		t.Fatal(err)
	}
	st := wrapSuspendTty(nil)
	st.stopped = true
	scr.(*screen).tty = st
	done := make(chan struct{})
	go func() { scr.Close(); close(done) }()
	<-f.polled
	select {
	case <-scr.(*screen).pumpDone:
		select {
		case <-f.release:
		case <-time.After(time.Second):
			close(f.release)
			<-done
			t.Fatal("event pump stopped before the parser finished")
		}
	case <-f.release:
		<-done
	case <-time.After(time.Second):
		t.Fatal("close did not finish")
	}
}
