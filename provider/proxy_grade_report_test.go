package main

import (
	"testing"
	"time"
)

// proxyGradeFor is the pure seam for hub grade surfacing: given both grade
// stores, it returns the grade payload for an address. Paid/file (proxy.state)
// wins over URL (proxy_url.json) when both are graded; ok=false when neither
// store has a graded entry for the address.

func TestProxyGradeFor_PaidWinsWhenBothGraded(t *testing.T) {
	paid := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.2.3.4:1080": {Score: 0.95, Graded: true, Failed: []string{"a.com"}},
	}}
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {Score: 0.82, Graded: true},
	}}

	info, ok := proxyGradeFor("1.2.3.4:1080", paid, url)
	if !ok {
		t.Fatal("expected ok=true when both stores graded")
	}
	if info.Score != 0.95 {
		t.Errorf("Score = %f, want paid store's 0.95", info.Score)
	}
	if info.Tier != "A" {
		t.Errorf("Tier = %q, want A for 0.95", info.Tier)
	}
	if len(info.Failed) != 1 || info.Failed[0] != "a.com" {
		t.Errorf("Failed = %v, want paid store's [a.com]", info.Failed)
	}
}

func TestProxyGradeFor_URLUsedWhenPaidNotGraded(t *testing.T) {
	paid := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.2.3.4:1080": {Score: 0.0, Graded: false}, // exists but never graded
	}}
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {Score: 0.71, Graded: true},
	}}

	info, ok := proxyGradeFor("1.2.3.4:1080", paid, url)
	if !ok {
		t.Fatal("expected ok=true when URL store graded")
	}
	if info.Score != 0.71 {
		t.Errorf("Score = %f, want URL store's 0.71", info.Score)
	}
	if info.Tier != "C" {
		t.Errorf("Tier = %q, want C for 0.71", info.Tier)
	}
}

func TestProxyGradeFor_NeitherGraded(t *testing.T) {
	paid := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.2.3.4:1080": {Score: 0.0, Graded: false},
	}}
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"9.9.9.9:1080": {Score: 0.9, Graded: true}, // different address
	}}

	if _, ok := proxyGradeFor("1.2.3.4:1080", paid, url); ok {
		t.Fatal("expected ok=false when neither store has a graded entry")
	}
}

func TestProxyGradeFor_PaidLastGradedUnix(t *testing.T) {
	gradedAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	paid := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.2.3.4:1080": {Score: 0.99, Graded: true, LastGraded: gradedAt},
	}}

	info, ok := proxyGradeFor("1.2.3.4:1080", paid, &ProxyURLState{})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if info.LastGraded != gradedAt.Unix() {
		t.Errorf("LastGraded = %d, want %d", info.LastGraded, gradedAt.Unix())
	}
}

func TestProxyGradeFor_URLNoLastGradedReportsZero(t *testing.T) {
	// A graded URL entry with a LastProbe but no LastGraded (e.g. a
	// pre-migration proxy_url.json, or liveness-only bumps) must report
	// last_graded = 0 — the honest "grade never re-recorded" answer, not
	// a liveness timestamp masquerading as a grade time.
	lastProbe := time.Date(2026, 8, 10, 13, 30, 0, 0, time.UTC)
	url := &ProxyURLState{Cache: map[string]ProxyURLEntry{
		"1.2.3.4:1080": {Score: 0.61, Graded: true, LastProbe: lastProbe},
	}}

	info, ok := proxyGradeFor("1.2.3.4:1080", &ProxyState{}, url)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if info.LastGraded != 0 {
		t.Errorf("LastGraded = %d, want 0 (no grade timestamp recorded)", info.LastGraded)
	}
	if info.Tier != "D" {
		t.Errorf("Tier = %q, want D for 0.61", info.Tier)
	}
}

func TestProxyGradeFor_TierMapping(t *testing.T) {
	cases := []struct {
		score float64
		want  string
	}{
		{1.00, "A"},
		{0.90, "A"},
		{0.89, "B"},
		{0.80, "B"},
		{0.79, "C"},
		{0.70, "C"},
		{0.69, "D"},
		{0.60, "D"},
		{0.59, "F"},
		{0.00, "F"},
	}
	for _, c := range cases {
		paid := &ProxyState{Proxies: map[string]ProxyEntry{
			"1.2.3.4:1080": {Score: c.score, Graded: true},
		}}
		info, ok := proxyGradeFor("1.2.3.4:1080", paid, &ProxyURLState{})
		if !ok {
			t.Fatalf("score %.2f: expected ok=true", c.score)
		}
		if info.Tier != c.want {
			t.Errorf("score %.2f: Tier = %q, want %q", c.score, info.Tier, c.want)
		}
	}
}

func TestProxyGradeFor_ZeroLastGraded(t *testing.T) {
	// A graded entry with a zero LastGraded/LastProbe must report 0, not a
	// negative epoch or a panic.
	paid := &ProxyState{Proxies: map[string]ProxyEntry{
		"1.2.3.4:1080": {Score: 0.9, Graded: true}, // LastGraded zero value
	}}
	info, ok := proxyGradeFor("1.2.3.4:1080", paid, &ProxyURLState{})
	if !ok {
		t.Fatal("expected ok=true")
	}
	if info.LastGraded != 0 {
		t.Errorf("LastGraded = %d, want 0 for zero-value timestamp", info.LastGraded)
	}
}
