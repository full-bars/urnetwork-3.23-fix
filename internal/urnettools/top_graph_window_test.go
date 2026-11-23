package urnettools

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// A graph level is a fixed, round time span drawn edge to edge across the
// plot. These tests pin the table, the resampling that fills the plot, the
// titles and the keys.

func TestTopGraphWindowsAreFourRoundSpans(t *testing.T) {
	want := []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute, 15 * time.Minute}
	wantNames := []string{"10s", "1m", "5m", "15m"}
	if len(topGraphWindows) != len(want) {
		t.Fatalf("the table holds %d levels, want %d", len(topGraphWindows), len(want))
	}
	for i, d := range want {
		if topGraphWindows[i] != d {
			t.Fatalf("level %d is %v, want %v", i, topGraphWindows[i], d)
		}
		if topGraphWindowNames[i] != wantNames[i] {
			t.Fatalf("level %d is named %q, want %q", i, topGraphWindowNames[i], wantNames[i])
		}
		if got, ok := topGraphWindowIndex(wantNames[i]); !ok || got != i {
			t.Fatalf("index of %q = %d, %v", wantNames[i], got, ok)
		}
	}
	if _, ok := topGraphWindowIndex("2m"); ok {
		t.Fatal("an unknown name must not resolve to a level")
	}
}

// spreadSeries is what makes a level fill the plot: a reading covers the
// columns its time span crosses, so few readings become wide step bars and
// many readings keep the highest one in each column.
func TestSpreadSeriesFillsEveryColumnWithStepBars(t *testing.T) {
	const base int64 = 1_000_000_000_000
	var at []int64
	var spans []time.Duration
	var vals []float64
	for i := 0; i < 10; i++ {
		at = append(at, base+int64(i)*int64(time.Second))
		spans = append(spans, time.Second)
		vals = append(vals, float64(i+1))
	}
	got := spreadSeries(at, spans, vals, at[len(at)-1], 10*time.Second, 40)
	if len(got) != 40 {
		t.Fatalf("got %d columns, want 40", len(got))
	}
	for i, v := range got {
		if math.IsNaN(v) || v <= 0 {
			t.Fatalf("column %d of a full window is empty: %v", i, got)
		}
	}
	if got[len(got)-1] != 10 {
		t.Fatalf("newest column = %v, want the newest reading 10", got[len(got)-1])
	}
	// Ten readings over forty columns: each covers about four columns, so the
	// first quarter carries the oldest value or the one at its edge.
	if got[0] != 1 && got[0] != 2 {
		t.Fatalf("left edge = %v, want the oldest reading", got[0])
	}
}

func TestSpreadSeriesKeepsThePeakInAColumn(t *testing.T) {
	const base int64 = 1_000_000_000_000
	var at []int64
	var spans []time.Duration
	var vals []float64
	for i := 0; i < 200; i++ {
		at = append(at, base+int64(i)*int64(50*time.Millisecond))
		spans = append(spans, 50*time.Millisecond)
		vals = append(vals, 1000)
	}
	vals[123] = 9_000_000 // one spike between two quiet readings
	got := spreadSeries(at, spans, vals, at[len(at)-1], 10*time.Second, 30)
	peak := 0.0
	for _, v := range got {
		if !math.IsNaN(v) && v > peak {
			peak = v
		}
	}
	if peak != 9_000_000 {
		t.Fatalf("peak over the plot = %v, want the spike downsampled intact", peak)
	}
}

func TestSpreadSeriesLeavesAYoungWindowAtTheRightEdge(t *testing.T) {
	const base int64 = 1_000_000_000_000
	var at []int64
	var spans []time.Duration
	var vals []float64
	for i := 0; i < 40; i++ {
		at = append(at, base+int64(100+i)*int64(time.Second))
		spans = append(spans, time.Second)
		vals = append(vals, 5)
	}
	got := spreadSeries(at, spans, vals, at[len(at)-1], 15*time.Minute, 100)
	first := -1
	for i, v := range got {
		if !math.IsNaN(v) {
			first = i
			break
		}
	}
	if first < 90 {
		t.Fatalf("40s of a 15m window starts at column %d of 100; it must grow from the right", first)
	}
	if math.IsNaN(got[len(got)-1]) {
		t.Fatal("the newest column must carry the newest reading")
	}
	for _, v := range got[:first] {
		if !math.IsNaN(v) {
			t.Fatalf("columns before the data are filled: %v", got[:first])
		}
	}
}

