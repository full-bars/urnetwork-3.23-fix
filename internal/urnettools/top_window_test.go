package urnettools

import (
	"math"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

var topWindowBase = time.Date(2026, 9, 19, 16, 0, 0, 0, time.UTC)

func TestWindowDownsamplingAverage(t *testing.T) {
	w := &topWindow{}
	now := topWindowBase
	for i := 0; i < 4; i++ {
		w.push(100, now)
		w.push(200, now)
	}
	got := w.windowFor(1, now.Add(59*time.Second))
	// The current minute is the newest slot; every older one is empty.
	if len(got) != 60 {
		t.Fatalf("windowFor(1) = %d slots, want 60", len(got))
	}
	if v := got[59]; v != 150 {
		t.Fatalf("current minute bucket = %v, want the average 150", v)
	}
	for i := 0; i < 59; i++ {
		if !math.IsNaN(got[i]) {
			t.Fatalf("slot %d = %v, want NaN for a bucket that never filled", i, got[i])
		}
	}
}

func TestWindowRingIsBounded(t *testing.T) {
	w := &topWindow{}
	now := topWindowBase
	// Two hundred minutes of samples: the minute ring holds 60 slots, so the
	// first 140 minutes must be gone and only the last 60 remain.
	for m := 0; m < 200; m++ {
		w.push(1, now.Add(time.Duration(m)*time.Minute))
	}
	got := w.windowFor(1, now.Add(199*time.Minute))
	if len(got) != 60 {
		t.Fatalf("windowFor(1) = %d slots, want 60", len(got))
	}
	for i, v := range got {
		if !math.IsNaN(v) && v != 1 {
			t.Fatalf("slot %d = %v, want 1", i, v)
		}
	}
	// minute 0's slot must now carry a much newer minute, not minute 0.
	i := int(mod(now.Unix()/60, int64(topMinuteBuckets)))
	if w.minuteIndex[i] == now.Unix()/60 {
		t.Fatal("minute 0 survived in a 60 slot ring")
	}
	// The quarter ring is bounded the same way: 96 slots, whatever the clock.
	w2 := &topWindow{}
	for m := 0; m < 100*24*4; m += 15 {
		w2.push(1, now.Add(time.Duration(m)*time.Minute))
	}
	got24 := w2.windowFor(24, now.Add(99*24*time.Hour))
	if len(got24) != 96 {
		t.Fatalf("windowFor(24) = %d slots, want 96", len(got24))
	}
	// a quarter from before the window must not be in it
	qIdx := now.Add(99*24*time.Hour).Unix()/900 - 97
	j := int(mod(qIdx, int64(topQuarterBuckets)))
	if w2.quarterIndex[j] == qIdx {
		t.Fatal("a quarter older than the window survived in a 96 slot ring")
	}
}

func TestWindowQuarterBuckets(t *testing.T) {
	w := &topWindow{}
	now := topWindowBase
	// One quarter of readings, then a gap, then today's quarter.
	for m := 0; m < 15; m++ {
		w.push(10, now.Add(time.Duration(m)*time.Minute))
	}
	for m := 30; m < 45; m++ {
		w.push(20, now.Add(time.Duration(m)*time.Minute))
	}
	got := w.windowFor(24, now.Add(44*time.Minute))
	if len(got) != 96 {
		t.Fatalf("windowFor(24) = %d slots, want 96", len(got))
	}
	// The filled quarters are the newest slot (now) and slot 93 (two back);
	// quarter 1 sits between them and never filled, so it is a gap.
	if v := got[95]; v != 20 {
		t.Fatalf("newest quarter bucket = %v, want 20", v)
	}
	if v := got[94]; !math.IsNaN(v) {
		t.Fatalf("quarter 1 bucket = %v, want NaN for a bucket that never filled", v)
	}
	if v := got[93]; v != 10 {
		t.Fatalf("quarter 0 bucket = %v, want 10", v)
	}
	for i := 0; i < 93; i++ {
		if !math.IsNaN(got[i]) {
			t.Fatalf("slot %d = %v, want NaN for a bucket that never filled", i, got[i])
		}
	}
}

func TestWindowTitle(t *testing.T) {
	cases := map[int]string{
		0:  "last 10 minutes",
		1:  "last 1 hour",
		24: "last 24 hours",
	}
	for h, want := range cases {
		if got := windowTitle(h); got != want {
			t.Errorf("windowTitle(%d) = %q, want %q", h, got, want)
		}
	}
}

func TestModelWindowFor(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	if got := m.windowFor(0); len(got) != len(m.history()) {
		t.Fatalf("windowFor(0) = %d samples, want the provider ring %d", len(got), len(m.history()))
	}
	m.windowHours = 1
	got := m.windowFor(1)
	if len(got) != 60 {
		t.Fatalf("windowFor(1) = %d slots, want 60", len(got))
	}
	if math.IsNaN(got[59]) {
		t.Fatal("the newest minute must hold the snapshot rate, not a gap")
	}
	m.windowHours = 24
	got = m.windowFor(24)
	if len(got) != 96 {
		t.Fatalf("windowFor(24) = %d slots, want 96", len(got))
	}
}

func TestModelSeriesShowsTheWindow(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	m.windowHours = 1
	sr := m.series()
	if len(sr.billable) != 60 {
		t.Fatalf("series = %d slots, want 60", len(sr.billable))
	}
	if title := m.throughputTitle("Billable", sr); title != "Billable, last 1 hour" {
		t.Fatalf("title = %q", title)
	}
	m.windowHours = 24
	sr = m.series()
	if title := m.throughputTitle("Billable", sr); title != "Billable, last 24 hours" {
		t.Fatalf("title = %q", title)
	}
	// The default window keeps the provider's own ring and title.
	m.windowHours = 0
	sr = m.series()
	if title := m.throughputTitle("Billable", sr); title != "Billable, 10m" {
		t.Fatalf("title = %q", title)
	}
}

func TestModelSelectProviderResetsTheWindows(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(2), clock)
	m.apply(0, topFlowing(t), nil)
	m.windowHours = 1
	got := m.windowFor(1)
	if math.IsNaN(got[59]) {
		t.Fatal("the newest minute must hold the snapshot rate")
	}
	m.selectProvider(1)
	if m.windowHours != 1 {
		t.Fatalf("the window choice must survive the switch, got %d", m.windowHours)
	}
	got = m.windowFor(1)
	for i, v := range got {
		if !math.IsNaN(v) {
			t.Fatalf("slot %d = %v after the switch, want NaN: the rings must reset", i, v)
		}
	}
}
