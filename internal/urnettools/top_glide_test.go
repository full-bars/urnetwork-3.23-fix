package urnettools

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/urnetwork/connect/internal/tui"
)

// fakeTopTimer is a deterministic timer for testing runTop without sleeps.
type fakeTopTimer struct {
	ch      chan time.Time
	delay   time.Duration
	stopped atomic.Bool
	onStop  func()
}

func (f *fakeTopTimer) Chan() <-chan time.Time { return f.ch }

func (f *fakeTopTimer) Stop() bool {
	if f.stopped.CompareAndSwap(false, true) {
		if f.onStop != nil {
			f.onStop()
		}
		return true
	}
	return false
}

// fakeTimerHub records every glide timer runTop creates or stops and lets a
// test fire the current timer on demand.
type fakeTimerHub struct {
	mu      sync.Mutex
	starts  int
	stops   int
	active  int
	delays  []time.Duration
	current *fakeTopTimer
	started chan time.Duration
	stopped chan struct{}
}

func newFakeTimerHub() *fakeTimerHub {
	return &fakeTimerHub{
		started: make(chan time.Duration, 64),
		stopped: make(chan struct{}, 64),
	}
}

func (h *fakeTimerHub) newTimer(d time.Duration) topTimer {
	h.mu.Lock()
	h.starts++
	h.active++
	h.delays = append(h.delays, d)
	ft := &fakeTopTimer{
		ch:    make(chan time.Time, 1),
		delay: d,
	}
	ft.onStop = func() {
		h.mu.Lock()
		h.stops++
		h.active--
		if h.current == ft {
			h.current = nil
		}
		h.mu.Unlock()
		select {
		case h.stopped <- struct{}{}:
		default:
		}
	}
	h.current = ft
	h.mu.Unlock()
	select {
	case h.started <- d:
	default:
	}
	return ft
}

func (h *fakeTimerHub) fire(now time.Time) bool {
	h.mu.Lock()
	ft := h.current
	if ft == nil || ft.stopped.Load() {
		h.mu.Unlock()
		return false
	}
	h.current = nil
	h.active--
	ft.stopped.Store(true)
	h.mu.Unlock()
	ft.ch <- now
	return true
}

func (h *fakeTimerHub) stats() (starts, stops, active int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.starts, h.stops, h.active
}

func TestNextDotBoundary(t *testing.T) {
	// 10s over 240 dots: one dot is ~41.8ms, clamped to topMinFrame (50ms).
	if got := nextDotBoundary(topBase, 10*time.Second, 240); got != topMinFrame {
		t.Fatalf("10s/240 dots = %v, want %v", got, topMinFrame)
	}
	// 1m over 240 dots: one dot is ~251ms. Starting on an exact boundary gives
	// one full step.
	step1m := time.Duration(int64(time.Minute) / 239)
	aligned1m := time.Unix(0, topBase.UnixNano()-topBase.UnixNano()%int64(step1m))
	if got := nextDotBoundary(aligned1m, time.Minute, 240); got != step1m {
		t.Fatalf("1m aligned = %v, want %v", got, step1m)
	}
	// Just before the next boundary, the floor keeps redraws at most 20 fps.
	if got := nextDotBoundary(aligned1m.Add(step1m-5*time.Millisecond), time.Minute, 240); got != topMinFrame {
		t.Fatalf("1m 5ms before boundary = %v, want %v", got, topMinFrame)
	}
	// 5m and 15m views redraw every few seconds, not at 20 fps.
	if got := nextDotBoundary(topBase, 5*time.Minute, 240); got < 500*time.Millisecond {
		t.Fatalf("5m/240 dots = %v, want multi-hundred-ms step", got)
	}
	step15m := time.Duration(int64(15*time.Minute) / 239)
	aligned15m := time.Unix(0, topBase.UnixNano()-topBase.UnixNano()%int64(step15m))
	if got := nextDotBoundary(aligned15m, 15*time.Minute, 240); got != step15m {
		t.Fatalf("15m aligned = %v, want %v", got, step15m)
	}
	// Invalid inputs return 0.
	if got := nextDotBoundary(topBase, 0, 240); got != 0 {
		t.Fatalf("zero window = %v, want 0", got)
	}
	if got := nextDotBoundary(topBase, time.Minute, 1); got != 0 {
		t.Fatalf("1 dot = %v, want 0", got)
	}
}

