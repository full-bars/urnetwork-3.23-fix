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
	// they survive hours of main-log flooding. Markers carry the exact
	// "[proxy][url] " prefix so unrelated lines cannot match (coderabbit
	// review).
	"[proxy][url] probe grade breakdown",
	"[proxy][url] admitted by tier",
	"[proxy][url] cap eviction",
	"[proxy][url] reaper: refreshed grade",
	// Grade summary + per-address delta lines (design 2026-08-09): the
	// running-tier snapshot, per-source breakdown, changes-vs-last-round,
	// score stats, and tier-change deltas are low-volume and high-value —
	// keep them in the important buffer + disk events.log. The countdown
	// line ("next fetch probe ...") deliberately matches NO marker, so it
	// stays in the regular ramlog only.
	"[proxy][grade] running:",
	"[proxy][grade] sources:",
	"[proxy][grade] changes",
	"[proxy][grade] scores:",
	"[proxy][grade] delta",
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
