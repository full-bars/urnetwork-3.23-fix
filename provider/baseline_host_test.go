package main

import (
	"os"
	"testing"
)

// Per brief #2074: mem_available_mib is the tighter of the host and cgroup
// readings, with avail_source naming which one supplied it and host winning a
// tie. In Docker /proc/meminfo shows the HOST's free RAM, so a container near
// its limit would otherwise read healthy and a memory regression would be
// invisible on exactly those boxes.
//
// These go through buildBaselineHost with synthetic readings rather than the
// real /proc readers, so every branch is exercised deterministically on any
// machine. The point of splitting the raw readings from the decision was to make
// that possible.
func TestBaselineHostAvailabilityTakesTheTighterReading(t *testing.T) {
	cases := []struct {
		name       string
		host, cg   int64
		wantValue  *int64
		wantSource string
	}{
		{"host only, no cgroup limit", 4096, -1, i64p(4096), "host"},
		{"cgroup only, host unreadable", -1, 512, i64p(512), "cgroup"},
		{"cgroup is tighter", 4096, 512, i64p(512), "cgroup"},
		{"host is tighter", 512, 4096, i64p(512), "host"},
		{"tie goes to host", 1024, 1024, i64p(1024), "host"},
		// The two cases that must NOT be treated as no-data: a real reading of
		// zero means memory is exhausted, and a cgroup limit of zero means the
		// container is at its cap. Both are the situation an operator needs to
		// see, so >= 0 rather than > 0.
		{"host zero is real data", 0, -1, i64p(0), "host"},
		{"cgroup zero beats a healthy host", 4096, 0, i64p(0), "cgroup"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, err := buildBaselineHost(baselineHostRaw{HostMiB: c.host, CgroupMiB: c.cg})
			if err != nil {
				t.Fatalf("buildBaselineHost returned %v, want success", err)
			}
			if h.MemAvailableMiB == nil {
				t.Fatal("mem_available_mib was omitted but a reading existed")
			}
			if *h.MemAvailableMiB != *c.wantValue {
				t.Errorf("mem_available_mib = %d, want %d", *h.MemAvailableMiB, *c.wantValue)
			}
			if h.AvailSource != c.wantSource {
				t.Errorf("avail_source = %q, want %q", h.AvailSource, c.wantSource)
			}
		})
	}
}

func TestBaselineHostAvailabilityOmitsBothFieldsWhenNeitherReaderWorks(t *testing.T) {
	// Both -1 means neither reader produced anything. The memory fields must be
	// omitted, never zeroed: a zero here would read as "memory exhausted".
	h, err := buildBaselineHost(baselineHostRaw{HostMiB: -1, CgroupMiB: -1})
	if err == nil {
		t.Error("buildBaselineHost returned success with no memory reading; the caller uses " +
			"this to decide whether to omit the host block entirely")
	}
	if h.MemAvailableMiB != nil {
		t.Errorf("mem_available_mib = %d, want omitted", *h.MemAvailableMiB)
	}
	if h.AvailSource != "" {
		t.Errorf("avail_source = %q, want empty", h.AvailSource)
	}
}

// The nit Claude raised in the review of 2da620e9d: readBaselineHost returned
// early when neither memory reader worked, so swap, PSI and load were dropped
// too. That is a box with an unreadable /proc/meminfo, which is exactly the
// situation someone is trying to diagnose, and losing the pressure numbers there
// is a silent gap.
func TestBaselineHostKeepsOtherReadingsWhenMemoryIsUnreadable(t *testing.T) {
	some, full60, full300, load, swap := 1.5, 2.5, 3.5, 0.9, int64(7)
	h, err := buildBaselineHost(baselineHostRaw{
		HostMiB: -1, CgroupMiB: -1,
		SwapUsed:   &swap,
		PSISome60:  &some,
		PSIFull60:  &full60,
		PSIFull300: &full300,
		Load1:      &load,
	})
	if err == nil {
		t.Error("want errNoHostData so the caller knows the memory fields are missing")
	}
	// The memory fields go...
	if h.MemAvailableMiB != nil || h.AvailSource != "" {
		t.Error("memory fields must be omitted when neither reader worked")
	}
	// ...but everything else must survive, independently.
	if h.SwapUsedMiB == nil || *h.SwapUsedMiB != 7 {
		t.Error("swap_used_mib was dropped with the memory reading")
	}
	if h.PSISomeAvg60 == nil || *h.PSISomeAvg60 != 1.5 {
		t.Error("psi_some_avg60 was dropped with the memory reading")
	}
	if h.PSIFullAvg60 == nil || *h.PSIFullAvg60 != 2.5 {
		t.Error("psi_full_avg60 was dropped with the memory reading")
	}
	if h.PSIFullAvg300 == nil || *h.PSIFullAvg300 != 3.5 {
		t.Error("psi_full_avg300 was dropped with the memory reading")
	}
	if h.Load1 == nil || *h.Load1 != 0.9 {
		t.Error("load1 was dropped with the memory reading")
	}
}