func TestGlideDelayStopConditions(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	snap := packetsFixture(t)
	m.apply(0, snap, nil)
	m.contentRect = tui.Rect{X: 0, Y: 2, W: 120, H: 36}

	// All four graph pages schedule a tick when connected with traffic.
	for _, id := range []string{"overview", "remote", "billable", "total"} {
		m.page = pageByID(id)
		d, ok := m.glideDelay(clock.t)
		if !ok || d < topMinFrame {
			t.Fatalf("page %q: glideDelay = (%v, %v), want active >= %v", id, d, ok, topMinFrame)
		}
	}

	// Off-graph page never starts the timer.
	m.page = pageByID("transports")
	if d, ok := m.glideDelay(clock.t); ok {
		t.Fatalf("transports page: glideDelay = (%v, true), want inactive", d)
	}

	// Back to Remote: verify zoom levels change the cadence.
	m.page = pageByID("remote")
	m.winLevel = 0 // 10s
	d10s, ok := m.glideDelay(clock.t)
	if !ok || d10s != topMinFrame {
		t.Fatalf("remote 10s = (%v, %v), want (%v, true)", d10s, ok, topMinFrame)
	}
	m.winLevel = 3 // 15m (capped to 601s for remoteWindow)
	// Advance past the 500ms newest-sample ease and step onto the next dot
	// boundary so we measure one full dot interval (~2.55s at 120 cols).
	tSettled := clock.t.Add(time.Second)
	dFirst, ok := m.glideDelay(tSettled)
	if !ok {
		t.Fatalf("remote 15m settled = (%v, false), want active", dFirst)
	}
	d15m, ok := m.glideDelay(tSettled.Add(dFirst))
	if !ok || d15m <= 2*time.Second {
		t.Fatalf("remote 15m full step = (%v, %v), want > 2s", d15m, ok)
	}

	// Custom wheel zoom (5s) clamps to topMinFrame.
	m.winCustom = 5 * time.Second
	if d5s, ok := m.glideDelay(clock.t.Add(time.Second)); !ok || d5s != topMinFrame {
		t.Fatalf("remote custom 5s = (%v, %v), want (%v, true)", d5s, ok, topMinFrame)
	}
	m.winCustom = 0

	// Scale ease in flight runs at topMinFrame even at 15m zoom.
	m.remoteScales = map[string]*remoteScale{
		"billable": {from: 1000, to: 2000, start: clock.t, set: true},
	}
	if dEase, ok := m.glideDelay(clock.t.Add(100 * time.Millisecond)); !ok || dEase != topMinFrame {
		t.Fatalf("remote 15m during scale ease = (%v, %v), want (%v, true)", dEase, ok, topMinFrame)
	}
	m.remoteScales = nil

	// Every overlay / modal / disconnect / quit stops the timer.
	stops := []struct {
		name string
		set  func(bool)
	}{
		{"help", func(v bool) { m.help = v }},
		{"menu", func(v bool) { m.menu = v }},
		{"about", func(v bool) { m.about = v }},
		{"overview_table", func(v bool) { m.overview = v }},
		{"panel", func(v bool) {
			if v {
				m.panel = "now"
			} else {
				m.panel = ""
			}
		}},
		{"proxyPopup", func(v bool) { m.proxyPopup = v }},
		{"lingerDialog", func(v bool) { m.lingerFix.dialog = v }},
		{"compact", func(v bool) { m.compact = v }},
		{"quit", func(v bool) { m.quit = v }},
		{"disconnected", func(v bool) {
			if v {
				m.conn = topDisconnected
			} else {
				m.conn = topConnected
			}
		}},
	}
	for _, tc := range stops {
		tc.set(true)
		if d, ok := m.glideDelay(clock.t.Add(time.Second)); ok {
			t.Fatalf("with %s active: glideDelay = (%v, true), want inactive", tc.name, d)
		}
		tc.set(false)
	}

	// Static chart: once all traffic has scrolled past the window, the timer stops.
	m.winLevel = 1 // 1m
	if d, ok := m.glideDelay(clock.t.Add(2 * time.Minute)); ok {
		t.Fatalf("after traffic scrolled out of window: glideDelay = (%v, true), want inactive", d)
	}
}

