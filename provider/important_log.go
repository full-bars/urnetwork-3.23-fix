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