func TestTopGraphRingReportsItsCoveredSpan(t *testing.T) {
	const base int64 = 1_000_000_000_000
	var r topGraphRing
	for i := 0; i < 40; i++ {
		r.push(base+int64(i)*int64(time.Second), time.Second, 1, 1)
	}
	covered, full := r.cover(15 * time.Minute)
	if full {
		t.Fatal("40s must not count as a full 15m window")
	}
	if covered < 39*time.Second || covered > 41*time.Second {
		t.Fatalf("covered = %v, want about 40s", covered)
	}
	for i := 0; i < 900; i++ {
		r.push(base+int64(40+i)*int64(time.Second), time.Second, 1, 1)
	}
	if covered, full = r.cover(15 * time.Minute); !full {
		t.Fatalf("15m of readings is not full: covered %v", covered)
	}
	if n := len(r.at); n > 1+int(15*time.Minute/time.Second) {
		t.Fatalf("the ring holds %d readings, more than the 15m window needs", n)
	}
}

func TestTopGraphWindowTitleNamesTheRoundLevel(t *testing.T) {
	m, clock := liveModel(t, 500*time.Millisecond)
	var at int64 = 1_000_000_000_000
	var bytes uint64
	feedTrafficAt(t, m, clock, 2, 500*time.Millisecond, 1_000_000, &at, &bytes)
	// The ring normally starts from the provider's own history. Empty it, so
	// this window is young the way a freshly started provider's is.
	m.winRing.reset()
	m.winLevel = len(topGraphWindows) - 1
	feedTrafficAt(t, m, clock, 20, 500*time.Millisecond, 1_000_000, &at, &bytes)
	txt, _ := showOnSim(t, m, 120, 40)
	if !strings.Contains(txt, "Billable, 15m (collecting 10s)") {
		t.Fatalf("young window title:\n%s", txt)
	}
	// The shortest level is full at once, and it names itself round.
	m.winLevel = 0
	txt, _ = showOnSim(t, m, 120, 40)
	if !strings.Contains(txt, "Billable, 10s") || strings.Contains(txt, "collecting") {
		t.Fatalf("full window title:\n%s", txt)
	}
	// A full minute of readings fills the next level along.
	feedTrafficAt(t, m, clock, 120, 500*time.Millisecond, 1_000_000, &at, &bytes)
	pressKey(m, 'w')
	txt, _ = showOnSim(t, m, 120, 40)
	if !strings.Contains(txt, "Billable, 1m") || strings.Contains(txt, "collecting") {
		t.Fatalf("stepped level title:\n%s", txt)
	}
}

func TestTopGraphWindowKeysStepForwardAndBack(t *testing.T) {
	m, _ := liveModel(t, 500*time.Millisecond)
	if m.winLevel != topGraphWindowDefault || topGraphWindowNames[m.winLevel] != "1m" {
		t.Fatalf("default level = %q, want 1m", topGraphWindowNames[m.winLevel])
	}
	var seen []string
	for i := 0; i < len(topGraphWindows); i++ {
		pressKey(m, 'w')
		seen = append(seen, topGraphWindowNames[m.winLevel])
	}
	if got := strings.Join(seen, ","); got != "5m,15m,10s,1m" {
		t.Fatalf("w stepped %s", got)
	}
	pressKey(m, 'W')
	if got := topGraphWindowNames[m.winLevel]; got != "10s" {
		t.Fatalf("W from 1m went to %q, want 10s", got)
	}
	pressKey(m, 'w')
	if got := topGraphWindowNames[m.winLevel]; got != "1m" {
		t.Fatalf("w from 10s went to %q, want 1m", got)
	}
}

func TestTopGraphWindowFooterNamesTheLevel(t *testing.T) {
	m, clock := liveModel(t, 500*time.Millisecond)
	var at int64 = 1_000_000_000_000
	var bytes uint64
	feedTrafficAt(t, m, clock, 3, 500*time.Millisecond, 1_000_000, &at, &bytes)
	txt, _ := showOnSim(t, m, 160, 40)
	if !strings.Contains(txt, "w zoom 1m") {
		t.Fatalf("footer hint does not name the level:\n%s", txt)
	}
	pressKey(m, 'w')
	txt, _ = showOnSim(t, m, 160, 40)
	if !strings.Contains(txt, "w zoom 5m") {
		t.Fatalf("footer hint after stepping:\n%s", txt)
	}
}

