package main

import (
	"fmt"
	"sync"
)

// memory_headroom.go watches real free memory and logs when it runs short and
// when it recovers. It is observation only: it changes nothing. The pressure
// score is not a usable signal for this (it read a fixed 1.00 on a healthy box
// and 0.00 on a box at 54% memory PSI), and connected clients, not just proxies,
// drive memory (about 0.7-0.9 MiB each), so the box can get short with no change
// in its proxy count.

const (
	headroomLowFloorMiB = 150
	headroomLowCapMiB   = 400
	headroomEnterCount  = 2 // consecutive low samples to announce
	headroomLeaveCount  = 4 // consecutive clear samples to announce recovery
)

// headroomLowThresholdMiB is the free-memory level below which a box is short:
// 10% of RAM, never below 150 MiB nor above 400 MiB.
func headroomLowThresholdMiB(ramMiB int64) int64 {
	return min(max(ramMiB/10, headroomLowFloorMiB), headroomLowCapMiB)
}

// headroomLowForRAMLimit resolves the low-headroom threshold for an effective
// RAM limit, and the one startup line that explains it when the limit was not a
// real reading. ramKnown reports whether ramBytes was measured (see
// effectiveRAMLimit); only a real reading is evidence of a small box, so only a
// real reading may take the 10% floor.
//
// The source decides this, not the byte count. When host memory cannot be read,
// connect.EffectiveRAMLimit returns an 850 MiB placeholder: 850 >> 20 = 850 and
// 10% of that is 85, which clamps to the 150 MiB floor. An 8 GiB host whose
// detection failed would then stay silent until 150 MiB free, well into swap --
// exactly the failure this guard exists to catch -- and a real 850 MiB host
// differs from the placeholder only in its source. For an unknown host the cap
// is the conservative choice: it is the highest threshold the guard ever uses,
// so it warns earliest.
func headroomLowForRAMLimit(ramBytes int64, ramKnown bool) (lowMiB int64, startupNote string) {
	if !ramKnown {
		return headroomLowCapMiB, fmt.Sprintf(
			"[proxy][resources] memory headroom threshold %d MiB: effective RAM unreadable, using the cap\n",
			headroomLowCapMiB,
		)
	}
	return headroomLowThresholdMiB(ramBytes >> 20), ""
}

// headroomAtStartup reads the effective RAM limit once and resolves the
// threshold the pressure monitor will use, plus the note explaining it.
func headroomAtStartup() (lowMiB int64, startupNote string) {
	ramBytes, ramKnown := effectiveRAMLimit()
	return headroomLowForRAMLimit(ramBytes, ramKnown)
}

// headroomStartupOnce makes the unreadable-RAM note fire once per process:
// runPressureMonitor resolves the threshold on every start and superviseLoop
// restarts that loop after a panic, so an unguarded line would repeat on every
// restart (the same reason resourceConfigWarnOnce exists).
var headroomStartupOnce sync.Once

// logHeadroomStartupNote emits the note through importantLogf, which writes both
// the ramlog and the persisted events.log. The line explains why the threshold
// is the cap, so it must survive a reboot alongside the low/recovered lines that
// are logged the same way; plain tlog would only reach /dev/shm. No-op when the
// RAM limit was readable.
func logHeadroomStartupNote(note string) {
	if note == "" {
		return
	}
	headroomStartupOnce.Do(func() {
		importantLogf("%s", note)
	})
}

// headroomTracker is the announce/recover state machine with hysteresis:
// entering takes two consecutive low samples; leaving takes four consecutive
// samples above the threshold by a 25% margin, so a reading hovering at the
// line does not flap.
type headroomTracker struct {
	low          bool
	below, clear int
}

// Observe feeds one free-memory reading (MiB; negative = unknown, ignored) and
// returns "low" or "recovered" on a transition, else "".
func (h *headroomTracker) Observe(availMiB, lowMiB int64) string {
	if availMiB < 0 {
		return ""
	}
	if !h.low {
		if availMiB < lowMiB {
			h.below++
			if h.below >= headroomEnterCount {
				h.low, h.below, h.clear = true, 0, 0
				return "low"
			}
		} else {
			h.below = 0
		}
		return ""
	}
	if availMiB*4 >= lowMiB*5 {
		h.clear++
		if h.clear >= headroomLeaveCount {
			h.low, h.below, h.clear = false, 0, 0
			return "recovered"
		}
	} else {
		h.clear = 0
	}
	return ""
}

// headroomLogLine formats the transition, or "" when there is none.
func headroomLogLine(transition string, availMiB, lowMiB int64, proxies, goroutines int) string {
	switch transition {
	case "low":
		return fmt.Sprintf("[proxy][resources] low memory headroom: %d MiB available, below %d MiB (%d proxies, %d goroutines); connected clients drive memory, so this can happen without any change in the proxy count. Consider `urnet-tools proxy trim` if it persists",
			availMiB, lowMiB, proxies, goroutines)
	case "recovered":
		return fmt.Sprintf("[proxy][resources] memory headroom recovered: %d MiB available (%d proxies, %d goroutines)", availMiB, proxies, goroutines)
	}
	return ""
}
