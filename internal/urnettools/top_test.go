package urnettools

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// topBase is the fixed wall clock the goldens are drawn at. UTC keeps them
// independent of the machine's zone.
var topBase = time.Date(2026, 9, 19, 16, 4, 31, 0, time.UTC)

// fakeClock is a clock the tests move by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// topSamples is a deterministic 10 minute series, one sample a second, that
// avoids math functions so goldens do not depend on floating point details.
func topSamples() []float64 {
	const mib = 1024 * 1024
	out := make([]float64, 600)
	seed := uint32(12345)
	for i := range out {
		phase := i % 240
		if phase > 120 {
			phase = 240 - phase
		}
		seed = seed*1664525 + 1013904223
		noise := int(seed>>24) % 6
		out[i] = float64(phase*mib/6+noise*mib/4) + 8*mib
	}
	return out
}

// topFlowing is the flowing fixture with a full ten minute history, so the
// graph has something to draw.
func topFlowing(t *testing.T) *NodeSnapshot {
	t.Helper()
	s := loadSnapshotFixture(t, "node_snapshot_v1.json")
	s.Rate.HistoryBps = topSamples()
	// Total traffic is billable plus what was not billable: a steady extra 60%.
	total := make([]float64, len(s.Rate.HistoryBps))
	for i, v := range s.Rate.HistoryBps {
		total[i] = v * 1.6
	}
	s.Traffic.TotalHistoryBps = total
	return s
}

func topIdle(t *testing.T) *NodeSnapshot {
	t.Helper()
	s := loadSnapshotFixture(t, "node_snapshot_v1_idle_minimal.json")
	s.Rate.HistoryBps = make([]float64, 600)
	return s
}

func topDegraded(t *testing.T) *NodeSnapshot {
	t.Helper()
	s := topFlowing(t)
	s.State = "degraded"
	s.Proxies = SnapshotProxies{Up: 20, Degraded: 18, Connecting: 6, Dead: 16}
	s.Pressure = 0.93
	s.RestartPending = true
	return s
}

func topProviders(n int) []Provider {
	names := []string{"tornado", "alpha", "bravo", "charlie"}
	out := make([]Provider, n)
	for i := range out {
		out[i] = Provider{User: "u", Network: names[i], StateDir: "/state/" + names[i], Running: true}
	}
	return out
}

func newTestModel(th tui.Theme, providers []Provider, clock *fakeClock) *topModel {
	m := newTopModel(providers, 0, topDefaultInterval, th, clock.Now)
	m.retry = topRetryDelay
	return m
}

// simText reads the simulated terminal as text, one line per row, with the
// trailing blanks trimmed the way Buffer.String does.
func simText(sim tcell.SimulationScreen) string {
	cells, w, h := sim.GetContents()
	var lines []string
	for y := 0; y < h; y++ {
		var row strings.Builder
		for x := 0; x < w; x++ {
			c := cells[y*w+x]
			if len(c.Runes) == 0 {
				continue // the second half of a wide rune, or never drawn
			}
			row.WriteString(string(c.Runes))
		}
		lines = append(lines, strings.TrimRight(row.String(), " "))
	}
	return strings.Join(lines, "\n") + "\n"
}

// showOnSim draws the model through the real adapter onto a simulation screen
// of the given size and returns what is on the terminal.
func showOnSim(t *testing.T, m *topModel, w, h int) (string, tcell.SimulationScreen) {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	scr, err := tcellui.Wrap(sim)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(scr.Close)
	sim.SetSize(w, h)
	b := tui.New(w, h)
	m.render(b)
	scr.Draw(b)
	return simText(sim), sim
}

