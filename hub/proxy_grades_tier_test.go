package main

import (
	"testing"
	"time"
)

// Final-review HIGH regression: the tier field is attacker-influenced
// (any authenticated node's report body) and is rendered unescaped into the
// dashboard innerHTML. It must be allowlisted to exactly A-F on ingest —
// a crafted tier value must never reach proxy_grades.

func TestPersistRejectsInvalidTier(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)

	// XSS payload as tier — must be rejected, no grade row stored.
	s.upsert("n1", &nodeState{
		NodeID: "n1", Timestamp: now,
		Proxies: []proxyReport{
			{
				ID: "p1", Address: "1.2.3.4:1080",
				Tier: `"><img src=x onerror=alert(1)>`, Graded: true, Score: 0.9,
			},
		},
	})

	var cnt int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM proxy_grades`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 {
		t.Fatalf("proxy_grades rows = %d, want 0 (crafted tier must be rejected)", cnt)
	}
}

func TestPersistAcceptsValidTiers(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)

	for i, tier := range []string{"A", "B", "C", "D", "F"} {
		s.upsert("n1", &nodeState{
			NodeID: "n1", Timestamp: now,
			Proxies: []proxyReport{
				{
					ID: "p" + string(rune('a'+i)), Address: "10.0.0." + string(rune('1'+i)) + ":1080",
					Tier: tier, Graded: true, Score: 0.9,
				},
			},
		})
	}

	var cnt int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM proxy_grades`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 5 {
		t.Fatalf("proxy_grades rows = %d, want 5 (all valid tiers accepted)", cnt)
	}
}

func TestValidGradeTier(t *testing.T) {
	cases := []struct {
		tier string
		want bool
	}{
		{"A", true}, {"B", true}, {"C", true}, {"D", true}, {"F", true},
		{"", false}, {"E", false}, {"a", false}, {"AA", false},
		{`"><img src=x>`, false}, {" ", false},
	}
	for _, c := range cases {
		if got := validGradeTier(c.tier); got != c.want {
			t.Errorf("validGradeTier(%q) = %v, want %v", c.tier, got, c.want)
		}
	}
}
