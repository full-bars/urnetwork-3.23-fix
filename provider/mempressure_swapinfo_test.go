package main

import "testing"

// swapinfo -k output, single device (no Total row):
//
//	Device          1K-blocks     Used    Avail Capacity
//	/dev/ada0p8      1048576     12345  1036231    2%
func TestParseSwapinfoSingleDevice(t *testing.T) {
	out := []byte("Device          1K-blocks     Used    Avail Capacity\n" +
		"/dev/ada0p8      1048576     12345  1036231    2%\n")
	total, used, ok := parseSwapinfo(out)
	if !ok {
		t.Fatal("parseSwapinfo reported no row for a one-device table")
	}
	if total != 1048576 || used != 12345 {
		t.Errorf("got total=%d used=%d, want 1048576/12345", total, used)
	}
}

// Multiple devices get a trailing Total row that RESTATES the per-device sums.
// Counting it would double the total and halve the reported utilization.
func TestParseSwapinfoMultipleDevicesIgnoresTotalRow(t *testing.T) {
	out := []byte("Device          1K-blocks     Used    Avail Capacity\n" +
		"/dev/ada0p8       524288      1000    523288    0%\n" +
		"/dev/ada1p8       524288      2000    522288    0%\n" +
		"Total            1048576      3000   1045576    0%\n")
	total, used, ok := parseSwapinfo(out)
	if !ok {
		t.Fatal("parseSwapinfo reported no rows")
	}
	if total != 1048576 {
		t.Errorf("total = %d, want 1048576 (the Total row must not be double counted)", total)
	}
	if used != 3000 {
		t.Errorf("used = %d, want 3000", used)
	}
}

// No swap configured: swapinfo prints just the header. That is a legitimate
// state, not a parse failure, and must report "no reading" so the caller falls
// back rather than inventing pressure.
func TestParseSwapinfoEmptyTable(t *testing.T) {
	out := []byte("Device          1K-blocks     Used    Avail Capacity\n")
	if _, _, ok := parseSwapinfo(out); ok {
		t.Error("an empty table must report ok=false, not a zero reading")
	}
	for _, in := range []string{"", "\n\n", "garbage line\n", "Device Used\n"} {
		if _, _, ok := parseSwapinfo([]byte(in)); ok {
			t.Errorf("input %q must report ok=false", in)
		}
	}
}

// A "-" or blank cell means the kernel did not report that figure. Coercing it
// to zero would understate the denominator and could report a healthy box as
// fully swapped.
func TestParseSwapinfoSkipsUnparsableCells(t *testing.T) {
	out := []byte("Device          1K-blocks     Used    Avail Capacity\n" +
		"/dev/ada0p8           -         -         -     -\n" +
		"/dev/ada1p8      1048576      5000  1043576    0%\n")
	total, used, ok := parseSwapinfo(out)
	if !ok {
		t.Fatal("the one parsable row should still be used")
	}
	if total != 1048576 || used != 5000 {
		t.Errorf("got total=%d used=%d, want 1048576/5000; the '-' row must be skipped, not read as zero", total, used)
	}
}

// A fully used swap device must pin the component at the top of the ramp, and
// that path goes through the same parser.
func TestSwapinfoFullUsageMapsToMaxPressure(t *testing.T) {
	out := []byte("Device          1K-blocks     Used    Avail Capacity\n" +
		"/dev/ada0p8      1048576   1048576         0   100%\n")
	total, used, ok := parseSwapinfo(out)
	if !ok || total == 0 {
		t.Fatalf("parse failed: ok=%v total=%d", ok, total)
	}
	frac := float64(used) / float64(total)
	if s := normalizeRamp(memPressurePercentFromSwapFrac(frac), psiRampLo, psiRampHi); s != 1 {
		t.Errorf("a full swap device must pin psi_mem at 1, got %v", s)
	}
}

func TestParseUintStrict(t *testing.T) {
	for _, c := range []struct {
		in   string
		want uint64
		ok   bool
	}{
		{"0", 0, true},
		{"1048576", 1048576, true},
		{"", 0, false},
		{"-", 0, false},
		{"12a", 0, false},
		{" 5", 0, false},
		{"-1", 0, false},
	} {
		got, ok := parseUint(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("parseUint(%q) = %d,%v want %d,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}
