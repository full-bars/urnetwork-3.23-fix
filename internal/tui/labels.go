package tui

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Humanized values for the panels. They are pure functions of their input so
// a golden frame never depends on the clock or the locale.

var byteUnits = []string{"B", "KiB", "MiB", "GiB", "TiB", "PiB"}

func humanBytes(v float64, trim bool) string {
	if math.IsNaN(v) || math.IsInf(v, 0) || v <= 0 {
		return "0 B"
	}
	i := 0
	for v >= 1024 && i < len(byteUnits)-1 {
		v /= 1024
		i++
	}
	// 1023.96 KiB rounds up to "1024.0", which reads wrong; carry it to the
	// next unit instead.
	if i < len(byteUnits)-1 && math.Round(v*10)/10 >= 1024 {
		v /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f B", v)
	}
	s := fmt.Sprintf("%.1f", v)
	if trim {
		s = strings.TrimSuffix(s, ".0")
	}
	return s + " " + byteUnits[i]
}

// Bytes formats a byte count with binary units and one decimal: 1.5 KiB.
// Zero, negative and non-finite values read as "0 B".
func Bytes(v float64) string { return humanBytes(v, false) }

// Rate formats bytes per second: 38.2 MiB/s.
func Rate(v float64) string { return humanBytes(v, false) + "/s" }

// RateShort is Rate without a trailing ".0", for graph axis labels where
// width is tight: 41 MiB/s.
func RateShort(v float64) string { return humanBytes(v, true) + "/s" }

// Duration formats an elapsed time as its two most significant units:
// 3d 4h, 4h 12m, 12m 5s, 5s. Negative and sub-second values read as "0s".
// A zero second unit is dropped, so 3d 0h 5m is "3d".
func Duration(d time.Duration) string {
	secs := int64(d / time.Second)
	if secs <= 0 {
		return "0s"
	}
	parts := []struct {
		n    int64
		unit string
	}{
		{secs / 86400, "d"},
		{secs % 86400 / 3600, "h"},
		{secs % 3600 / 60, "m"},
		{secs % 60, "s"},
	}
	// The leading unit, plus the unit right after it when that is nonzero.
	for i, p := range parts {
		if p.n == 0 {
			continue
		}
		out := fmt.Sprintf("%d%s", p.n, p.unit)
		if i+1 < len(parts) && parts[i+1].n > 0 {
			out += fmt.Sprintf(" %d%s", parts[i+1].n, parts[i+1].unit)
		}
		return out
	}
	return "0s"
}

// Percent formats a fraction (0.213 is 21%). Values under 10% keep one
// decimal so small nonzero loads do not vanish into "0%". The input is clamped
// to 0..1 first, and NaN reads as 0%.
func Percent(frac float64) string {
	if math.IsNaN(frac) || frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	if p := frac * 100; p < 9.95 {
		return fmt.Sprintf("%.1f%%", p)
	}
	return fmt.Sprintf("%.0f%%", frac*100)
}
