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
