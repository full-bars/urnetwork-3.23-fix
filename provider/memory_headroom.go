package main

import "fmt"

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
