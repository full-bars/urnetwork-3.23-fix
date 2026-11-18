package tcellui

import (
	"testing"

	"github.com/gdamore/tcell/v2"
)

func TestMouseReportingTransitionResetsHeldButton(t *testing.T) {
	sim := tcell.NewSimulationScreen("UTF-8")
	if err := sim.Init(); err != nil {
		t.Fatal(err)
	}
	defer sim.Fini()
	s := &screen{s: sim}
	event := tcell.NewEventMouse(0, 0, tcell.Button1, 0)
	if got, _ := s.translateMouse(event); got.Action != MousePress {
		t.Fatalf("first press: %+v", got)
	}
	s.SetMouse(true)
	if got, _ := s.translateMouse(event); got.Action != MouseDrag {
		t.Fatalf("unchanged reporting lost drag: %+v", got)
	}
	s.SetMouse(false)
	s.SetMouse(true)
	if got, _ := s.translateMouse(event); got.Action != MousePress {
		t.Fatalf("press after missed release: %+v", got)
	}
}