func TestTopGoldenScreens(t *testing.T) {
	clock := &fakeClock{t: topBase}
	cases := []struct {
		name string
		w, h int
		th   tui.Theme
		set  func(m *topModel)
	}{
		{"top_flowing_120x40", 120, 40, tui.DefaultTheme(), func(m *topModel) { m.apply(0, topFlowing(t), nil) }},
		{"top_flowing_80x24", 80, 24, tui.DefaultTheme(), func(m *topModel) { m.apply(0, topFlowing(t), nil) }},
		{"top_flowing_mono_80x24", 80, 24, tui.MonoTheme(), func(m *topModel) { m.apply(0, topFlowing(t), nil) }},
		{"top_idle_100x30", 100, 30, tui.DefaultTheme(), func(m *topModel) { m.apply(0, topIdle(t), nil) }},
		{"top_degraded_100x30", 100, 30, tui.DefaultTheme(), func(m *topModel) { m.apply(0, topDegraded(t), nil) }},
		{"top_disconnected_100x30", 100, 30, tui.DefaultTheme(), func(m *topModel) {
			m.apply(0, topFlowing(t), nil)
			clock.Advance(20 * time.Second)
			m.apply(0, nil, errors.New("live status unavailable: dial unix /state/tornado/provider.sock: connect: no such file or directory"))
			clock.Advance(time.Second)
		}},
		{"top_never_connected_100x30", 100, 30, tui.DefaultTheme(), func(m *topModel) {
			m.apply(0, nil, errors.New("live status unavailable: dial unix /state/tornado/provider.sock: connect: no such file or directory"))
		}},
		{"top_connecting_100x30", 100, 30, tui.DefaultTheme(), func(m *topModel) {}},
		{"top_compact_flowing_50x12", 50, 12, tui.DefaultTheme(), func(m *topModel) { m.apply(0, topFlowing(t), nil) }},
		{"top_compact_idle_mono_40x8", 40, 8, tui.MonoTheme(), func(m *topModel) { m.apply(0, topIdle(t), nil) }},
		{"top_help_100x30", 100, 30, tui.DefaultTheme(), func(m *topModel) {
			m.apply(0, topFlowing(t), nil)
			m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: '?'})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock.t = topBase
			m := newTestModel(c.th, topProviders(1), clock)
			c.set(m)
			got, _ := showOnSim(t, m, c.w, c.h)
			assertGolden(t, c.name, got)
			for i, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
				if w := tui.StringWidth(line); w > c.w {
					t.Fatalf("row %d is %d cells in a %d wide screen", i, w, c.w)
				}
			}
		})
	}
}

// The default theme paints colored roles; the mono theme must not carry a
// single color or attribute through to the terminal.
func TestTopMonoThemeDrawsNoColor(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.MonoTheme(), topProviders(1), clock)
	m.apply(0, topDegraded(t), nil)
	_, sim := showOnSim(t, m, 100, 30)
	cells, _, _ := sim.GetContents()
	for i, c := range cells {
		fg, bg, attr := c.Style.Decompose()
		if fg != tcell.ColorDefault || bg != tcell.ColorDefault || attr != tcell.AttrNone {
			t.Fatalf("cell %d carries style fg=%v bg=%v attr=%v", i, fg, bg, attr)
		}
	}
	m2 := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m2.apply(0, topDegraded(t), nil)
	_, sim2 := showOnSim(t, m2, 100, 30)
	colored := false
	cells2, _, _ := sim2.GetContents()
	for _, c := range cells2 {
		if fg, _, _ := c.Style.Decompose(); fg != tcell.ColorDefault {
			colored = true
			break
		}
	}
	if !colored {
		t.Fatal("default theme should draw colors")
	}
}

func TestTopRenderAnySize(t *testing.T) {
	clock := &fakeClock{t: topBase}
	for _, mk := range []func(*topModel){
		func(m *topModel) {},
		func(m *topModel) { m.apply(0, topFlowing(t), nil) },
		func(m *topModel) { m.apply(0, topIdle(t), nil); m.help = true },
		func(m *topModel) { m.apply(0, nil, errors.New("x")) },
	} {
		for w := 0; w <= 130; w += 9 {
			for h := 0; h <= 45; h += 4 {
				m := newTestModel(tui.DefaultTheme(), topProviders(3), clock)
				mk(m)
				b := tui.New(w, h)
				m.render(b) // must not panic at any size
			}
		}
	}
}

func TestTopFullLayoutStartsAtTheNamedMinimum(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	full := tui.New(topMinWidth, topMinHeight)
	m.render(full)
	if !strings.Contains(full.String(), "Billable") {
		t.Fatalf("%dx%d should be the full layout:\n%s", topMinWidth, topMinHeight, full.String())
	}
	small := tui.New(topMinWidth-1, topMinHeight)
	m.render(small)
	if strings.Contains(small.String(), "Billable") || !strings.Contains(small.String(), "enlarge") {
		t.Fatalf("%dx%d should be the compact view:\n%s", topMinWidth-1, topMinHeight, small.String())
	}
}

