package urnettools

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
)

func TestScanWaveFlashColorsAndFades(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.proxyHeatmapStyle = heatmapScan
	m.proxyAfterglowSeconds = 5

	cell := proxyCell{index: 1, state: connect.ProxyCellUp, bucket: 0}
	m.proxyCells = []proxyCell{cell}

	// 1. OK check -> Green flash
	m.applyUDPScanEvents([]ProxyUDPScanEvent{
		{Index: 1, Status: "ok", At: topBase},
	})

	st, frac, ok := m.proxyScanFlashState(1, topBase)
	if !ok || st != "ok" || frac < 0.99 {
		t.Fatalf("at 0s: got ok=%v, status=%q, frac=%f", ok, st, frac)
	}
	_, style := m.proxyScanFlashStyle(st, frac, cell.state)
	if style.FG != tui.Xterm(46) || (style.Attrs&tui.AttrBold) == 0 {
		t.Fatalf("peak ok flash style: got FG=%v Attrs=%v, want xterm(46) bold", style.FG, style.Attrs)
	}

	// Mid-fade at 2.5s (50% remaining)
	midTime := topBase.Add(2500 * time.Millisecond)
	st, frac, ok = m.proxyScanFlashState(1, midTime)
	if !ok || st != "ok" || frac > 0.6 || frac < 0.4 {
		t.Fatalf("at 2.5s: got ok=%v, status=%q, frac=%f", ok, st, frac)
	}
	_, style = m.proxyScanFlashStyle(st, frac, cell.state)
	if style.FG != tui.Xterm(82) {
		t.Fatalf("mid-fade ok flash style: got FG=%v, want xterm(82)", style.FG)
	}

	// Expired at 5s
	expTime := topBase.Add(5 * time.Second)
	_, _, ok = m.proxyScanFlashState(1, expTime)
	if ok {
		t.Fatalf("at 5s: flash must be expired")
	}

	// 2. No relay -> Orange flash
	m.applyUDPScanEvents([]ProxyUDPScanEvent{
		{Index: 1, Status: "no_relay", At: expTime},
	})
	st, frac, ok = m.proxyScanFlashState(1, expTime)
	if !ok || st != "no_relay" {
		t.Fatalf("no_relay flash state: ok=%v, st=%q", ok, st)
	}
	_, style = m.proxyScanFlashStyle(st, frac, cell.state)
	if style.FG != tui.Xterm(214) || (style.Attrs&tui.AttrBold) == 0 {
		t.Fatalf("peak no_relay flash style: got FG=%v Attrs=%v, want xterm(214) bold", style.FG, style.Attrs)
	}

	// 3. Mismatch -> Violet flash
	m.applyUDPScanEvents([]ProxyUDPScanEvent{
		{Index: 1, Status: "mismatch", At: expTime},
	})
	st, frac, ok = m.proxyScanFlashState(1, expTime)
	if !ok || st != "mismatch" {
		t.Fatalf("mismatch flash state: ok=%v, st=%q", ok, st)
	}
	_, style = m.proxyScanFlashStyle(st, frac, cell.state)
	if style.FG != tui.Xterm(177) || (style.Attrs&tui.AttrBold) == 0 {
		t.Fatalf("peak mismatch flash style: got FG=%v Attrs=%v, want xterm(177) bold", style.FG, style.Attrs)
	}

	// 4. Unknown -> Dim flash
	m.applyUDPScanEvents([]ProxyUDPScanEvent{
		{Index: 1, Status: "unknown", At: expTime},
	})
	st, frac, ok = m.proxyScanFlashState(1, expTime)
	if !ok || st != "unknown" {
		t.Fatalf("unknown flash state: ok=%v, st=%q", ok, st)
	}
	_, style = m.proxyScanFlashStyle(st, frac, cell.state)
	if (style.Attrs & tui.AttrBold) == 0 {
		t.Fatalf("peak unknown flash style: expected bold attribute")
	}
}

