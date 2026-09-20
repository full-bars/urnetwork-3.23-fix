package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Restart reasons reported in the node snapshot and urnet_restart_reason.
const (
	restartReasonUpdate     = "update"
	restartReasonHotswap    = "hotswap"
	restartReasonManual     = "manual"
	restartReasonClean      = "clean"
	restartReasonUnclean    = "unclean"
	restartReasonFirstStart = "first-start"
)

// restartMarkerMaxAge is how old a .restart-reason marker may be and still
// count. An older one was left by a restart that never completed.
const restartMarkerMaxAge = 10 * time.Minute

// restartMarkerReasons are the reasons a marker may carry. Everything else in
// the reason set is derived by classifyRestart, never written.
var restartMarkerReasons = map[string]bool{
	restartReasonUpdate:  true,
	restartReasonHotswap: true,
	restartReasonManual:  true,
}

// parseRestartMarker reads the contents of <state dir>/.restart-reason:
// one line, "<reason> <RFC3339 UTC time>". It returns "" for a marker that is
// malformed, carries an unknown reason, or is older than restartMarkerMaxAge.
func parseRestartMarker(data []byte, now time.Time) string {
	fields := strings.Fields(string(data))
	if len(fields) != 2 || !restartMarkerReasons[fields[0]] {
		return ""
	}
	at, err := time.Parse(time.RFC3339, fields[1])
	if err != nil {
		return ""
	}
	if now.Sub(at) > restartMarkerMaxAge {
		return ""
	}
	return fields[0]
}

// consumeRestartMarker reads and deletes <state dir>/.restart-reason and
// returns the reason it carried, or "" when it was absent, stale or invalid.
// The file is removed in every case so a bad marker cannot linger.
func consumeRestartMarker(stateDir string, now time.Time) string {
	path := filepath.Join(stateDir, ".restart-reason")
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	os.Remove(path)
	return parseRestartMarker(data, now)
}

// classifyRestart decides why this process started. A fresh marker written by
// whoever restarted us wins. Otherwise the clean-shutdown marker separates a
// plain restart or reboot from a crash, and a missing version file means
// there was no previous run.
func classifyRestart(markerReason string, cleanShutdown bool, previousVersion string) string {
	switch {
	case markerReason != "":
		return markerReason
	case cleanShutdown:
		return restartReasonClean
	case previousVersion != "":
		return restartReasonUnclean
	default:
		return restartReasonFirstStart
	}
}
