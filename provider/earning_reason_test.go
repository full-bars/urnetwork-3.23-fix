package main

import "testing"

// earningReason powers the reason= field on the [profit] line: when billable
// traffic is NOT moving, it says *why* in one greppable token, so an operator
// can tell "no demand" apart from "no proxies" apart from "still warming up"
// without cross-referencing other lines. When earning, reason is "-".
func TestEarningReason(t *testing.T) {
	cases := []struct {
		name      string
		earning   bool
		proxiesUp int
		clients   int64
		warmup    bool
		want      string
	}{
		{"earning beats everything", true, 10, 4, false, "-"},
		{"earning even during warmup", true, 10, 4, true, "-"},
		{"warmup when not yet ramped", false, 0, 0, true, "warmup"},
		{"no proxies up", false, 0, 0, false, "no_proxies"},
		{"proxies up but no clients matched", false, 12, 0, false, "idle"},
		{"clients present but no billable bytes", false, 12, 3, false, "no_traffic"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := earningReason(c.earning, c.proxiesUp, c.clients, c.warmup, false); got != c.want {
				t.Fatalf("earningReason(earning=%v up=%d clients=%d warmup=%v) = %q, want %q",
					c.earning, c.proxiesUp, c.clients, c.warmup, got, c.want)
			}
		})
	}
}

func TestEarningReasonDirectOnly(t *testing.T) {
	for _, tc := range []struct {
		earning bool
		clients int64
		warmup  bool
		want    string
	}{
		{false, 2, false, "no_traffic"}, {false, 0, false, "idle"},
		{false, 2, true, "warmup"}, {true, 2, false, "-"},
	} {
		if got := earningReason(tc.earning, 0, tc.clients, tc.warmup, true); got != tc.want {
			t.Errorf("earningReason(%+v, directUp=true) = %q", tc, got)
		}
	}
}
