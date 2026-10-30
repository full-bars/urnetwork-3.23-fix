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

// The swapped binary in the root-owned FreeBSD tree must not be chowned to the
// service user: root executes it afterwards. Paths outside the tree, and names
// that merely share its prefix, are not in the tree.
func TestBSDTreeOwnedByRoot(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/usr/local/libexec/urnetwork-provider/bin/urnetwork", true},
		{"/usr/local/libexec/urnetwork-provider/bin/urnet-tools", true},
		{"/usr/local/libexec/urnetwork-provider", true},
		{"/usr/local/libexec/urnetwork-provider-evil/bin/urnetwork", false},
		{"/home/alice/.local/share/urnetwork-provider/bin/urnetwork", false},
		{"", false},
	}
	for _, c := range cases {
		if got := bsdTreeOwnedByRoot(c.path); got != c.want {
			t.Errorf("bsdTreeOwnedByRoot(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// Root's crontab must never run a path the invoker can replace. Root gets the
// staged tool or an error, and a non-root crontab keeps the running tool.
func TestBSDScheduledUpdate(t *testing.T) {
	tool, logDir, err := bsdScheduledUpdate(0, true, "/home/alice/bin/urnet-tools", "/home/alice/.urnetwork")
	if err != nil || tool != bsdRootToolPath || logDir != bsdRootLogDir {
		t.Errorf("root with staged tool = (%q, %q, %v)", tool, logDir, err)
	}
	if _, _, err := bsdScheduledUpdate(0, false, "/home/alice/bin/urnet-tools", "/home/alice/.urnetwork"); err == nil {
		t.Error("root without a staged tool must be refused, not fall back to the invoker's binary")
	}
	tool, logDir, err = bsdScheduledUpdate(1001, false, "/home/alice/bin/urnet-tools", "/home/alice/.urnetwork")
	if err != nil || tool != "/home/alice/bin/urnet-tools" || logDir != "/home/alice/.urnetwork" {
		t.Errorf("non-root = (%q, %q, %v)", tool, logDir, err)
	}
}
