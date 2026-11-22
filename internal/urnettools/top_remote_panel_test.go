package urnettools

import (
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// The Remote panel is hidden until the operator asks for it, so every existing
// screen keeps its layout and its goldens.
func TestTopRemoteHiddenByDefault(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	b := tui.New(120, 40)
	m.render(b)
	if strings.Contains(b.String(), "Remote") {
		t.Fatalf("the Remote panel drew without being asked for:\n%s", b.String())
	}
}

// With the panel on, it shares the graph band: the two throughput graphs stay
// and the Remote panel sits below them.
func TestTopRemotePanelSharesTheGraphBand(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	m.remote = true
	b := tui.New(120, 40)
	m.render(b)
	text := b.String()
	for _, want := range []string{"Remote", "Billable", "Total traffic", "billable ▲", "total ▼"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the panel is on screen without %q:\n%s", want, text)
		}
	}
	if !strings.Contains(text, "Proxies") || !strings.Contains(text, "Events") {
		t.Fatalf("the rest of the screen moved away:\n%s", text)
	}
}

// A band too short for both keeps the graphs and leaves the panel out; the key
// still remembers it is on, so enlarging the terminal brings it back.
func TestTopRemoteSuppressedOnAShortBand(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	m.remote = true
	short := tui.New(100, 30)
	m.render(short)
	if strings.Contains(short.String(), "Remote") {
		t.Fatalf("the panel drew in a band too short for it:\n%s", short.String())
	}
	if !strings.Contains(short.String(), "Billable") {
		t.Fatalf("the throughput graph went missing:\n%s", short.String())
	}
	tall := tui.New(120, 40)
	m.render(tall)
	if !strings.Contains(tall.String(), "Remote") {
		t.Fatalf("the panel did not come back on a taller screen:\n%s", tall.String())
	}
}

// The key toggles the panel both ways.
func TestTopRemoteKeyToggles(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	if m.remote {
		t.Fatal("the panel starts on")
	}
	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 'r'})
	if !m.remote {
		t.Fatal("r did not turn the panel on")
	}
	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 'r'})
	if m.remote {
		t.Fatal("r did not turn the panel off again")
	}
}

// The panel draws the client-side ring once it has one, which is the same
// second-by-second series the graph scrolls.
func TestTopRemoteUsesLiveHistory(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	m.trafficHistory.billable = remoteFlat(60, 0)
	m.trafficHistory.total = remoteFlat(60, 0)
	m.trafficHistory.billable[59] = 512 * 1024
	m.trafficHistory.total[59] = 1024 * 1024
	up, down, _, interval, ok := m.remoteData()
	if !ok || interval != time.Second {
		t.Fatalf("live history not used: ok=%v interval=%v", ok, interval)
	}
	if len(up) != 60 || len(down) != 60 {
		t.Fatalf("halves are %d and %d samples, want 60 each", len(up), len(down))
	}
	b := tui.New(78, 11)
	m.drawRemote(b)
	text := b.String()
	if !strings.Contains(text, "512.0 KiB/s") && !strings.Contains(text, "512 KiB/s") {
		t.Fatalf("the live billable peak is not labelled:\n%s", text)
	}
	if !strings.Contains(text, "1.0 MiB/s") && !strings.Contains(text, "1 MiB/s") {
		t.Fatalf("the live total peak is not labelled:\n%s", text)
	}
}

// A provider that predates the traffic block draws billable only, and says so
// in the footer instead of drawing a second flat line.
func TestTopRemoteWithoutTotalTraffic(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	s := topFlowing(t)
	s.Traffic = nil
	m.apply(0, s, nil)
	b := tui.New(78, 11)
	m.drawRemote(b)
	if !strings.Contains(b.String(), "total not reported") {
		t.Fatalf("no note about the missing total figures:\n%s", b.String())
	}
}

// Before the first snapshot there is nothing to draw.
func TestTopRemoteBeforeTheFirstSnapshot(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	b := tui.New(78, 11)
	m.drawRemote(b)
	if !strings.Contains(b.String(), "waiting for the provider") {
		t.Fatalf("no waiting line:\n%s", b.String())
	}
}

// The panel is drawn through the whole screen at any size without panicking,
// which is what the narrow and short degradations rest on.
func TestTopRemoteRendersAtAnySize(t *testing.T) {
	clock := &fakeClock{t: topBase}
	for w := 0; w <= 160; w += 7 {
		for h := 0; h <= 60; h += 3 {
			m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
			m.apply(0, topFlowing(t), nil)
			m.remote = true
			b := tui.New(w, h)
			m.render(b)
		}
	}
}

// TestTopRemotePanelGolden is the panel on screen between the graphs and the
// Proxies box, drawn through the real adapter.
func TestTopRemotePanelGolden(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	m.remote = true
	got, _ := showOnSim(t, m, 120, 40)
	assertGolden(t, "top_remote_panel_on_120x40", got)
	for i, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
		if w := tui.StringWidth(line); w > 120 {
			t.Fatalf("row %d is %d cells in a 120 wide screen", i, w)
		}
	}
	// The mono theme draws the panel with ASCII only.
	m = newTestModel(tui.MonoTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	m.remote = true
	_, sim := showOnSim(t, m, 120, 40)
	cells, _, _ := sim.GetContents()
	for i, c := range cells {
		fg, bg, attr := c.Style.Decompose()
		if fg != tcell.ColorDefault || bg != tcell.ColorDefault || attr != tcell.AttrNone {
			t.Fatalf("cell %d carries style fg=%v bg=%v attr=%v under mono", i, fg, bg, attr)
		}
	}
}
