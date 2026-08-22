package main

// importantLogf logs a line to BOTH the ramlog (via tlog, which also feeds
// the /dev/shm important buffer through isImportantLogLine markers) AND the
// disk-based events.log (critLog). Use it for rare, high-value lines the
// operator should still find after a reboot wipes /dev/shm — the probe
// grade breakdown, tier admissions, cap evictions, and reaper grade
// refreshes happen only a few times a day and are the record of how the
// quality gate behaved.
func importantLogf(format string, args ...any) {
	tlog(format, args...)
	critLog(format, args...)
}