// The graph is filled from the snapshot's own ring, so it is populated on the
// first frame with no local warmup.
func TestTopGraphIsPopulatedFromTheFirstSnapshot(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	b := tui.New(120, 40)
	m.render(b)
	if !strings.Contains(b.String(), "⣿") {
		t.Fatal("the braille graph should be drawn on the first frame")
	}
	if got := len(m.history()); got != 600 {
		t.Fatalf("history len = %d", got)
	}
}

func TestTopEventsFromSnapshotsAndTransitions(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	first := topFlowing(t)
	m.apply(0, first, nil)
	ev := m.sortedEvents()
	if len(ev) != 1 || ev[0].Text != "restart: update (v3.23.0-fix.32.0 to v3.23.0-fix.32.1)" {
		t.Fatalf("first snapshot should surface its restart, got %+v", ev)
	}
	if want := time.Date(2026, 9, 16, 12, 0, 0, 0, time.UTC); !ev[0].At.Equal(want) {
		t.Fatalf("restart event time = %v, want the provider start time %v", ev[0].At, want)
	}

	// flowing to idle
	clock.Advance(10 * time.Second)
	idle := topFlowing(t)
	idle.State = "idle"
	m.apply(0, idle, nil)
	// the provider goes away
	clock.Advance(10 * time.Second)
	m.apply(0, nil, errors.New("live status unavailable: connection refused"))
	// and comes back as a new process on a new version
	clock.Advance(30 * time.Second)
	back := topFlowing(t)
	back.StartedAt = "2026-09-19T16:04:50Z"
	back.UptimeSeconds = 10
	prev := "v3.23.0-fix.32.1"
	back.PreviousVersion, back.Version = &prev, "v3.23.0-fix.33.0"
	back.Restart.Reason = "hotswap"
	back.State = "starting"
	m.apply(0, back, nil)

	var got []string
	for _, e := range m.sortedEvents() {
		got = append(got, e.Text)
	}
	want := []string{
		"state idle to starting",
		"restart: hotswap (v3.23.0-fix.32.1 to v3.23.0-fix.33.0)",
		"reconnected after 30s",
		"lost the provider: connection refused",
		"state flowing to idle",
		"restart: update (v3.23.0-fix.32.0 to v3.23.0-fix.32.1)",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("events:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestTopEventsAreBounded(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	for i := 0; i < topMaxEvents*2; i++ {
		clock.Advance(time.Second)
		m.addEvent(clock.Now(), "x", topInfo)
	}
	if len(m.events) != topMaxEvents {
		t.Fatalf("events = %d, want %d", len(m.events), topMaxEvents)
	}
}

func TestTopFetchCadence(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.interval = time.Second

	gen, p, ok := m.wantFetch(clock.Now())
	if !ok || p.Network != "tornado" {
		t.Fatalf("the first tick should fetch at once (ok=%v p=%v)", ok, p)
	}
	if _, _, ok := m.wantFetch(clock.Now()); ok {
		t.Fatal("no second fetch while one is in flight")
	}
	m.apply(gen, topFlowing(t), nil)
	clock.Advance(500 * time.Millisecond)
	if _, _, ok := m.wantFetch(clock.Now()); ok {
		t.Fatal("fetched again before the interval passed")
	}
	clock.Advance(500 * time.Millisecond)
	gen, _, ok = m.wantFetch(clock.Now())
	if !ok {
		t.Fatal("should fetch once the interval has passed")
	}

	// A silent provider is retried on the countdown, not every tick.
	m.apply(gen, nil, errors.New("gone"))
	clock.Advance(time.Second)
	if _, _, ok := m.wantFetch(clock.Now()); ok {
		t.Fatal("retried before the retry delay")
	}
	if v, lvl := m.verdict(); v != "DISCONNECTED retry in 2s" || lvl != topBad {
		t.Fatalf("verdict = %q", v)
	}
	clock.Advance(topRetryDelay)
	if _, _, ok := m.wantFetch(clock.Now()); !ok {
		t.Fatal("should retry after the retry delay")
	}
}

func TestTopDropsResultsForAnEarlierSelection(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(2), clock)
	oldGen, _, _ := m.wantFetch(clock.Now())
	if eff := m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyTab}); eff != topRefetch {
		t.Fatalf("tab effect = %v", eff)
	}
	m.apply(oldGen, topFlowing(t), nil) // the slow answer for the provider left behind
	if m.snap != nil || m.conn != topConnecting {
		t.Fatal("a late result for the old selection was applied to the new one")
	}
	gen, p, ok := m.wantFetch(clock.Now())
	if !ok || p.Network != "alpha" || gen == oldGen {
		t.Fatalf("the new selection should be fetched at once (ok=%v p=%v gen=%d)", ok, p.Network, gen)
	}
}

