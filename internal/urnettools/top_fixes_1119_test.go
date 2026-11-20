package urnettools

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// The popup shows the UDP check state of the proxy.
func TestProxyCellInfoShowsUDPStatus(t *testing.T) {
	m := topHeatGridModel(t, tui.DefaultTheme(), 3)
	m.proxyDetail = map[int]ProxyCellDetail{
		1: {Index: 1, Addr: "198.51.100.7:1080", UDP: "no_relay"},
		2: {Index: 2, Addr: "198.51.100.8:1080", UDP: "ok"},
	}
	for _, c := range []struct {
		index int
		want  string
	}{{1, "udp no UDP"}, {2, "udp ok"}} {
		found := false
		for _, line := range m.proxyCellInfo(c.index) {
			if line == c.want {
				found = true
			}
		}
		if !found {
			t.Errorf("proxy %d info %v lacks %q", c.index, m.proxyCellInfo(c.index), c.want)
		}
	}
	m.proxyDetail = nil
	for _, line := range m.proxyCellInfo(1) {
		if line == "udp ?" {
			return
		}
	}
	t.Errorf("info before the detail arrives should say udp ?: %v", m.proxyCellInfo(1))
}

func TestProxyUDPDisplay(t *testing.T) {
	for in, want := range map[string]string{"ok": "ok", "no_relay": "no UDP", "mismatch": "mismatch", "unknown": "unknown", "unchecked": "unchecked", "direct": "native (no relay)", "": "?", "other": "?"} {
		if got := proxyUDPDisplay(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// Every proxy state has a color of its own in the default theme, and no up
// shade, whatever its throughput, is the color of a state.
func TestProxyCellStatesAreVisuallyDistinct(t *testing.T) {
	m := topHeatGridModel(t, tui.DefaultTheme(), 1)
	states := []uint8{connect.ProxyCellDegraded, connect.ProxyCellConnecting, connect.ProxyCellDead, connect.ProxyCellParked}
	seen := map[tui.Color]uint8{}
	for _, state := range states {
		fg := m.proxyCellStyle(state, 4).FG
		if other, dup := seen[fg]; dup {
			t.Errorf("states %d and %d share a color", state, other)
		}
		seen[fg] = state
	}
	for bucket := uint8(0); bucket < 8; bucket++ {
		fg := m.proxyCellStyle(connect.ProxyCellUp, bucket).FG
		if state, dup := seen[fg]; dup {
			t.Errorf("up bucket %d has the color of state %d", bucket, state)
		}
	}
}

// A burst that has scrolled off the left edge no longer holds the axis high:
// the axis follows the columns on screen.
func TestGraphAxisFollowsVisibleColumns(t *testing.T) {
	m := topHeatGridModel(t, tui.DefaultTheme(), 1)
	clock := &fakeClock{t: topBase}
	m.now = clock.Now
	samples := make([]float64, 300)
	for i := range samples {
		samples[i] = 1 << 20 // 1 MiB/s
	}
	samples[10] = 250 << 20 // an old burst, far off the left of a 40 column graph
	scale := &topScale{}
	sr := topSeries{columns: true}
	draw := func() {
		b := tui.New(50, 8)
		m.drawGraph(b, sr, samples, scale, 1<<20, tui.Style{})
	}
	draw()
	clock.Advance(topScaleHold + time.Second)
	draw()
	if scale.top > 16<<20 {
		t.Fatalf("the axis is held at %.0f MiB/s by a burst that is not on screen", scale.top/(1<<20))
	}
	// A rise that fills a tenth of the columns on screen raises it at once. A
	// single spike would be clipped instead (TestGraphAxisIgnoresAShortBurst).
	for i := len(samples) - 16; i < len(samples); i++ {
		samples[i] = 40 << 20
	}
	draw()
	if scale.top < 40<<20 {
		t.Fatalf("a visible 40 MiB/s rise does not fit: axis %.0f MiB/s", scale.top/(1<<20))
	}
}

// At a 100 ms refresh the ten minute graph shows the newest columns of a long
// retained history. A burst far to the left of the visible columns must not set
// the axis, or the live traffic draws one pixel high.
func TestTenMinuteGraphAxisIgnoresAScrolledOffBurst(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	var total, billable uint64
	for i := 0; i < 700; i++ {
		clock.Advance(100 * time.Millisecond)
		rate := uint64(1 << 20) // 1 MiB/s
		if i == 40 {
			rate = 250 << 20
		}
		total += rate / 10
		billable += rate / 30
		if gen, _, ok := m.wantTraffic(clock.Now()); ok {
			m.applyTraffic(gen, &LiveTraffic{AtUnixNano: clock.Now().UnixNano(), BillableBytes: billable, TotalBytes: total}, nil)
		}
	}
	b := tui.New(120, 40)
	render := func() {
		b.Fill(b.Rect(), ' ', tui.Style{})
		m.render(b)
	}
	render()
	clock.Advance(topScaleHold + time.Second)
	render()
	if m.scaleT.top > 32<<20 {
		t.Fatalf("the total graph axis is %.0f MiB/s although the visible columns carry about 1 MiB/s", m.scaleT.top/(1<<20))
	}
}

// The loop wakes on every tick and every poll result. With a 5 ms tick that is
// two hundred wakeups a second, and it must not draw on each one.
func TestRunTopCapsTheRedrawRate(t *testing.T) {
	m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	src, _ := fakeSource(t)
	rig := startLoop(t, m, src, 100, 30)
	rig.waitFor(t, "the first frame", func(text string) bool { return strings.TrimSpace(text) != "" })
	before := rig.scr.draws.Load()
	time.Sleep(time.Second)
	drawn := rig.scr.draws.Load() - before
	// 20 frames a second is the cap, the margin covers a frame at each edge
	if drawn > 24 {
		t.Fatalf("%d frames in one second, want at most about 20", drawn)
	}
	if drawn < 5 {
		t.Fatalf("only %d frames in one second, the screen is not refreshing", drawn)
	}
}

// Mouse movement inside one region changes nothing on screen, so it must not
// ask for a redraw. A move to another region or cell does.
func TestHoverSignatureChangesOnlyWhenTheHoverDoes(t *testing.T) {
	m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	m.regions = []topRegion{{Name: "a", Rect: tui.Rect{X: 0, Y: 0, W: 10, H: 5}}, {Name: "b", Rect: tui.Rect{X: 10, Y: 0, W: 10, H: 5}}}
	move := func(x, y int) {
		m.mouseHandle(tcellui.Event{Kind: tcellui.EventMouse, X: x, Y: y, Action: tcellui.MouseMove})
	}
	move(1, 1)
	first := m.hoverSignature()
	move(2, 2)
	if m.hoverSignature() != first {
		t.Fatal("moving inside one region changed the hover signature")
	}
	move(12, 2)
	if m.hoverSignature() == first {
		t.Fatal("moving to another region did not change the hover signature")
	}
}

// A few seconds of burst, such as every proxy connecting after a restart, must
// not hold the axis so high that the steady traffic draws one pixel high. The
// burst is clipped and its real height is noted.
func TestGraphAxisIgnoresAShortBurst(t *testing.T) {
	if got := robustPeak(append(make([]float64, 0), 1, 2, 3)); got != 3 {
		t.Fatalf("a short series uses its highest value, got %v", got)
	}
	samples := make([]float64, 300)
	for i := range samples {
		samples[i] = float64(1<<20) * (0.8 + 0.4*float64(i%5)/4) // about 1 MiB/s
	}
	for i := 100; i < 106; i++ {
		samples[i] = 160 << 20 // six seconds of 160 MiB/s
	}
	if peak := robustPeak(samples); peak > 2<<20 {
		t.Fatalf("the axis peak is %.1f MiB/s, a six second burst set it", peak/(1<<20))
	}
	// a sustained rise is not a burst
	for i := 100; i < 200; i++ {
		samples[i] = 50 << 20
	}
	if peak := robustPeak(samples); peak < 50<<20 {
		t.Fatalf("a sustained 50 MiB/s rise was treated as a burst: peak %.1f MiB/s", peak/(1<<20))
	}
}

func TestClippedGraphNamesItsRealPeak(t *testing.T) {
	m := topHeatGridModel(t, tui.DefaultTheme(), 1)
	m.now = (&fakeClock{t: topBase}).Now
	samples := make([]float64, 200)
	for i := range samples {
		samples[i] = 1 << 20
	}
	samples[50] = 160 << 20
	b := tui.New(60, 8)
	m.drawGraph(b, topSeries{}, samples, &topScale{}, 0, tui.Style{})
	var text strings.Builder
	for x := 0; x < b.Width(); x++ {
		text.WriteRune(b.Cell(x, 0).Rune)
	}
	if !strings.Contains(text.String(), "clipped, peak 160") {
		t.Fatalf("the first row does not name the clipped peak: %q", text.String())
	}
}
