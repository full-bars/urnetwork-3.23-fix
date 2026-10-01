package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Observation-only resource signals for the baseline recorder and /metrics:
// the GC state, GC CPU share, conntrack fill and TCP socket states. Nothing
// here drives an actuator. They exist so a later change to the governor or
// the pool controller can be judged against measurements instead of guesses,
// and so socket exhaustion, which drops packets silently and shows only in
// dmesg, becomes visible. Every reader is best-effort and Linux-only: a reader
// that fails omits its field rather than writing a zero.

// baselineGC is the GC governor's visible state. GOGC is the value in force,
// Tightening whether the governor is holding it below the baseline, and CPUFrac
// the share of CPU spent in GC over the last internals window.
type baselineGC struct {
	GOGC       int64    `json:"gogc"`
	Tightening bool     `json:"tightening"`
	CPUFrac    *float64 `json:"cpu_frac,omitempty"`
}

// baselineNet is the kernel's socket-table pressure. ConntrackUsedFrac is
// nf_conntrack_count over nf_conntrack_max; TimeWait and Orphans come from the
// TCP line of /proc/net/sockstat.
type baselineNet struct {
	ConntrackUsedFrac *float64 `json:"conntrack_used_frac,omitempty"`
	TimeWait          *int64   `json:"tcp_time_wait,omitempty"`
	Orphans           *int64   `json:"tcp_orphans,omitempty"`
}

// parseSockstat reads the TCP line of /proc/net/sockstat:
//
//	TCP: inuse 29 orphan 3 tw 112 alloc 40 mem 7
//
// ok is false when there is no TCP line or it carries neither counter.
func parseSockstat(content string) (timeWait, orphans int64, ok bool) {
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "TCP:") {
			continue
		}
		fields := strings.Fields(line)
		var haveTW, haveOrphan bool
		for i := 1; i+1 < len(fields); i++ {
			switch fields[i] {
			case "tw":
				if v, err := strconv.ParseInt(fields[i+1], 10, 64); err == nil {
					timeWait, haveTW = v, true
				}
			case "orphan":
				if v, err := strconv.ParseInt(fields[i+1], 10, 64); err == nil {
					orphans, haveOrphan = v, true
				}
			}
		}
		return timeWait, orphans, haveTW || haveOrphan
	}
	return 0, 0, false
}

// conntrackUsedFrac is count/max, unknown when max is not positive. A real
// count of zero is data.
func conntrackUsedFrac(count, max int64) (float64, bool) {
	if max <= 0 || count < 0 {
		return 0, false
	}
	return float64(count) / float64(max), true
}

func readProcInt(path string) (int64, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	v, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// readBaselineNet gathers the socket-table readings, or nil when none of them
// could be read (non-Linux, or a kernel without conntrack loaded).
func readBaselineNet() *baselineNet {
	var n baselineNet
	any := false
	count, okCount := readProcInt("/proc/sys/net/netfilter/nf_conntrack_count")
	max, okMax := readProcInt("/proc/sys/net/netfilter/nf_conntrack_max")
	if okCount && okMax {
		if f, ok := conntrackUsedFrac(count, max); ok {
			n.ConntrackUsedFrac, any = &f, true
		}
	}
	if b, err := os.ReadFile("/proc/net/sockstat"); err == nil {
		if tw, orphans, ok := parseSockstat(string(b)); ok {
			n.TimeWait, n.Orphans, any = &tw, &orphans, true
		}
	}
	if !any {
		return nil
	}
	return &n
}

// readBaselineGC gathers the GC governor's visible state. GOGC comes from the
// non-mutating runtime metric; the CPU share is omitted until the internals
// collector has completed a window.
func readBaselineGC(now time.Time) *baselineGC {
	gogc, ok := readGOGCPercent()
	if !ok {
		return nil
	}
	g := &baselineGC{GOGC: int64(gogc), Tightening: gcTightening.Load()}
	if in := nodeInternals.Get(now); in != nil && in.IntervalSeconds > 0 {
		f := in.GCCPUFraction
		g.CPUFrac = &f
	}
	return g
}

// writeResourceSignalGauges emits the Prometheus series for the readings above.
// A reading that is unknown is left out rather than exported as 0, so a missing
// series means unknown.
func writeResourceSignalGauges(b *strings.Builder, gc *baselineGC, net *baselineNet) {
	if gc != nil {
		fmt.Fprintf(b, "# HELP urnet_gc_gogc GOGC value currently in force.\n")
		fmt.Fprintf(b, "# TYPE urnet_gc_gogc gauge\n")
		fmt.Fprintf(b, "urnet_gc_gogc %d\n", gc.GOGC)
		tight := 0
		if gc.Tightening {
			tight = 1
		}
		fmt.Fprintf(b, "# HELP urnet_gc_tightening 1 while the GC governor holds GOGC below its baseline.\n")
		fmt.Fprintf(b, "# TYPE urnet_gc_tightening gauge\n")
		fmt.Fprintf(b, "urnet_gc_tightening %d\n", tight)
		if gc.CPUFrac != nil {
			fmt.Fprintf(b, "# HELP urnet_gc_cpu_fraction Share of CPU spent in GC over the last window.\n")
			fmt.Fprintf(b, "# TYPE urnet_gc_cpu_fraction gauge\n")
			fmt.Fprintf(b, "urnet_gc_cpu_fraction %g\n", *gc.CPUFrac)
		}
	}
	if net != nil {
		if net.ConntrackUsedFrac != nil {
			fmt.Fprintf(b, "# HELP urnet_conntrack_used_ratio nf_conntrack_count over nf_conntrack_max (Linux). Overflow drops packets silently.\n")
			fmt.Fprintf(b, "# TYPE urnet_conntrack_used_ratio gauge\n")
			fmt.Fprintf(b, "urnet_conntrack_used_ratio %g\n", *net.ConntrackUsedFrac)
		}
		if net.TimeWait != nil {
			fmt.Fprintf(b, "# HELP urnet_tcp_time_wait TCP sockets in TIME_WAIT (Linux, /proc/net/sockstat).\n")
			fmt.Fprintf(b, "# TYPE urnet_tcp_time_wait gauge\n")
			fmt.Fprintf(b, "urnet_tcp_time_wait %d\n", *net.TimeWait)
		}
		if net.Orphans != nil {
			fmt.Fprintf(b, "# HELP urnet_tcp_orphans Orphaned TCP sockets (Linux, /proc/net/sockstat).\n")
			fmt.Fprintf(b, "# TYPE urnet_tcp_orphans gauge\n")
			fmt.Fprintf(b, "urnet_tcp_orphans %d\n", *net.Orphans)
		}
	}
}
