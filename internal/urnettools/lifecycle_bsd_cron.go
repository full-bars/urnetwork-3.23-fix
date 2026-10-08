package urnettools

import "strings"

// The FreeBSD auto-update crontab bookkeeping, kept untagged so the matching
// RULE is unit-testable on any platform. The functions that use it
// (writeBSDUpdateCron / removeBSDUpdateCron) only exist on FreeBSD.

// bsdCronMarker tags every line this tool writes so a schedule change can
// remove its own lines without touching anything else in the crontab.
const bsdCronMarker = "# urnet-tools auto-update"

// matchBSDCronLine reports whether a crontab line is the auto-update entry for
// this specific service.
//
// The marker must be matched at the END of the line. bsdCronMarker is a common
// prefix of every marker written, so a Contains match on "... auto-update
// urnetwork" also matches "... auto-update urnetwork_beta" — which would let a
// schedule change for the stable provider delete the beta provider's entry on a
// box running both. Trimming first makes a trailing space harmless.
func matchBSDCronLine(line, service string) bool {
	return strings.HasSuffix(strings.TrimSpace(line), bsdCronMarker+" "+service)
}
