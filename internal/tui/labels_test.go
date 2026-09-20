package tui

import (
	"math"
	"testing"
	"time"
)

func TestBytesAndRate(t *testing.T) {
	cases := []struct {
		in        float64
		bytes     string
		rate      string
		rateShort string
	}{
		{0, "0 B", "0 B/s", "0 B/s"},
		{-5, "0 B", "0 B/s", "0 B/s"},
		{math.NaN(), "0 B", "0 B/s", "0 B/s"},
		{math.Inf(1), "0 B", "0 B/s", "0 B/s"},
		{512, "512 B", "512 B/s", "512 B/s"},
		{1023, "1023 B", "1023 B/s", "1023 B/s"},
		{1024, "1.0 KiB", "1.0 KiB/s", "1 KiB/s"},
		{1536, "1.5 KiB", "1.5 KiB/s", "1.5 KiB/s"},
		{41 * 1024 * 1024, "41.0 MiB", "41.0 MiB/s", "41 MiB/s"},
		{38.2 * 1024 * 1024, "38.2 MiB", "38.2 MiB/s", "38.2 MiB/s"},
		{1.8 * 1024 * 1024 * 1024, "1.8 GiB", "1.8 GiB/s", "1.8 GiB/s"},
		// Rounds to 1024.0 in KiB, so it must carry into MiB.
		{1024*1024 - 1, "1.0 MiB", "1.0 MiB/s", "1 MiB/s"},
		{math.Pow(1024, 6) * 3, "3072.0 PiB", "3072.0 PiB/s", "3072 PiB/s"},
	}
	for _, c := range cases {
		if got := Bytes(c.in); got != c.bytes {
			t.Errorf("Bytes(%v) = %q, want %q", c.in, got, c.bytes)
		}
		if got := Rate(c.in); got != c.rate {
			t.Errorf("Rate(%v) = %q, want %q", c.in, got, c.rate)
		}
		if got := RateShort(c.in); got != c.rateShort {
			t.Errorf("RateShort(%v) = %q, want %q", c.in, got, c.rateShort)
		}
	}
}

func TestDuration(t *testing.T) {
	day, hour := 24*time.Hour, time.Hour
	cases := []struct {
		in   time.Duration
		want string
	}{
		{-time.Minute, "0s"},
		{0, "0s"},
		{999 * time.Millisecond, "0s"},
		{5 * time.Second, "5s"},
		{65 * time.Second, "1m 5s"},
		{12 * time.Minute, "12m"},
		{4*hour + 12*time.Minute + 9*time.Second, "4h 12m"},
		{4 * hour, "4h"},
		{3*day + 4*hour + 30*time.Minute, "3d 4h"},
		{3*day + 5*time.Minute, "3d"},
		{400 * day, "400d"},
		{time.Hour + time.Second, "1h"},
	}
	for _, c := range cases {
		if got := Duration(c.in); got != c.want {
			t.Errorf("Duration(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestPercent(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0, "0.0%"}, {0.002, "0.2%"}, {0.0994, "9.9%"}, {0.0996, "10%"}, {0.1, "10%"},
		{0.213, "21%"}, {1, "100%"}, {1.7, "100%"}, {-0.5, "0.0%"}, {math.NaN(), "0.0%"},
		{math.Inf(1), "100%"},
	}
	for _, c := range cases {
		if got := Percent(c.in); got != c.want {
			t.Errorf("Percent(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}
