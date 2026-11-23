package urnettools

import (
	"errors"
	"testing"
	"time"
)

func TestTopLingerReadings(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ps := []Provider{{User: "alice", Unit: "a.service"}, {User: "alice", Unit: "b.service"}, {User: "bob", Unit: "c.service"}, {User: "skip"}}
	got := collectTopLinger(ps, nil, now, func(p Provider) bool { return p.Unit != "" }, func(user string) (bool, error) {
		if user == "bob" {
			return false, errors.New("unavailable")
		}
		return false, nil
	})
	if len(got) != 1 || got[0].User != "alice" || got[0].Enabled || !got[0].CheckedAt.Equal(now) {
		t.Fatalf("unexpected readings: %+v", got)
	}
}

// A check that fails keeps the reading top already has, a check that succeeds
// replaces it, and an account that is no longer discovered is dropped.
func TestTopLingerFailedCheckKeepsTheLastReading(t *testing.T) {
	earlier := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := earlier.Add(time.Minute)
	prev := []topLingerReading{
		{User: "alice", Enabled: false, CheckedAt: earlier},
		{User: "carol", Enabled: false, CheckedAt: earlier},
	}
	ps := []Provider{{User: "alice", Unit: "a.service"}, {User: "bob", Unit: "b.service"}}
	got := collectTopLinger(ps, prev, now, func(Provider) bool { return true }, func(user string) (bool, error) {
		switch user {
		case "alice":
			return false, errors.New("unavailable")
		case "bob":
			return true, nil
		}
		return false, errors.New("unknown user")
	})
	want := map[string]topLingerReading{
		"alice": {User: "alice", Enabled: false, CheckedAt: earlier},
		"bob":   {User: "bob", Enabled: true, CheckedAt: now},
	}
	if len(got) != len(want) {
		t.Fatalf("readings = %+v, want %+v", got, want)
	}
	for _, r := range got {
		w, ok := want[r.User]
		if !ok || !r.CheckedAt.Equal(w.CheckedAt) || r.Enabled != w.Enabled {
			t.Fatalf("reading %+v, want %+v", r, w)
		}
	}
}

// Off then a failed check keeps the banner, off then a successful on clears it,
// and a stale revision is dropped.
func TestLingerFailedCheckKeepsTheBanner(t *testing.T) {
	m, _ := lingerModel(t, false)
	applicable := func(Provider) bool { return true }
	read := func(answer func(string) (bool, error)) {
		t.Helper()
		m.applyLinger(collectTopLinger(m.providers, m.linger, m.now(), applicable, answer), m.lingerRev)
	}
	read(func(string) (bool, error) { return false, errors.New("unavailable") })
	if !m.lingerBannerActive() {
		t.Fatal("a failed check dropped a reading that said off, so the banner went away")
	}
	read(func(string) (bool, error) { return true, nil })
	if m.lingerBannerActive() {
		t.Fatal("a check that said on left the banner up")
	}
	// A read that started before a fix carries the old answer and is dropped.
	before := m.linger
	m.applyLinger([]topLingerReading{{User: "alice", Enabled: false, CheckedAt: m.now()}}, m.lingerRev+1)
	if len(m.linger) != len(before) || m.linger[0].Enabled != before[0].Enabled {
		t.Fatalf("a stale reading was applied: %+v", m.linger)
	}
}