func TestScanWaveThemeDegradation(t *testing.T) {
	clock := &fakeClock{t: topBase}

	// Mono theme: no colors, bold & ASCII glyphs
	mMono := newTestModel(tui.MonoTheme(), topProviders(1), clock)
	mMono.proxyHeatmapStyle = heatmapScan
	glyph, st := mMono.proxyScanFlashStyle("ok", 1.0, connect.ProxyCellUp)
	if st.FG.Kind != tui.ColorDefault {
		t.Fatalf("mono theme must have ColorDefault FG, got %+v", st.FG)
	}
	if (st.Attrs & tui.AttrBold) == 0 {
		t.Fatalf("mono theme peak flash must be bold")
	}
	if glyph != '*' {
		t.Fatalf("mono ok peak glyph = %c, want *", glyph)
	}
	glyphNoRelay, _ := mMono.proxyScanFlashStyle("no_relay", 1.0, connect.ProxyCellUp)
	if glyphNoRelay != '!' {
		t.Fatalf("mono no_relay glyph = %c, want !", glyphNoRelay)
	}
	glyphMismatch, _ := mMono.proxyScanFlashStyle("mismatch", 1.0, connect.ProxyCellUp)
	if glyphMismatch != 'x' {
		t.Fatalf("mono mismatch glyph = %c, want x", glyphMismatch)
	}
	glyphUnknown, _ := mMono.proxyScanFlashStyle("unknown", 1.0, connect.ProxyCellUp)
	if glyphUnknown != '?' {
		t.Fatalf("mono unknown glyph = %c, want ?", glyphUnknown)
	}

	// ASCII theme
	thASCII := tui.Theme{Name: "ascii", ASCII: true}
	mASCII := newTestModel(thASCII, topProviders(1), clock)
	mASCII.proxyHeatmapStyle = heatmapScan
	gASCII, _ := mASCII.proxyScanFlashStyle("ok", 0.3, connect.ProxyCellUp)
	if gASCII != '+' {
		t.Fatalf("ascii faded ok glyph = %c, want +", gASCII)
	}

	// Colorblind theme (Okabe-Ito: blue for ok, orange for warn, yellow for bad)
	thCB, ok := tui.ThemeByName("colorblind")
	if !ok {
		t.Fatal("colorblind theme missing")
	}
	mCB := newTestModel(thCB, topProviders(1), clock)
	mCB.proxyHeatmapStyle = heatmapScan
	gCB, stCB := mCB.proxyScanFlashStyle("ok", 1.0, connect.ProxyCellUp)
	if stCB.FG != thCB.OK.FG {
		t.Fatalf("colorblind ok FG = %v, want %v", stCB.FG, thCB.OK.FG)
	}
	if gCB != '●' {
		t.Fatalf("colorblind ok glyph = %c, want ●", gCB)
	}
	_, stCBWarn := mCB.proxyScanFlashStyle("no_relay", 1.0, connect.ProxyCellUp)
	if stCBWarn.FG != thCB.Warn.FG {
		t.Fatalf("colorblind no_relay FG = %v, want %v", stCBWarn.FG, thCB.Warn.FG)
	}
	_, stCBMismatch := mCB.proxyScanFlashStyle("mismatch", 1.0, connect.ProxyCellUp)
	if stCBMismatch.FG != thCB.Bad.FG {
		t.Fatalf("colorblind mismatch FG = %v, want %v", stCBMismatch.FG, thCB.Bad.FG)
	}
}

func TestScanWaveGridSizes(t *testing.T) {
	clock := &fakeClock{t: topBase}
	sizes := []int{1, 3, 50, 1001, 4000, 65535}

	for _, n := range sizes {
		m := topHeatGridModel(t, tui.DefaultTheme(), n)
		m.proxyHeatmapStyle = heatmapScan
		m.now = clock.Now

		// Feed a scan event for the first proxy
		m.applyUDPScanEvents([]ProxyUDPScanEvent{
			{Index: 1, Status: "ok", At: topBase},
		})

		buf := tui.New(100, 30)
		m.drawProxyHeatmap(buf)

		// Must render "Scan" in the legend row
		s := buf.String()
		if !strings.Contains(s, "Scan") {
			t.Fatalf("n=%d: expected Scan in legend, got:\n%s", n, s)
		}
	}
}

func TestScanWaveSequentialOrderAndRipple(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := topHeatGridModel(t, tui.DefaultTheme(), 10)
	m.proxyHeatmapStyle = heatmapScan
	m.now = clock.Now

	// 1. Line-by-line wave: checks complete sequentially
	eventsSeq := make([]ProxyUDPScanEvent, 10)
	for i := 0; i < 10; i++ {
		eventsSeq[i] = ProxyUDPScanEvent{
			Index:  i + 1,
			Status: "ok",
			At:     topBase.Add(time.Duration(i*100) * time.Millisecond),
		}
	}
	m.applyUDPScanEvents(eventsSeq)

	now := topBase.Add(1000 * time.Millisecond)
	clock.t = now

	// Check that proxy 10 (at 900ms) is fresher than proxy 1 (at 0ms)
	_, frac1, ok1 := m.proxyScanFlashState(1, now)
	_, frac10, ok10 := m.proxyScanFlashState(10, now)
	if !ok1 || !ok10 {
		t.Fatalf("expected both flashes active: ok1=%v, ok10=%v", ok1, ok10)
	}
	if frac10 <= frac1 {
		t.Fatalf("fraction 10 (%f) must be greater than fraction 1 (%f)", frac10, frac1)
	}

	// 2. Out-of-order ripple: check completion order
	m.proxyScanFlashes = nil
	m.applyUDPScanEvents([]ProxyUDPScanEvent{
		{Index: 9, Status: "mismatch", At: now},
		{Index: 2, Status: "no_relay", At: now},
	})
	st9, _, ok9 := m.proxyScanFlashState(9, now)
	st2, _, ok2 := m.proxyScanFlashState(2, now)
	if !ok9 || st9 != "mismatch" || !ok2 || st2 != "no_relay" {
		t.Fatalf("ripple flashes mismatch: 9=(%v,%s), 2=(%v,%s)", ok9, st9, ok2, st2)
	}
}

func TestScanWaveZeroCostWhenOffOrIdle(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := topHeatGridModel(t, tui.DefaultTheme(), 10)
	m.now = clock.Now

	// Style is Glow (off): events must be ignored and not allocated
	m.proxyHeatmapStyle = heatmapGlow
	m.applyUDPScanEvents([]ProxyUDPScanEvent{
		{Index: 1, Status: "ok", At: topBase},
	})
	if m.proxyScanFlashes != nil {
		t.Fatalf("proxyScanFlashes must be nil when style is off")
	}
	if _, _, ok := m.proxyScanFlashState(1, topBase); ok {
		t.Fatalf("flash state must report false when style is off")
	}

	// Style is Scan, but empty events: zero allocation
	m.proxyHeatmapStyle = heatmapScan
	m.applyUDPScanEvents(nil)
	if len(m.proxyScanFlashes) != 0 {
		t.Fatalf("proxyScanFlashes must be empty when events is nil")
	}
}
