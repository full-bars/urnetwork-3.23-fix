package main

import "strings"

// importantLogMarkers select the high-value, low-volume lines mirrored into the
// separate small /dev/shm "important" buffer so the earnings/health signal
// survives for hours even when the main ramlog floods. Deliberately excludes
// high-volume lines (per-proxy reload enumeration, per-attempt auth failures,
// `give-up` cycles, `[net][s]select`); only the rare terminal `Permanently
// removed` eviction line is kept from the proxy-init path.
var importantLogMarkers = []string{
	"[profit]",
	"[earn]",
	"[health]",
	"[outage]",
	"[pace]",
	"client_id",
	"instance_id",
	"Permanently removed",
	"[proxy][authrate]",
	// Probe-grade lines are low-volume and high-value: the per-cycle grade
	// breakdown, what got admitted by tier, cap evictions, and reaper grade
	// refreshes happen at most a few times a day and are the only record of
	// how the quality gate behaved. Keep them in the important buffer so
	// they survive hours of main-log flooding.
	"probe grade breakdown",
	"admitted by tier",
	"cap eviction",
	"reaper: refreshed grade",
}

// isImportantLogLine reports whether a single log line should be mirrored to the
// important buffer.
func isImportantLogLine(line string) bool {
	for _, m := range importantLogMarkers {
		if strings.Contains(line, m) {
			return true
		}
	}
	return false
}
