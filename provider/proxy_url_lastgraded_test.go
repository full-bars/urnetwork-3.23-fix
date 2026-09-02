package main

import (
	"testing"
	"time"
)

// URL-store last_graded honesty:
// ProxyURLEntry gains a real LastGraded field, stamped ONLY when a genuine
// stage-1 grade lands. The old proxyGradeFor borrowed LastProbe, which is
// also bumped by liveness-only re-checks (fetch re-encounters, reaper
// demotions) — so a URL proxy graded weeks ago but passing cheap liveness
// since showed a misleadingly fresh last_graded on the hub dashboard.

func TestApplyProxyGradeToEntryStampsLastGraded(t *testing.T) {
	gradedAt := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	entry := ProxyURLEntry{}
	g := proxyURLGrade{
		Decidable:  true,
		Socks5Only: false,
		Score:      0.92,
		Failed:     []string{"a.com"},
	}

	applyProxyGradeToEntry(&entry, g, gradedAt)

	if !entry.Graded {
		t.Fatal("expected entry graded after a decidable pass")
	}
	if entry.Score != 0.92 {
		t.Errorf("Score = %f, want 0.92", entry.Score)
	}
	if !entry.LastGraded.Equal(gradedAt) {
		t.Errorf("LastGraded = %v, want %v", entry.LastGraded, gradedAt)
	}
}

func TestApplyProxyGradeToEntrySocks5OnlyDoesNotStamp(t *testing.T) {
	entry := ProxyURLEntry{LastGraded: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)}
	g := proxyURLGrade{Decidable: false, Socks5Only: true}

	// A socks5-only (or undecidable) probe is NOT a grade — it must not
	// advance LastGraded, or the dashboard would show a fresh timestamp
	// for a proxy whose grade never actually changed.
	applyProxyGradeToEntry(&entry, g, time.Now())

	if entry.Graded {
		t.Fatal("socks5-only probe must not set Graded")
	}
	if !entry.LastGraded.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("LastGraded advanced on a non-grade probe: %v", entry.LastGraded)
	}
}

func TestProxyGradeForURLLastGradedFromFieldNotLastProbe(t *testing.T) {
	gradedAt := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	// LastProbe is FRESH (recent liveness check), LastGraded is OLD (weeks
	// ago) — exactly the misleading case the MEDIUM describes.
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {
			Score: 0.61, Graded: true,
			LastProbe:  time.Now(),
			LastGraded: gradedAt,
		},
	}}

	info, ok := proxyGradeFor("1.2.3.4:1080", &ProxyState{}, url)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if info.LastGraded != gradedAt.Unix() {
		t.Errorf("LastGraded = %d, want %d (must use the grade timestamp, not LastProbe)",
			info.LastGraded, gradedAt.Unix())
	}
}

func TestProxyGradeForURLZeroLastGraded(t *testing.T) {
	// A graded URL entry with no LastGraded recorded (e.g. a pre-migration
	// proxy_url.json) must report 0, not fall back to a liveness timestamp.
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {
			Score: 0.71, Graded: true,
			LastProbe: time.Now(), // fresh liveness, no grade record
		},
	}}

	info, ok := proxyGradeFor("1.2.3.4:1080", &ProxyState{}, url)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if info.LastGraded != 0 {
		t.Errorf("LastGraded = %d, want 0 when no grade timestamp is recorded", info.LastGraded)
	}
}