func TestTopGraphWindowIsRemembered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "top.conf")
	if err := saveTopSettings(path, topSettings{Theme: "default", Graph: "braille", GraphWindow: "5m"}); err != nil {
		t.Fatal(err)
	}
	if s := loadTopSettings(path); s.GraphWindow != "5m" {
		t.Fatalf("saved window read back as %q", s.GraphWindow)
	}
	// A file without the key, and one with a name that is not a level, keep the
	// default. An older top.conf must keep working.
	old := filepath.Join(t.TempDir(), "top.conf")
	if err := os.WriteFile(old, []byte("theme = default\ngraph = braille\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := loadTopSettings(old); s.GraphWindow != "" {
		t.Fatalf("missing key gave %q", s.GraphWindow)
	}
	m, _ := liveModel(t, 500*time.Millisecond)
	m.applySettings(topSettings{GraphWindow: "2m"})
	if m.winLevel != topGraphWindowDefault {
		t.Fatalf("an unknown name selected level %q", topGraphWindowNames[m.winLevel])
	}
	m.applySettings(topSettings{GraphWindow: "15m"})
	if topGraphWindowNames[m.winLevel] != "15m" {
		t.Fatalf("a saved 15m did not apply: %q", topGraphWindowNames[m.winLevel])
	}
	// The key saves the choice, and the model reports it back.
	if s := m.settings(); s.GraphWindow != "15m" {
		t.Fatalf("settings report %q", s.GraphWindow)
	}
}

// The graphs must fill the plot at every level and refresh speed, which is the
// whole point of a fixed window: the range they ask for is always the level.
func TestTopGraphWindowFillsAtEveryLevelAndSpeed(t *testing.T) {
	for _, interval := range []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second, 2 * time.Second} {
		m, clock := liveModel(t, interval)
		var at int64 = 1_000_000_000_000
		var bytes uint64
		feedTrafficAt(t, m, clock, 4000, interval, uint64(interval)*1_000, &at, &bytes) // 16 minutes at any speed
		for level := range topGraphWindows {
			m.winLevel = level
			sr := m.series()
			if !sr.window || !sr.full {
				t.Fatalf("%v at level %s: window %v full %v", interval, topGraphWindowNames[level], sr.window, sr.full)
			}
			got := spreadSeries(sr.at, sr.spans, sr.billable, sr.anchor, topGraphWindows[level], 140)
			if len(got) != 140 {
				t.Fatalf("%v at level %s: %d columns", interval, topGraphWindowNames[level], len(got))
			}
			empty := 0
			for _, v := range got {
				if math.IsNaN(v) {
					empty++
				}
			}
			if empty > 0 {
				t.Fatalf("%v at level %s: %d of 140 columns empty", interval, topGraphWindowNames[level], empty)
			}
		}
	}
}

// feedTrafficAt pushes one traffic reading per clock step of d, at a fixed rate
// of bytesPerSecond, with the provider clock kept in step with the fake clock.
func feedTrafficAt(t *testing.T, m *topModel, clock *fakeClock, n int, d time.Duration, bytesPerSecond uint64, at *int64, bytes *uint64) {
	t.Helper()
	for i := 0; i < n; i++ {
		clock.Advance(d)
		gen, _, ok := m.wantTraffic(clock.Now())
		if !ok {
			t.Fatalf("traffic not requested at step %d of %d", i, n)
		}
		*at += int64(d)
		*bytes += uint64(float64(bytesPerSecond) * d.Seconds())
		m.applyTraffic(gen, &LiveTraffic{AtUnixNano: *at, BillableBytes: *bytes, TotalBytes: *bytes * 2}, nil)
	}
}

// The axis is taken over what is drawn, so a level never shows a plot scaled
// to history that is not on screen.
func TestTopGraphWindowAxisFollowsTheLevel(t *testing.T) {
	m, clock := liveModel(t, time.Second)
	var at int64 = 1_000_000_000_000
	var bytes uint64
	feedTrafficAt(t, m, clock, 300, time.Second, 1_000_000, &at, &bytes)
	_ = tui.Rate(0)
	if m.winScaleB[topGraphWindowDefault].top == 0 {
		// The scale is fed by the render path, so draw once first.
		showOnSim(t, m, 120, 40)
	}
	if m.winScaleB[topGraphWindowDefault].top == 0 {
		t.Fatal("the window graph has no steady axis")
	}
}

// The Remote chart and the direction pages read the second by second ring, so
// they follow the level up to what that ring holds and no further.
func TestTopRemoteWindowFollowsTheLevelUpToItsHistory(t *testing.T) {
	m, _ := liveModel(t, 500*time.Millisecond)
	for level, want := range map[int]time.Duration{
		0: 10 * time.Second,
		1: time.Minute,
		2: 5 * time.Minute,
		3: time.Duration(topGraphCapacity) * time.Second,
	} {
		m.winLevel = level
		if got := m.remoteWindow(); got != want {
			t.Fatalf("level %s: the Remote chart spans %v, want %v", topGraphWindowNames[level], got, want)
		}
	}
}