func TestRunTopGlideTimerLifecycleAndStaticPages(t *testing.T) {
	waitStarted := func(t *testing.T, hub *fakeTimerHub) time.Duration {
		t.Helper()
		select {
		case d := <-hub.started:
			return d
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for glide timer to start")
			return 0
		}
	}
	waitStopped := func(t *testing.T, hub *fakeTimerHub) {
		t.Helper()
		select {
		case <-hub.stopped:
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for glide timer to stop")
		}
	}

	t.Run("off_graph_page_never_starts_timer", func(t *testing.T) {
		clock := &fakeClock{t: topBase}
		hub := newFakeTimerHub()
		m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
		m.newTimer = hub.newTimer
		m.page = pageByID("transports")
		src, _ := fakeSource(t)
		rig := startLoop(t, m, src, 120, 40)
		rig.waitFor(t, "Transports", func(s string) bool { return strings.Contains(s, "FLOWING") })
		drawsBefore := rig.scr.draws.Load()
		// Quit synchronously after loop is settled: no extra draws and zero timer starts.
		rig.sim.InjectKey(tcell.KeyRune, 'q', 0)
		if err := rig.wait(t); err != nil {
			t.Fatal(err)
		}
		if got := rig.scr.draws.Load(); got != drawsBefore {
			t.Fatalf("static transports page redrew %d extra times, want 0", got-drawsBefore)
		}
		if starts, _, active := hub.stats(); starts != 0 || active != 0 {
			t.Fatalf("off-graph page: starts=%d active=%d, want 0/0", starts, active)
		}
	})

	t.Run("static_idle_graph_does_not_redraw", func(t *testing.T) {
		clock := &fakeClock{t: topBase}
		hub := newFakeTimerHub()
		m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
		m.newTimer = hub.newTimer
		m.page = pageByID("remote")
		src := topSourceFunc(func(Provider) (*NodeSnapshot, error) {
			s := topFlowing(t)
			s.Rate.HistoryBps = remoteFlat(60, 0)
			if s.Traffic != nil {
				s.Traffic.TotalHistoryBps = remoteFlat(60, 0)
				s.Traffic.Directions = nil
				s.Traffic.Packets = nil
			}
			return s, nil
		})
		rig := startLoop(t, m, src, 120, 40)
		rig.waitFor(t, "Remote", func(s string) bool { return strings.Contains(s, "FLOWING") })
		drawsBefore := rig.scr.draws.Load()
		rig.sim.InjectKey(tcell.KeyRune, 'q', 0)
		if err := rig.wait(t); err != nil {
			t.Fatal(err)
		}
		if got := rig.scr.draws.Load(); got != drawsBefore {
			t.Fatalf("static idle graph redrew %d extra times, want 0", got-drawsBefore)
		}
		if starts, _, active := hub.stats(); starts != 0 || active != 0 {
			t.Fatalf("idle graph: starts=%d active=%d, want 0/0", starts, active)
		}
	})

	t.Run("starts_fires_and_stops_on_help_disconnect_and_quit", func(t *testing.T) {
		var mu sync.Mutex
		now := topBase
		var down atomic.Bool
		src := topSourceFunc(func(Provider) (*NodeSnapshot, error) {
			if down.Load() {
				return nil, errors.New("live status unavailable: connection refused")
			}
			return topFlowing(t), nil
		})
		hub := newFakeTimerHub()
		m := newTopModel(topProviders(1), 0, topDefaultInterval, tui.DefaultTheme(), func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return now
		})
		m.newTimer = hub.newTimer
		m.page = pageByID("remote")
		m.winLevel = 0 // 10s
		rig := startLoop(t, m, src, 120, 40)
		rig.waitFor(t, "Remote", func(s string) bool { return strings.Contains(s, "FLOWING") })

		d := waitStarted(t, hub)
		if d != topMinFrame {
			t.Fatalf("remote 10s timer delay = %v, want %v", d, topMinFrame)
		}

		// Firing the timer triggers a redraw and schedules the next timer.
		drawsBefore := rig.scr.draws.Load()
		mu.Lock()
		now = now.Add(topMinFrame)
		fireAt := now
		mu.Unlock()
		if !hub.fire(fireAt) {
			t.Fatal("expected an active timer to fire")
		}
		waitStarted(t, hub)
		if rig.scr.draws.Load() <= drawsBefore {
			t.Fatal("firing the glide timer did not redraw the screen")
		}

		// Opening help stops the timer.
		rig.sim.InjectKey(tcell.KeyRune, '?', 0)
		waitStopped(t, hub)
		if _, _, active := hub.stats(); active != 0 {
			t.Fatalf("help open: active timers = %d, want 0", active)
		}

		// Closing help restarts the timer.
		rig.sim.InjectKey(tcell.KeyEsc, 0, 0)
		waitStarted(t, hub)

		// Disconnecting stops the timer.
		down.Store(true)
		mu.Lock()
		now = now.Add(2 * time.Second)
		mu.Unlock()
		rig.waitFor(t, "DISCONNECTED", func(s string) bool { return strings.Contains(s, "DISCONNECTED") })
		if _, _, active := hub.stats(); active != 0 {
			t.Fatalf("disconnected: active timers = %d, want 0", active)
		}

		// Quitting leaves zero active timers.
		rig.sim.InjectKey(tcell.KeyRune, 'q', 0)
		if err := rig.wait(t); err != nil {
			t.Fatal(err)
		}
		if _, _, active := hub.stats(); active != 0 {
			t.Fatalf("after quit: active timers = %d, want 0", active)
		}
	})
}