func TestTopKeys(t *testing.T) {
	key := func(k tcellui.Key) tcellui.Event { return tcellui.Event{Kind: tcellui.EventKey, Key: k} }
	r := func(r rune) tcellui.Event { return tcellui.Event{Kind: tcellui.EventRune, Rune: r} }
	clock := &fakeClock{t: topBase}
	newM := func(n int) *topModel { return newTestModel(tui.DefaultTheme(), topProviders(n), clock) }

	for _, ev := range []tcellui.Event{r('q'), r('Q'), key(tcellui.KeyEsc), key(tcellui.KeyCtrlC), {Kind: tcellui.EventQuit}} {
		if newM(1).handle(ev) != topQuit {
			t.Errorf("%+v should quit", ev)
		}
	}
	for _, ev := range []tcellui.Event{r('x'), key(tcellui.KeyEnter), key(tcellui.KeyOther), {Kind: tcellui.EventResize, W: 1, H: 1}} {
		if newM(1).handle(ev) != topNone {
			t.Errorf("%+v should do nothing", ev)
		}
	}

	// Esc closes the help overlay before it quits.
	m := newM(1)
	m.handle(r('?'))
	if !m.help {
		t.Fatal("? should open help")
	}
	if m.handle(key(tcellui.KeyEsc)) != topNone || m.help {
		t.Fatal("Esc should close help, not quit")
	}
	if m.handle(key(tcellui.KeyEsc)) != topQuit {
		t.Fatal("Esc should quit from the main screen")
	}
	m = newM(1)
	m.handle(r('?'))
	m.handle(r('?'))
	if m.help {
		t.Fatal("? should toggle")
	}

	// Tab and Shift-Tab cycle and wrap; with one provider they do nothing.
	m = newM(3)
	var seq []string
	for _, ev := range []tcellui.Event{key(tcellui.KeyTab), key(tcellui.KeyTab), key(tcellui.KeyTab), key(tcellui.KeyBacktab), key(tcellui.KeyBacktab)} {
		if m.handle(ev) != topRefetch {
			t.Fatalf("%+v should ask for a refetch", ev)
		}
		seq = append(seq, m.provider().Network)
	}
	if strings.Join(seq, " ") != "alpha bravo tornado bravo alpha" {
		t.Fatalf("provider sequence = %v", seq)
	}
	if newM(1).handle(key(tcellui.KeyTab)) != topNone {
		t.Fatal("tab with one provider should do nothing")
	}
}

func TestTopRefreshRateKeys(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	press := func(r rune) { m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: r}) }

	press('+')
	press('+')
	press('+')
	press('+') // clamps at the fastest step, 100ms like btop
	if m.interval != topMinInterval {
		t.Fatalf("interval = %v, want the %v floor", m.interval, topMinInterval)
	}
	if topMinInterval != 100*time.Millisecond {
		t.Fatalf("floor = %v, want 100ms like btop", topMinInterval)
	}
	press('-')
	if m.interval != 250*time.Millisecond {
		t.Fatalf("interval = %v", m.interval)
	}
	press('-')
	if m.interval != 500*time.Millisecond {
		t.Fatalf("interval = %v", m.interval)
	}
	for i := 0; i < 10; i++ {
		press('-')
	}
	if m.interval != 10*time.Second {
		t.Fatalf("interval = %v, want the slowest step", m.interval)
	}

	// A custom --interval sits between steps and moves to the next one.
	m.interval = 1500 * time.Millisecond
	press('+')
	if m.interval != time.Second {
		t.Fatalf("interval = %v", m.interval)
	}
	m.interval = 1500 * time.Millisecond
	press('-')
	if m.interval != 2*time.Second {
		t.Fatalf("interval = %v", m.interval)
	}

	if got := topIntervalText(100 * time.Millisecond); got != "100ms" {
		t.Fatalf("interval text = %q", got)
	}
	if got := topIntervalText(250 * time.Millisecond); got != "250ms" {
		t.Fatalf("interval text = %q", got)
	}
	if got := topIntervalText(2 * time.Second); got != "2s" {
		t.Fatalf("interval text = %q", got)
	}
}

