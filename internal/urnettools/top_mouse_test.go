package urnettools

import (
	"strings"
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

func TestTopWheelScrollsTheEventsList(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	for i := 0; i < 8; i++ {
		m.addEvent(topBase.Add(time.Duration(i)*time.Minute), "event "+string(rune('a'+i)), topInfo)
	}
	b := tui.New(120, 40)
	m.render(b)
	ev := regionRect(t, m, "box:events")
	if !containsLine(b.String(), "event h") {
		t.Fatalf("the newest event should be on screen:\n%s", b.String())
	}
	// Wheel down moves the window to the older events.
	m.handle(mouseEv(tcellui.MouseWheel, tcellui.MouseWheelDown, ev.X+5, ev.Y+1))
	m.render(b)
	if !containsLine(b.String(), "event g") {
		t.Fatalf("one wheel step should show the next older event:\n%s", b.String())
	}
	// The offset stops at the last full visible window.
	for i := 0; i < 20; i++ {
		m.handle(mouseEv(tcellui.MouseWheel, tcellui.MouseWheelDown, ev.X+5, ev.Y+1))
	}
	want := max(len(collapseEvents(filterEvents(m.sortedEvents(), m.eventFilter)))-m.eventHeight, 0)
	if m.eventScroll != want {
		t.Fatalf("event scroll = %d, want %d", m.eventScroll, want)
	}
}

func containsLine(txt, sub string) bool {
	for _, l := range mouseSplitLines(txt) {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}

func mouseSplitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func TestTopClickableFooterHints(t *testing.T) {
	m, _ := mouseModel(t, 120, 40)
	// The zoom hint toggles the zoom window, like the w key.
	zoom := regionRect(t, m, "footer:zoom")
	m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, zoom.X+2, zoom.Y))
	if !m.zoomOn {
		t.Fatal("clicking the zoom hint should toggle zoom")
	}
	// The quit hint quits.
	quit := regionRect(t, m, "footer:quit")
	if eff := m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, quit.X+2, quit.Y)); eff != topQuit {
		t.Fatalf("clicking quit gave effect %v", eff)
	}
}

// The provider tab cycles the selection, like Tab.
func TestTopClickableProviderTab(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(2), clock)
	m.apply(0, topFlowing(t), nil)
	b := tui.New(120, 40)
	m.render(b)
	tab := regionRect(t, m, "tab:provider")
	m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, tab.X+5, tab.Y))
	if m.cur != 1 {
		t.Fatalf("tab click should move to the second provider, got %d", m.cur)
	}
}

func TestTopGraphTitleClickTogglesZoom(t *testing.T) {
	m, _ := mouseModel(t, 120, 40)
	title := regionRect(t, m, "title:graph")
	m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, title.X+5, title.Y))
	if !m.zoomOn {
		t.Fatal("clicking the graph title should toggle zoom")
	}
	m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, title.X+5, title.Y))
	if m.zoomOn {
		t.Fatal("the second click should turn zoom back off")
	}
}

func TestTopHoverHighlightsTheRegionUnderTheCursor(t *testing.T) {
	m, b := mouseModel(t, 120, 40)
	quit := regionRect(t, m, "footer:quit")
	m.handle(mouseEv(tcellui.MouseMove, tcellui.MouseNone, quit.X+2, quit.Y))
	if m.hover != "footer:quit" {
		t.Fatalf("hover = %q", m.hover)
	}
	m.render(b)
	c := b.Cell(quit.X+1, quit.Y)
	if c.Style.Attrs&tui.AttrUnderline == 0 {
		t.Fatalf("the hovered hint is not underlined: %+v", c.Style)
	}
	// The mono theme has no color and no attributes: hover must not add one.
	clock := &fakeClock{t: topBase}
	m2 := newTestModel(tui.MonoTheme(), topProviders(1), clock)
	m2.apply(0, topFlowing(t), nil)
	b2 := tui.New(120, 40)
	m2.handle(mouseEv(tcellui.MouseMove, tcellui.MouseNone, 2, 39))
	m2.render(b2)
	for y := 0; y < b2.Height(); y++ {
		for x := 0; x < b2.Width(); x++ {
			if st := b2.Cell(x, y).Style; st.Attrs != 0 || st.FG.Kind != tui.ColorDefault {
				t.Fatalf("mono hover drew style %+v at %d,%d", st, x, y)
			}
		}
	}
}

func TestTopMouseOutsideTheScreenIsIgnored(t *testing.T) {
	m, _ := mouseModel(t, 120, 40)
	// A click far outside the buffer hits no region and does nothing.
	if eff := m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, 500, 500)); eff != topNone {
		t.Fatalf("outside click gave effect %v", eff)
	}
	if m.hover != "" {
		t.Fatalf("hover outside = %q", m.hover)
	}
}

func TestTopPanelBlocksDividerInput(t *testing.T) {
	m, _ := mouseModel(t, 120, 40)
	d := regionRect(t, m, "left:proxies")
	m.panel = "gauges"
	m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, d.X+5, d.Y))
	if m.panel != "" || m.drag != nil {
		t.Fatalf("panel=%q drag=%v", m.panel, m.drag)
	}
}

func TestTopDragThenPressDoesNotReset(t *testing.T) {
	m, b := mouseModel(t, 120, 40)
	d := regionRect(t, m, "left:proxies")
	m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, d.X+5, d.Y))
	m.handle(mouseEv(tcellui.MouseDrag, tcellui.MouseLeft, d.X+5, d.Y-5))
	m.handle(mouseEv(tcellui.MouseRelease, tcellui.MouseLeft, d.X+5, d.Y-5))
	m.render(b)
	d = regionRect(t, m, "left:proxies")
	m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, d.X+5, d.Y))
	if m.drag == nil {
		t.Fatal("press after drag should start a new drag")
	}
}

func TestTopOverlayReleaseCancelsDrag(t *testing.T) {
	for _, overlay := range []string{"menu", "help", "gauges"} {
		t.Run(overlay, func(t *testing.T) {
			m, _ := mouseModel(t, 120, 40)
			d := regionRect(t, m, "left:proxies")
			m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, d.X+5, d.Y))
			if m.drag == nil {
				t.Fatal("expected active drag")
			}
			switch overlay {
			case "menu":
				m.menu = true
			case "help":
				m.help = true
			default:
				m.panel = overlay
			}
			m.handle(mouseEv(tcellui.MouseRelease, tcellui.MouseLeft, d.X+5, d.Y))
			if m.drag != nil {
				t.Fatal("overlay retained released drag")
			}
		})
	}
}
