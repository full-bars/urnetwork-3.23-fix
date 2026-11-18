package urnettools

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

func TestEventTime(t *testing.T) {
	now := time.Date(2026, 9, 19, 16, 4, 31, 0, time.UTC)
	cases := []struct {
		name string
		at   time.Time
		zoom bool
		want string
	}{
		{"just now", now.Add(-30 * time.Second), false, "just now"},
		{"minutes", now.Add(-12 * time.Minute), false, "12m ago"},
		{"hours", now.Add(-3 * time.Hour), false, "3h ago"},
		{"days", now.Add(-48 * time.Hour), false, "2d ago"},
		{"zoom today", now.Add(-12 * time.Minute), true, "15:52:31"},
		{"zoom other day", now.Add(-72 * time.Hour), true, "09-16 16:04"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := eventTime(c.at, now, c.zoom); got != c.want {
				t.Fatalf("eventTime = %q, want %q", got, c.want)
			}
		})
	}
}

func TestEventIcon(t *testing.T) {
	th := tui.DefaultTheme()
	if got := eventIcon(topGood, th); got != '✓' {
		t.Fatalf("good icon = %q", got)
	}
	if got := eventIcon(topGood, tui.MonoTheme()); got != '+' {
		t.Fatalf("ascii good icon = %q", got)
	}
	if got := eventIcon(topInfo, th); got != '•' {
		t.Fatalf("info icon = %q, want bullet", got)
	}
	if got := eventIcon(topWarn, th); got != '!' {
		t.Fatalf("warn icon = %q, want !", got)
	}
	if got := eventIcon(topBad, th); got != '✗' {
		t.Fatalf("bad icon = %q, want ✗", got)
	}
	mono := tui.MonoTheme()
	if got := eventIcon(topInfo, mono); got != 'i' {
		t.Fatalf("ascii info icon = %q, want i", got)
	}
	if got := eventIcon(topBad, mono); got != 'x' {
		t.Fatalf("ascii bad icon = %q, want x", got)
	}
	// the theme's own symbols win over the built-in ones
	custom := tui.DefaultTheme()
	custom.IconOK = 'o'
	if got := eventIcon(topInfo, custom); got != 'o' {
		t.Fatalf("custom icon = %q, want o", got)
	}
}

func TestCollapseEvents(t *testing.T) {
	at := time.Date(2026, 9, 19, 16, 4, 0, 0, time.UTC)
	events := []topEvent{
		{At: at, Text: "a", Level: topInfo},
		{At: at.Add(time.Second), Text: "a", Level: topInfo},
		{At: at.Add(2 * time.Second), Text: "a", Level: topInfo},
		{At: at.Add(3 * time.Second), Text: "b", Level: topWarn},
		{At: at.Add(4 * time.Second), Text: "a", Level: topInfo},
	}
	got := collapseEvents(events)
	if len(got) != 3 {
		t.Fatalf("collapsed %d rows, want 3: %+v", len(got), got)
	}
	// The input is newest first; the collapse keeps that order and only merges
	// consecutive repeats.
	want := []struct {
		text  string
		count int
	}{{"a", 3}, {"b", 1}, {"a", 1}}
	for i, w := range want {
		if got[i].Text != w.text || got[i].Count != w.count {
			t.Errorf("row %d = %q x%d, want %q x%d", i, got[i].Text, got[i].Count, w.text, w.count)
		}
	}
	// the newest time survives the collapse: the input is newest first, so the
	// first occurrence of the group is the newest one
	if !got[0].At.Equal(at) {
		t.Errorf("collapsed row keeps the newest time, got %v", got[0].At)
	}
}

func TestCollapseEventsEmptyAndSingle(t *testing.T) {
	if got := collapseEvents(nil); len(got) != 0 {
		t.Fatalf("nil input -> %+v", got)
	}
	at := time.Date(2026, 9, 19, 16, 4, 0, 0, time.UTC)
	got := collapseEvents([]topEvent{{At: at, Text: "x", Level: topWarn}})
	if len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("single event -> %+v", got)
	}
}

func TestFilterEvents(t *testing.T) {
	at := time.Date(2026, 9, 19, 16, 4, 0, 0, time.UTC)
	events := []topEvent{
		{At: at, Text: "provider slow", Level: topWarn},
		{At: at, Text: "restart: update", Level: topWarn},
		{At: at, Text: "connection refused", Level: topBad},
		{At: at, Text: "provider responsive", Level: topGood},
	}
	cases := []struct {
		name string
		min  topLevel
		text string
		want []string
	}{
		{"no filter", topInfo, "", []string{"provider slow", "restart: update", "connection refused", "provider responsive"}},
		{"warn and above", topWarn, "", []string{"provider slow", "restart: update", "connection refused"}},
		{"bad only", topBad, "", []string{"connection refused"}},
		{"text match", topInfo, "slow", []string{"provider slow"}},
		{"text case-insensitive", topInfo, "RESTART", []string{"restart: update"}},
		{"warn and text", topWarn, "provider", []string{"provider slow"}},
		{"no match", topInfo, "zzzzz", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := filterEvents(events, eventFilter{MinLevel: c.min, Text: c.text})
			var texts []string
			for _, e := range got {
				texts = append(texts, e.Text)
			}
			if strings.Join(texts, "|") != strings.Join(c.want, "|") {
				t.Fatalf("filter kept %v, want %v", texts, c.want)
			}
		})
	}
}

func TestScrollEvents(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	for i := 0; i < 8; i++ {
		clock.Advance(time.Second)
		m.addEvent(clock.Now(), fmt.Sprintf("event %d", i), topWarn)
	}
	n := len(collapseEvents(filterEvents(m.sortedEvents(), m.eventFilter)))
	m.eventHeight = 4

	m.scrollEvents(3)
	if m.eventScroll != 3 {
		t.Fatalf("scroll = %d, want 3", m.eventScroll)
	}
	m.scrollEvents(100)
	if m.eventScroll != max(n-4, 0) {
		t.Fatalf("scroll = %d, want %d", m.eventScroll, max(n-4, 0))
	}
	m.scrollEvents(-100)
	if m.eventScroll != 0 {
		t.Fatalf("scroll = %d, want 0", m.eventScroll)
	}
}
