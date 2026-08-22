package main

import (
	"testing"
	"time"
)

// proxyAuthSlowRetryDelay must escalate 5m -> 10m -> 15m and cap at 15m, with
// at most 30s of jitter on top, for any attempt number (including <1).
func TestProxyAuthSlowRetryDelay(t *testing.T) {
	const maxJitter = 30 * time.Second
	cases := []struct {
		attempt int
		base    time.Duration
	}{
		{-3, 5 * time.Minute}, // clamped to >=1
		{0, 5 * time.Minute},  // clamped to >=1
		{1, 5 * time.Minute},
		{2, 10 * time.Minute},
		{3, 15 * time.Minute},
		{4, 15 * time.Minute},   // capped
		{100, 15 * time.Minute}, // capped
	}
	for _, c := range cases {
		for i := 0; i < 100; i++ {
			d := proxyAuthSlowRetryDelay(c.attempt)
			if d < c.base || d > c.base+maxJitter {
				t.Fatalf("attempt %d: got %v, want within [%v, %v]", c.attempt, d, c.base, c.base+maxJitter)
			}
		}
	}
}

func TestProxyURLGiveUpRetryDelay_Schedule(t *testing.T) {
	cases := []struct {
		giveUpCount int
		base        time.Duration
	}{
		{-3, 15 * time.Minute}, // clamped to >=1
		{0, 15 * time.Minute},  // clamped to >=1
		{1, 15 * time.Minute},
		{2, 30 * time.Minute},
		{3, time.Hour},
		{4, 2 * time.Hour},
		{5, 4 * time.Hour},
		{6, 8 * time.Hour},
		{7, 16 * time.Hour},
		{8, 24 * time.Hour},    // first cycle to hit the cap
		{9, 24 * time.Hour},    // capped
		{1000, 24 * time.Hour}, // capped
	}
	for _, c := range cases {
		for i := 0; i < 50; i++ {
			d := proxyURLGiveUpRetryDelay(c.giveUpCount)
			maxJitter := c.base / 5 // up to 20%
			if d < c.base || d > c.base+maxJitter {
				t.Fatalf("giveUpCount %d: got %v, want within [%v, %v]", c.giveUpCount, d, c.base, c.base+maxJitter)
			}
		}
	}
}
