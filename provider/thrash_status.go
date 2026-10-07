package main

// thrash_status.go holds the PORTABLE slice of the thrash responder: the
// published snapshot type, the atomic holding the latest one, the accessors
// the pressure-status writer reads, and the shared sentence helpers. The
// sensing loop lives in thrash_watchdog.go behind the linux build tag; this
// file must compile everywhere because resource_pressure.go and main.go
// reference these from every platform.

import (
	"fmt"
	"strings"
	"sync/atomic"
)

// thrashSnapshot is the published, aggregator-ready view (fixed field names;
// the deferred heal-status surface wraps these, it does not re-derive them).
type thrashSnapshot struct {
	State            string   `json:"state"`
	SinceUnix        int64    `json:"since_unix"`
	PSIFull          *float64 `json:"psi_mem_full"`      // stalled fraction 0..1; null when unavailable
	SomeFrac         *float64 `json:"some_stalled_frac"` // null when unavailable
	SwapIOPS         *float64 `json:"swap_io_pps"`       // pages/s (in+out); null when unavailable
	UnitSwapMiB      *int64   `json:"unit_swap_mib"`     // null when unavailable
	HostSwapUsedMiB  *int64   `json:"host_swap_used_mib"`
	HostSwapTotalMiB *int64   `json:"host_swap_total_mib"`
	RAMAvailMiB      *int64   `json:"ram_avail_mib"`
	RAMTotalMiB      *int64   `json:"ram_total_mib"`
	HeapFrac         *float64 `json:"heap_frac"`       // vs the soft limit
	Attribution      string   `json:"attribution"`     // unit | other | unknown
	UnitShare        *float64 `json:"unit_swap_share"` // null when unknown
	LastAction       string   `json:"last_action,omitempty"`
	Restarts24h      int      `json:"restarts_24h"`
	Summary          string   `json:"summary"`
	Updated          string   `json:"updated"`
}

var globalThrashSnap atomic.Pointer[thrashSnapshot]

func ptrF(v float64, ok bool) *float64 {
	if !ok {
		return nil
	}
	return &v
}
func ptrI(v int64, ok bool) *int64 {
	if !ok {
		return nil
	}
	return &v
}

// fmtMiBHuman renders MiB in human units: "3.1 GB" / "512 MB".
func fmtMiBHuman(mib int64) string {
	if mib >= 1024 {
		return fmt.Sprintf("%.1f GB", float64(mib)/1024)
	}
	return fmt.Sprintf("%d MB", mib)
}

// fmtSwapMBs renders a pages/sec rate as MB/s (4 KiB pages).
func fmtSwapMBs(pps float64) string {
	return fmt.Sprintf("%.0f MB/s", pps/256.0)
}

// Accessors for the pressure_status writer (atomic reads only).
func thrashStateName() string {
	if s := globalThrashSnap.Load(); s != nil {
		return s.State
	}
	return ""
}

func thrashPSIFullForStatus() (float64, bool) {
	if s := globalThrashSnap.Load(); s != nil && s.PSIFull != nil {
		return *s.PSIFull, true
	}
	return 0, false
}

func thrashSwapIOForStatus() (float64, bool) {
	if s := globalThrashSnap.Load(); s != nil && s.SwapIOPS != nil {
		return *s.SwapIOPS, true
	}
	return 0, false
}

// pressureSummaryOf renders the operator-facing summary for the
// pressure_status file: one sentence, human units, no decoding required
// (thrash states reuse the thrash snapshot's summary, which is written by
// the watchdog). The worst component names the driver in words. The calm/
// easing wording is decided from BOTH the smoothed score and the live
// components: the EWMA lags, so a smoothed-high score with calm components
// must read "easing", never "calm".
func pressureSummaryOf(score float64, comps map[string]float64) string {
	if s := globalThrashSnap.Load(); s != nil && s.Summary != "" &&
		(s.State == "thrashing" || s.State == "critical") {
		return fmt.Sprintf("memory pressure %.2f — %s", score, s.Summary)
	}
	best, bestV := "", 0.0
	for k, v := range comps {
		if v > bestV {
			best, bestV = k, v
		}
	}
	snap := globalThrashSnap.Load()
	var facts []string
	if snap != nil {
		if snap.HeapFrac != nil {
			facts = append(facts, fmt.Sprintf("heap at %.0f%% of its soft limit", *snap.HeapFrac*100))
		}
		if snap.RAMAvailMiB != nil {
			facts = append(facts, fmt.Sprintf("%s RAM free", fmtMiBHuman(*snap.RAMAvailMiB)))
		}
		if snap.SwapIOPS != nil && *snap.SwapIOPS > 0 {
			facts = append(facts, fmt.Sprintf("swapping %s", fmtSwapMBs(*snap.SwapIOPS)))
		}
	}
	joined := strings.Join(facts, "; ")
	if bestV < 0.5 {
		switch {
		case score < 0.5 && joined == "":
			return fmt.Sprintf("system calm (pressure %.2f)", score)
		case score < 0.5:
			return fmt.Sprintf("system calm (pressure %.2f); %s", score, joined)
		case joined == "":
			return fmt.Sprintf("pressure is easing (%.2f)", score)
		default:
			return fmt.Sprintf("pressure is easing (%.2f); %s", score, joined)
		}
	}
	driver, prefix := map[string]string{
		"heap":    "the program's heap is close to its soft limit",
		"mem":     "the machine is low on free RAM",
		"psi_mem": "tasks are stalling on memory",
		"psi_cpu": "tasks are stalling on CPU",
		"psi_io":  "tasks are stalling on disk IO",
		"goro":    "goroutine growth is high",
		"load":    "system load is high",
		"fd":      "file descriptors are getting scarce",
	}[best], "system pressure"
	if driver == "" {
		driver, prefix = "pressure is elevated", "system pressure"
	} else if best == "heap" || best == "mem" || best == "psi_mem" {
		prefix = "memory pressure"
	}
	if joined != "" {
		return fmt.Sprintf("%s %.2f — %s (%s)", prefix, score, driver, joined)
	}
	return fmt.Sprintf("%s %.2f — %s", prefix, score, driver)
}
