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

func TestTopWheelZoomClampsAtLimits(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	m.selectPageID("remote")
	b := tui.New(160, 40)
	m.render(b)
	rem := regionRect(t, m, "box:remote")

	// Zoom in far past the lower limit: must clamp at 5s.
	for i := 0; i < 40; i++ {
		m.handle(mouseEv(tcellui.MouseWheel, tcellui.MouseWheelUp, rem.X+5, rem.Y+3))
	}
	if got := m.graphWindow(); got != 5*time.Second {
		t.Fatalf("lower clamp = %v, want 5s", got)
	}
	if got := m.remoteWindow(); got != 5*time.Second {
		t.Fatalf("remote lower clamp = %v, want 5s", got)
	}
	m.render(b)
	if !containsLine(b.String(), "w zoom 5s") {
		t.Fatalf("footer at lower limit did not show 'w zoom 5s':\n%s", b.String())
	}
	// One notch out from 5s must move off the clamp.
	m.handle(mouseEv(tcellui.MouseWheel, tcellui.MouseWheelDown, rem.X+5, rem.Y+3))
	if got := m.graphWindow(); got <= 5*time.Second {
		t.Fatalf("zoom out from 5s stayed at %v", got)
	}

	// Zoom out far past the upper limit: must clamp at 15m (preset).
	for i := 0; i < 60; i++ {
		m.handle(mouseEv(tcellui.MouseWheel, tcellui.MouseWheelDown, rem.X+5, rem.Y+3))
	}
	if got := m.graphWindow(); got != 15*time.Minute {
		t.Fatalf("upper clamp = %v, want 15m", got)
	}
	if m.winCustom != 0 {
		t.Fatalf("at 15m preset, winCustom = %v, want 0", m.winCustom)
	}
	if got := m.remoteWindow(); got != time.Duration(topGraphCapacity)*time.Second {
		t.Fatalf("remote upper clamp = %v, want %ds", got, topGraphCapacity)
	}
	m.render(b)
	if !containsLine(b.String(), "w zoom 15m") {
		t.Fatalf("footer at upper limit did not show 'w zoom 15m':\n%s", b.String())
	}
}

func TestTopWheelZoomFooterAndTitleShowExactWindow(t *testing.T) {
	asciiDefault := tui.DefaultTheme()
	asciiDefault.Name = "ascii"
	asciiDefault.ASCII = true
	for _, th := range []tui.Theme{tui.DefaultTheme(), tui.MonoTheme(), asciiDefault} {
		t.Run(th.Name, func(t *testing.T) {
			clock := &fakeClock{t: topBase}
			m := newTestModel(th, topProviders(1), clock)
			m.apply(0, topFlowing(t), nil)
			var at int64 = 1_000_000_000_000
			var bytes uint64
			feedTrafficAt(t, m, clock, 120, time.Second, 1_000_000, &at, &bytes)
			m.winCustom = 100 * time.Second // 1m40s

			b := tui.New(160, 40)
			m.render(b)
			txt := b.String()
			if !containsLine(txt, "w zoom 1m40s") {
				t.Fatalf("theme %s: footer missing 'w zoom 1m40s':\n%s", th.Name, txt)
			}
			if !containsLine(txt, "Billable, 1m40s") {
				t.Fatalf("theme %s: graph title missing 'Billable, 1m40s':\n%s", th.Name, txt)
			}
			// Custom window is not persisted: settings keep the preset name.
			if got := m.settings().GraphWindow; got != "1m" {
				t.Fatalf("theme %s: settings GraphWindow = %q, want preset 1m", th.Name, got)
			}
		})
	}
}

func TestTopWheelZoomKeySnapping(t *testing.T) {
	cases := []struct {
		custom   time.Duration
		key      rune
		wantSpan time.Duration
	}{
		{5 * time.Second, 'w', 10 * time.Second},
		{5 * time.Second, 'W', 15 * time.Minute},
		{48 * time.Second, 'w', time.Minute},
		{48 * time.Second, 'W', 10 * time.Second},
		{100 * time.Second, 'w', 5 * time.Minute},
		{100 * time.Second, 'W', time.Minute},
		{7 * time.Minute, 'w', 15 * time.Minute},
		{7 * time.Minute, 'W', 5 * time.Minute},
	}
	for _, tc := range cases {
		m, _ := liveModel(t, 500*time.Millisecond)
		m.winCustom = tc.custom
		pressKey(m, tc.key)
		if m.winCustom != 0 {
			t.Fatalf("%v + %c: winCustom = %v, want 0", tc.custom, tc.key, m.winCustom)
		}
		if got := m.graphWindow(); got != tc.wantSpan {
			t.Fatalf("%v + %c: graphWindow = %v, want %v", tc.custom, tc.key, got, tc.wantSpan)
		}
	}
}
