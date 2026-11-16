package urnettools

import (
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// mouseEv builds one simulated mouse event.
func mouseEv(action tcellui.MouseAction, button tcellui.MouseButton, x, y int) tcellui.Event {
	return tcellui.Event{Kind: tcellui.EventMouse, X: x, Y: y, Button: button, Action: action}
}

// regionRect returns the rect of a named region from the last frame.
func regionRect(t *testing.T, m *topModel, name string) tui.Rect {
	t.Helper()
	for _, r := range m.regions {
		if r.Name == name {
			return r.Rect
		}
	}
	t.Fatalf("region %q not recorded", name)
	return tui.Rect{}
}

func mouseModel(t *testing.T, w, h int) (*topModel, *tui.Buffer) {
	t.Helper()
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	b := tui.New(w, h)
	m.render(b)
	return m, b
}

func TestTopDividerDragResizesTheBoxes(t *testing.T) {
	m, b := mouseModel(t, 120, 40)
	graph := regionRect(t, m, "box:graph")
	if graph.H != 29 {
		t.Fatalf("default graph height = %d, want 29", graph.H)
	}
	// Drag the graph|Proxies divider up five rows.
	d := regionRect(t, m, "left:proxies")
	m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, d.X+5, d.Y))
	m.handle(mouseEv(tcellui.MouseDrag, tcellui.MouseLeft, d.X+5, d.Y-5))
	m.handle(mouseEv(tcellui.MouseRelease, tcellui.MouseLeft, d.X+5, d.Y-5))
	if m.layout.Dividers["left:proxies"] != 24 {
		t.Fatalf("divider row = %d, want 24", m.layout.Dividers["left:proxies"])
	}
	m.render(b)
	graph = regionRect(t, m, "box:graph")
	proxies := regionRect(t, m, "box:proxies")
	events := regionRect(t, m, "box:events")
	if graph.H != 24 || proxies.H != 11 || events.H != 3 {
		t.Fatalf("after drag: graph %d, proxies %d, events %d; want 24 11 3", graph.H, proxies.H, events.H)
	}
}

func TestTopDividerDragHoldsTheMinimumBoxSize(t *testing.T) {
	m, b := mouseModel(t, 120, 40)
	d := regionRect(t, m, "left:proxies")
	m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, d.X+5, d.Y))
	m.handle(mouseEv(tcellui.MouseDrag, tcellui.MouseLeft, d.X+5, d.Y-30))
	m.handle(mouseEv(tcellui.MouseRelease, tcellui.MouseLeft, d.X+5, d.Y-30))
	m.render(b)
	graph := regionRect(t, m, "box:graph")
	proxies := regionRect(t, m, "box:proxies")
	events := regionRect(t, m, "box:events")
	if graph.H < 3 || proxies.H < 3 || events.H < 3 {
		t.Fatalf("a box dropped below its frame plus one row: graph %d, proxies %d, events %d", graph.H, proxies.H, events.H)
	}
}

func TestTopSideDividerDragResizesTheSideColumn(t *testing.T) {
	m, b := mouseModel(t, 150, 40)
	now := regionRect(t, m, "box:now")
	if now.W != 45 {
		t.Fatalf("default side width = %d, want 45", now.W)
	}
	side := regionRect(t, m, "side")
	m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, side.X, side.Y+5))
	m.handle(mouseEv(tcellui.MouseDrag, tcellui.MouseLeft, side.X-10, side.Y+5))
	m.handle(mouseEv(tcellui.MouseRelease, tcellui.MouseLeft, side.X-10, side.Y+5))
	if m.layout.SideWidth != 46 {
		t.Fatalf("side width after drag = %d, want the 46 cap", m.layout.SideWidth)
	}
	m.render(b)
	now = regionRect(t, m, "box:now")
	if now.W != 46 {
		t.Fatalf("side width = %d, want 46", now.W)
	}
}

func TestTopDoubleClickResetsADivider(t *testing.T) {
	m, b := mouseModel(t, 120, 40)
	d := regionRect(t, m, "left:proxies")
	press := func() { m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, d.X+5, d.Y)) }
	press()
	m.handle(mouseEv(tcellui.MouseDrag, tcellui.MouseLeft, d.X+5, d.Y-6))
	m.handle(mouseEv(tcellui.MouseRelease, tcellui.MouseLeft, d.X+5, d.Y-6))
	if _, ok := m.layout.Dividers["left:proxies"]; !ok {
		t.Fatal("the drag should have saved a divider row")
	}
	// A second press on the same divider within the double-click window
	// resets it. The divider has moved since the drag, so re-read its
	// position first.
	m.render(b)
	d = regionRect(t, m, "left:proxies")
	press()
	m.now = func() time.Time { return topBase.Add(300 * time.Millisecond) }
	press()
	if _, ok := m.layout.Dividers["left:proxies"]; ok {
		t.Fatal("the double click should have reset the divider")
	}
}

func TestTopDoubleClickResetsTheSideWidth(t *testing.T) {
	m, b := mouseModel(t, 150, 40)
	side := regionRect(t, m, "side")
	m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, side.X, side.Y+5))
	m.handle(mouseEv(tcellui.MouseDrag, tcellui.MouseLeft, side.X-10, side.Y+5))
	m.handle(mouseEv(tcellui.MouseRelease, tcellui.MouseLeft, side.X-10, side.Y+5))
	if m.layout.SideWidth == 0 {
		t.Fatal("the drag should have saved a side width")
	}
	m.render(b)
	// The divider has moved since the drag, so re-read its position first.
	side = regionRect(t, m, "side")
	m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, side.X, side.Y+5))
	m.now = func() time.Time { return topBase.Add(300 * time.Millisecond) }
	m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, side.X, side.Y+5))
	if m.layout.SideWidth != 0 {
		t.Fatalf("double click should reset the side width, got %d", m.layout.SideWidth)
	}
}

func TestTopLayoutZeroValueIsTheDefault(t *testing.T) {
	m, _ := mouseModel(t, 120, 40)
	if m.layout.SideWidth != 0 || m.layout.Dividers != nil {
		t.Fatalf("zero layout = %+v", m.layout)
	}
}

func TestTopApplySettingsCarriesTheLayout(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.applySettings(topSettings{LayoutSideWidth: 38, LayoutDividers: map[string]int{"left:proxies": 22}})
	if m.layout.SideWidth != 38 || m.layout.Dividers["left:proxies"] != 22 {
		t.Fatalf("layout not applied: %+v", m.layout)
	}
	// Unknown layout rows are dropped on read, so a stale file cannot hold a
	// divider that no longer exists.
	m.applySettings(topSettings{})
	if m.layout.SideWidth != 0 || m.layout.Dividers != nil {
		t.Fatalf("empty settings should reset the layout: %+v", m.layout)
	}
}
