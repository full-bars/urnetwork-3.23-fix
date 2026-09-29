package main

import "fmt"

// importantLogf logs a line to BOTH the ramlog (via tlog, which also feeds
// the /dev/shm important buffer through isImportantLogLine markers) AND the
// disk-based events.log (critLog). Use it for rare, high-value lines the
// operator should still find after a reboot wipes /dev/shm — the probe
// grade breakdown, tier admissions, cap evictions, and reaper grade
// refreshes happen only a few times a day and are the record of how the
// quality gate behaved.
func importantLogf(format string, args ...any) {
	line := format
	if len(args) > 0 {
		line = fmt.Sprintf(format, args...)
	}
	// The hook takes the line over completely: a test that sets it is asserting
	// on the exact startup line, and writing it to the ramlog and events.log as
	// well would only add noise to the test output.
	if importantLogHook != nil {
		importantLogHook(line)
		return
	}
	tlog("%s", line)
	critLog("%s", line)
}

// importantLogHook, when set by a test, receives the fully formatted line
// instead of it being written to the ramlog and events.log. Startup-only
// one-shot lines (the resource warnings) have no other way to be observed from
// a test: the process's own memory limits cannot be set per test, so the seam
// sits on the formatted line instead.
var importantLogHook func(line string)
