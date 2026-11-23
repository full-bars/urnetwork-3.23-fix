package urnettools

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

func TestTopWheelZoomOverRemoteChangesWindowAndNotOverList(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	for i := 0; i < 6; i++ {
		m.addEvent(topBase.Add(time.Duration(i)*time.Minute), "event "+string(rune('a'+i)), topInfo)
	}
	b := tui.New(120, 40)

	// Switch to page 2 (Remote) so box:remote is on screen.
	m.selectPageID("remote")
	m.render(b)
	rem := regionRect(t, m, "box:remote")
	if got := m.graphWindow(); got != time.Minute {
		t.Fatalf("initial graphWindow = %v, want 1m", got)
	}

	// Wheel down (back) over box:remote zooms out by 1.25x (60s -> 75s).
	m.handle(mouseEv(tcellui.MouseWheel, tcellui.MouseWheelDown, rem.X+5, rem.Y+3))
	if got := m.graphWindow(); got != 75*time.Second {
		t.Fatalf("after wheel down over box:remote: graphWindow = %v, want 1m15s", got)
	}
	if got := m.remoteWindow(); got != 75*time.Second {
		t.Fatalf("after wheel down over box:remote: remoteWindow = %v, want 1m15s", got)
	}
	if m.eventScroll != 0 {
		t.Fatalf("wheel over box:remote scrolled events to %d", m.eventScroll)
	}

	// Wheel up (forward) over box:remote zooms back in (75s -> 60s).
	m.handle(mouseEv(tcellui.MouseWheel, tcellui.MouseWheelUp, rem.X+5, rem.Y+3))
	if got := m.graphWindow(); got != time.Minute {
		t.Fatalf("after wheel up over box:remote: graphWindow = %v, want 1m", got)
	}

	// Horizontal wheel over a graph does nothing.
	m.handle(mouseEv(tcellui.MouseWheel, tcellui.MouseWheelLeft, rem.X+5, rem.Y+3))
	if got := m.graphWindow(); got != time.Minute {
		t.Fatalf("after wheel left over box:remote: graphWindow = %v, want 1m", got)
	}

	// Back on overview: wheel over box:events scrolls the list and does not change the window.
	m.selectPageID("overview")
	m.render(b)
	ev := regionRect(t, m, "box:events")
	m.handle(mouseEv(tcellui.MouseWheel, tcellui.MouseWheelDown, ev.X+5, ev.Y+1))
	if got := m.graphWindow(); got != time.Minute {
		t.Fatalf("wheel over box:events changed graphWindow to %v, want 1m", got)
	}
	if m.eventScroll == 0 {
		t.Fatal("wheel over box:events did not scroll events")
	}
}

func TestTopWheelZoomWorksOnAllGraphRegions(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(m *topModel)
		region string
	}{
		{name: "overview graph", setup: func(m *topModel) { m.selectPageID("overview") }, region: "box:graph"},
		{name: "overview graph title", setup: func(m *topModel) { m.selectPageID("overview") }, region: "title:graph"},
		{name: "overview remote panel", setup: func(m *topModel) { m.selectPageID("overview"); m.remote = true }, region: "box:remote"},
		{name: "remote page", setup: func(m *topModel) { m.selectPageID("remote") }, region: "box:remote"},
		{name: "billable page", setup: func(m *topModel) { m.selectPageID("billable") }, region: "box:billable"},
		{name: "total page", setup: func(m *topModel) { m.selectPageID("total") }, region: "box:total"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &fakeClock{t: topBase}
			m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
			m.apply(0, directionFixture(t), nil)
			tc.setup(m)
			b := tui.New(120, 40)
			m.render(b)
			r := regionRect(t, m, tc.region)
			m.handle(mouseEv(tcellui.MouseWheel, tcellui.MouseWheelDown, r.X+2, r.Y+r.H/2))
			if got := m.graphWindow(); got != 75*time.Second {
				t.Fatalf("wheel down over %s: graphWindow = %v, want 1m15s", tc.region, got)
			}
		})
	}
}
