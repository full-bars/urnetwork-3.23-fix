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
// count, as a guard against a marker left by a restart that never completed
// being read as the reason for a restart days later.
//
// It has to cover the WHOLE update, not just the restart: the marker is written
// before the binary is fetched, verified and swapped, and on a slow link or a
// loaded box that easily takes longer than a few minutes. At the old 10 minutes
// an ordinary in-place upgrade expired its own marker, the reason fell through
// to the version-change branch, and every upgraded box reported itself unclean
// for the life of that version (observed on two boxes after the 32.1 -> 32.7
// update, one reporting "unclean (v3.23.0-fix.32.1 to v3.23.0-fix.32.7)").
// An hour leaves room for a slow download plus a restart while still expiring
// a marker abandoned by a restart that never ran.
const restartMarkerMaxAge = time.Hour

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
// whoever restarted us wins. Otherwise a version change is an UPGRADE, not a
// crash: the binary on disk is not the one that wrote .provider_version, so
// something deliberately replaced it, and reporting that as unclean told
// operators a box had crashed when it had been updated. Only a same-version
// start with no marker and no clean-shutdown marker is a genuine unclean exit.
func classifyRestart(markerReason string, cleanShutdown bool, previousVersion, currentVersion string) string {
	switch {
	case markerReason != "":
		return markerReason
	case cleanShutdown:
		return restartReasonClean
	case previousVersion != "" && previousVersion != currentVersion:
		return restartReasonUpdate
	case previousVersion != "":
		return restartReasonUnclean
	default:
		return restartReasonFirstStart
	}
}
