package urnettools

import "testing"

// The cron marker must match the END of the line, not anywhere in it.
//
// bsdCronMarker is a prefix of every marker this tool writes, so a Contains
// match on "# urnet-tools auto-update urnetwork" also matches a line ending
// "... # urnet-tools auto-update urnetwork_beta". Scheduling a change for the
// stable provider would then silently delete the beta provider's schedule —
// and on a multi-provider box those are the two services most likely to coexist.
func TestBSDCronMarkerMatchesWholeServiceName(t *testing.T) {
	const marker = bsdCronMarker

	cases := []struct {
		name    string
		line    string
		service string
		want    bool
	}{
		{"exact match", "17 3 * * 0 urnet-tools update -f >> log 2>&1 " + marker + " urnetwork", "urnetwork", true},
		{"trailing space tolerated", "17 3 * * 0 urnet-tools update -f >> log 2>&1 " + marker + " urnetwork   ", "urnetwork", true},
		{"longer name sharing the prefix is NOT a match", "17 3 * * 0 urnet-tools update -f >> log 2>&1 " + marker + " urnetwork_beta", "urnetwork", false},
		{"the reverse direction is also safe", "17 3 * * 0 urnet-tools update -f >> log 2>&1 " + marker + " urnetwork", "urnetwork_beta", false},
		{"an unrelated crontab line", "0 4 * * * /usr/local/bin/backup.sh", "urnetwork", false},
		{"a different service entirely", "17 3 * * 0 urnet-tools update -f >> log 2>&1 " + marker + " provider", "urnetwork", false},
	}

	for _, c := range cases {
		// This mirrors the predicate used by writeBSDUpdateCron and
		// removeBSDUpdateCron. If those ever change, change it here too —
		// the point of the test is the RULE, not the literal.
		got := matchBSDCronLine(c.line, c.service)
		if got != c.want {
			t.Errorf("%s: matchBSDCronLine = %v, want %v (line %q, service %q)",
				c.name, got, c.want, c.line, c.service)
		}
	}
}