func TestParseTopFlags(t *testing.T) {
	cases := []struct {
		args     []string
		interval time.Duration
		rest     []string
		err      string
	}{
		{nil, time.Second, nil, ""},
		{[]string{"--interval", "500ms"}, 500 * time.Millisecond, nil, ""},
		{[]string{"--interval=2s", "--unit", "a.service"}, 2 * time.Second, []string{"--unit", "a.service"}, ""},
		{[]string{"--network", "n", "--interval", "3"}, 3 * time.Second, []string{"--network", "n"}, ""},
		{[]string{"--interval", "100ms"}, 100 * time.Millisecond, nil, ""},
		{[]string{"--interval", "50ms"}, 0, nil, "at least 100ms"},
		{[]string{"--interval", "fast"}, 0, nil, "not a duration"},
		{[]string{"--interval"}, 0, nil, "needs a value"},
	}
	for _, c := range cases {
		iv, rest, err := parseTopFlags(c.args)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: err = %v, want %q", c.args, err, c.err)
			}
			continue
		}
		if err != nil || iv != c.interval || strings.Join(rest, " ") != strings.Join(c.rest, " ") {
			t.Errorf("%v: got %v %v %v", c.args, iv, rest, err)
		}
	}
}

func TestWrapText(t *testing.T) {
	got := wrapText("all 58 proxies dead or connecting", 16, 2, false)
	// Three lines would be needed, so the second is cut off with an ellipsis.
	if strings.Join(got, "|") != "all 58 proxies|dead or ..." {
		t.Fatalf("wrap = %q", got)
	}
	if got := wrapText("all 58 proxies dead", 28, 2, false); strings.Join(got, "|") != "all 58 proxies dead" {
		t.Fatalf("wrap = %q", got)
	}
	if got := wrapText("a b c d e f g h", 3, 2, true); len(got) != 2 {
		t.Fatalf("wrap = %q", got)
	}
	if wrapText("x", 0, 2, false) != nil || wrapText("x", 5, 0, false) != nil {
		t.Fatal("no room should give no lines")
	}
}

// ---- the run loop, on a simulation screen ----

// closeCounter wraps a Screen to count Close calls and optionally panic in Draw.
// drawMu is held while drawing so a test can read the simulated terminal
// without racing tcell's own writes to it (GetContents hands out live storage).
type closeCounter struct {
	tcellui.Screen
	drawMu      sync.Mutex
	closes      atomic.Int32
	draws       atomic.Int32
	panicOnDraw int32
}

func (c *closeCounter) Close() { c.closes.Add(1); c.Screen.Close() }

func (c *closeCounter) Draw(b *tui.Buffer) {
	c.drawMu.Lock()
	defer c.drawMu.Unlock()
	if n := c.draws.Add(1); c.panicOnDraw > 0 && n >= c.panicOnDraw {
		panic("boom in draw")
	}
	c.Screen.Draw(b)
}

type loopRig struct {
	scr    *closeCounter
	sim    tcell.SimulationScreen
	done   chan error
	cancel context.CancelFunc
}