// A reader that failed contributes nothing, and must not blank out a sibling
// that succeeded. Each is an independent omission.
func TestBaselineHostOmitsEachFailedReaderIndependently(t *testing.T) {
	// Only swap came back.
	h, err := buildBaselineHost(baselineHostRaw{HostMiB: 2048, CgroupMiB: -1, SwapUsed: i64p(3)})
	if err != nil {
		t.Fatalf("err = %v, want nil (host memory worked)", err)
	}
	if h.SwapUsedMiB == nil {
		t.Error("the one successful reader was dropped")
	}
	for name, present := range map[string]bool{
		"psi_some_avg60":  h.PSISomeAvg60 != nil,
		"psi_full_avg60":  h.PSIFullAvg60 != nil,
		"psi_full_avg300": h.PSIFullAvg300 != nil,
		"load1":           h.Load1 != nil,
	} {
		if present {
			t.Errorf("%s was written although its reader never returned a value", name)
		}
	}
	if h.MemAvailableMiB == nil || *h.MemAvailableMiB != 2048 {
		t.Error("host memory should have been recorded")
	}
}

func TestParsePSIFullReadsAvg60AndAvg300(t *testing.T) {
	// The real /proc format. readPSI only parses the "some" line and only
	// avg60, so this reader had to be written separately; a parser that quietly
	// returned 0 for a missing field would hide a stall that is happening.
	content := "some avg10=0.00 avg60=0.10 avg300=0.20 total=1\n" +
		"full avg10=1.00 avg60=2.50 avg300=3.50 total=7\n"
	avg60, avg300, err := parsePSIFull(content)
	if err != nil {
		t.Fatalf("parsePSIFull: %v", err)
	}
	if avg60 != 2.5 || avg300 != 3.5 {
		t.Errorf("got avg60=%v avg300=%v, want 2.5 and 3.5", avg60, avg300)
	}
}

func TestParsePSIFullRejectsContentWithNoFullLine(t *testing.T) {
	// An error, not a silent zero: a box with no full-pressure support would
	// otherwise report avg60=0 and look perfectly healthy.
	if _, _, err := parsePSIFull("some avg10=0.00 avg60=0.10 total=1\n"); err == nil {
		t.Error("content with no 'full' line must be an error, not zeros")
	}
	if _, _, err := parsePSIFull(""); err == nil {
		t.Error("empty content must be an error, not zeros")
	}
}

// The unit tests feed parsePSIFull a string, which never noticed that the
// reader opened a file the kernel does not have: /proc/pressure/full does not
// exist. The "full" line lives in the resource files (memory, io), so on every
// real Linux box the two psi_full fields were silently omitted. This reads the
// real file, so it fails if the path is wrong; it skips only on a kernel with
// no PSI at all.
func TestReadPSIFullReadsTheRealKernelFile(t *testing.T) {
	if _, err := os.Stat("/proc/pressure/memory"); err != nil {
		t.Skip("this kernel has no PSI")
	}
	avg60, avg300, err := readPSIFull()
	if err != nil {
		t.Fatalf("readPSIFull failed on a kernel that has /proc/pressure/memory: %v", err)
	}
	if avg60 < 0 || avg300 < 0 {
		t.Errorf("avg60=%v avg300=%v, want non-negative", avg60, avg300)
	}
}