// startLoop runs runTop on a simulation screen with a fast tick.
func startLoop(t *testing.T, m *topModel, src topSource, w, h int) *loopRig {
	t.Helper()
	sim := tcell.NewSimulationScreen("UTF-8")
	real, err := tcellui.Wrap(sim)
	if err != nil {
		t.Fatal(err)
	}
	sim.SetSize(w, h)
	rig := &loopRig{scr: &closeCounter{Screen: real}, sim: sim, done: make(chan error, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	rig.cancel = cancel
	go func() { rig.done <- runTop(ctx, rig.scr, m, src, 5*time.Millisecond) }()
	t.Cleanup(func() { cancel(); real.Close() })
	return rig
}

// text reads the simulated terminal between frames.
func (r *loopRig) text() string {
	r.scr.drawMu.Lock()
	defer r.scr.drawMu.Unlock()
	return simText(r.sim)
}

func (r *loopRig) waitFor(t *testing.T, what string, cond func(text string) bool) string {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if txt := r.text(); cond(txt) {
			return txt
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s; screen:\n%s", what, r.text())
	return ""
}

func (r *loopRig) wait(t *testing.T) error {
	t.Helper()
	select {
	case err := <-r.done:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("runTop did not return")
	}
	return nil
}

func fakeSource(t *testing.T) (topSource, *atomic.Int32) {
	var calls atomic.Int32
	return topSourceFunc(func(p Provider) (*NodeSnapshot, error) {
		calls.Add(1)
		s := topFlowing(t)
		s.Clients = 100 + len(p.Network) // tells the providers apart on screen
		return s, nil
	}), &calls
}

func TestTopLoopQuitKeysRestoreTheTerminal(t *testing.T) {
	keys := []struct {
		name string
		key  tcell.Key
		r    rune
	}{
		{"q", tcell.KeyRune, 'q'},
		{"esc", tcell.KeyEsc, 0},
		{"ctrl-c", tcell.KeyCtrlC, 0},
	}
	for _, k := range keys {
		t.Run(k.name, func(t *testing.T) {
			clock := &fakeClock{t: topBase}
			src, _ := fakeSource(t)
			rig := startLoop(t, newTestModel(tui.DefaultTheme(), topProviders(1), clock), src, 100, 30)
			rig.waitFor(t, "FLOWING", func(s string) bool { return strings.Contains(s, "FLOWING") })
			rig.sim.InjectKey(k.key, k.r, 0)
			if err := rig.wait(t); err != nil {
				t.Fatal(err)
			}
			if rig.scr.closes.Load() < 1 {
				t.Fatal("the terminal was not given back on quit")
			}
		})
	}
}

func TestTopLoopShowsLiveDataThenResizesLive(t *testing.T) {
	clock := &fakeClock{t: topBase}
	src, _ := fakeSource(t)
	rig := startLoop(t, newTestModel(tui.DefaultTheme(), topProviders(1), clock), src, 120, 40)
	txt := rig.waitFor(t, "the full layout", func(s string) bool { return strings.Contains(s, "Resources") && strings.Contains(s, "FLOWING") })
	assertGolden(t, "top_loop_120x40", txt)

	// Shrinking below the minimum switches to the compact view without any key.
	rig.sim.SetSize(50, 12)
	if err := rig.sim.PostEvent(tcell.NewEventResize(50, 12)); err != nil {
		t.Fatal(err)
	}
	rig.waitFor(t, "the compact view", func(s string) bool { return strings.Contains(s, "enlarge") })
	// And growing back restores the full one.
	rig.sim.SetSize(90, 26)
	if err := rig.sim.PostEvent(tcell.NewEventResize(90, 26)); err != nil {
		t.Fatal(err)
	}
	rig.waitFor(t, "the full layout again", func(s string) bool { return strings.Contains(s, "Resources") && !strings.Contains(s, "enlarge") })

	rig.sim.InjectKey(tcell.KeyRune, 'q', 0)
	if err := rig.wait(t); err != nil {
		t.Fatal(err)
	}
}

func TestTopLoopSwitchesProviderWithTab(t *testing.T) {
	clock := &fakeClock{t: topBase}
	src, _ := fakeSource(t)
	rig := startLoop(t, newTestModel(tui.DefaultTheme(), topProviders(3), clock), src, 100, 30)
	// tornado is 7 letters, alpha 5, bravo 5: clients 107, 105, 105.
	rig.waitFor(t, "first provider", func(s string) bool { return strings.Contains(s, "[1/3]") && strings.Contains(s, "107") })
	rig.sim.InjectKey(tcell.KeyTab, 0, 0)
	txt := rig.waitFor(t, "second provider", func(s string) bool { return strings.Contains(s, "[2/3]") && strings.Contains(s, "FLOWING") })
	if !strings.Contains(txt, "alpha") || !strings.Contains(txt, "105") {
		t.Fatalf("second provider not shown:\n%s", txt)
	}
	rig.sim.InjectKey(tcell.KeyBacktab, 0, tcell.ModShift)
	rig.waitFor(t, "back to the first", func(s string) bool { return strings.Contains(s, "[1/3]") && strings.Contains(s, "107") })
	rig.sim.InjectKey(tcell.KeyRune, 'q', 0)
	if err := rig.wait(t); err != nil {
		t.Fatal(err)
	}
}

func TestTopLoopShowsDisconnectedAndResumesOnItsOwn(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	src := topSourceFunc(func(Provider) (*NodeSnapshot, error) {
		if down.Load() {
			return nil, errors.New("live status unavailable: connection refused")
		}
		return topFlowing(t), nil
	})
	m := newTopModel(topProviders(1), 0, topMinInterval, tui.DefaultTheme(), time.Now)
	m.retry = 30 * time.Millisecond
	rig := startLoop(t, m, src, 100, 30)
	rig.waitFor(t, "DISCONNECTED", func(s string) bool { return strings.Contains(s, "DISCONNECTED retry in") })
	down.Store(false)
	txt := rig.waitFor(t, "FLOWING", func(s string) bool { return strings.Contains(s, "FLOWING") })
	if strings.Contains(txt, "DISCONNECTED") {
		t.Fatalf("still disconnected after the provider came back:\n%s", txt)
	}
	rig.sim.InjectKey(tcell.KeyRune, 'q', 0)
	if err := rig.wait(t); err != nil {
		t.Fatal(err)
	}
}

// A fetch that panics must not take the process down with the terminal still
// in raw mode: it becomes a lost provider on screen.
func TestTopLoopSurvivesAPanickingSource(t *testing.T) {
	src := topSourceFunc(func(Provider) (*NodeSnapshot, error) { panic("source blew up") })
	rig := startLoop(t, newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase}), src, 100, 30)
	rig.waitFor(t, "a lost provider", func(s string) bool { return strings.Contains(s, "source blew up") })
	rig.sim.InjectKey(tcell.KeyRune, 'q', 0)
	if err := rig.wait(t); err != nil {
		t.Fatal(err)
	}
}

// The terminal must be restored when the screen code itself panics: Close runs
// and the panic comes back as an error carrying the stack, not a crash.
func TestTopLoopRestoresTheTerminalOnPanic(t *testing.T) {
	src, _ := fakeSource(t)
	sim := tcell.NewSimulationScreen("UTF-8")
	real, err := tcellui.Wrap(sim)
	if err != nil {
		t.Fatal(err)
	}
	scr := &closeCounter{Screen: real, panicOnDraw: 3}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	err = runTop(context.Background(), scr, m, src, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "boom in draw") || !strings.Contains(err.Error(), "goroutine") {
		t.Fatalf("err = %v, want the panic with a stack", err)
	}
	if scr.closes.Load() != 1 {
		t.Fatalf("Close called %d times on the panic path, want exactly 1", scr.closes.Load())
	}
	// The wrapped simulation screen really was finalized.
	if w, h := sim.Size(); w != 0 || h != 0 {
		t.Fatalf("simulation screen still has size %dx%d after the panic", w, h)
	}
}

func TestTopLoopStopsOnContextCancel(t *testing.T) {
	src, _ := fakeSource(t)
	rig := startLoop(t, newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase}), src, 100, 30)
	rig.waitFor(t, "FLOWING", func(s string) bool { return strings.Contains(s, "FLOWING") })
	rig.cancel()
	if err := rig.wait(t); err != nil {
		t.Fatal(err)
	}
	if rig.scr.closes.Load() < 1 {
		t.Fatal("the terminal was not restored when the context was cancelled")
	}
}

func TestTopLoopEndsWhenTheScreenGoesAway(t *testing.T) {
	src, _ := fakeSource(t)
	rig := startLoop(t, newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase}), src, 100, 30)
	rig.waitFor(t, "FLOWING", func(s string) bool { return strings.Contains(s, "FLOWING") })
	rig.scr.Screen.Close() // for example the terminal was closed under us
	if err := rig.wait(t); err != nil {
		t.Fatal(err)
	}
}
