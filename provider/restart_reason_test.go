package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseRestartMarker(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) string { return now.Add(-d).Format(time.RFC3339) }
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"update fresh", "update " + ts(time.Minute) + "\n", "update"},
		{"hotswap fresh", "hotswap " + ts(0), "hotswap"},
		{"manual fresh", "manual " + ts(9*time.Minute), "manual"},
		{"exactly at max age", "update " + ts(restartMarkerMaxAge), "update"},
		{"stale", "update " + ts(restartMarkerMaxAge+time.Second), ""},
		{"future timestamp accepted", "update " + now.Add(time.Minute).Format(time.RFC3339), "update"},
		{"unknown reason", "reboot " + ts(time.Minute), ""},
		{"derived reason not writable", "clean " + ts(time.Minute), ""},
		{"bad timestamp", "update yesterday", ""},
		{"reason only", "update", ""},
		{"extra field", "update " + ts(time.Minute) + " extra", ""},
		{"empty", "", ""},
		{"garbage bytes", "\x00\xff\xfe", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRestartMarker([]byte(tc.in), now); got != tc.want {
				t.Fatalf("parseRestartMarker(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestClassifyRestart(t *testing.T) {
	cases := []struct {
		name    string
		marker  string
		clean   bool
		prevVer string
		curVer  string
		want    string
	}{
		{"marker beats clean", "update", true, "v1", "v2", "update"},
		{"marker beats unclean", "hotswap", false, "v1", "v2", "hotswap"},
		{"marker beats first start", "manual", false, "", "", "manual"},
		{"clean marker", "", true, "v1", "v1", "clean"},
		{"clean marker without version", "", true, "", "", "clean"},
		// A version CHANGE is an upgrade, not a crash: the binary on disk is not
		// the one that wrote .provider_version, so something deliberately
		// replaced it. Reporting this as unclean is what made three upgraded
		// boxes claim they had crashed for the life of the new version.
		{"version change is an update", "", false, "v1", "v2", "update"},
		// Same version, no marker, no clean exit: the only genuine unclean case.
		{"same version with no marker is unclean", "", false, "v1", "v1", "unclean"},
		{"first start", "", false, "", "", "first-start"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRestart(tc.marker, tc.clean, tc.prevVer, tc.curVer); got != tc.want {
				t.Fatalf("classifyRestart(%q, %v, %q, %q) = %q, want %q",
					tc.marker, tc.clean, tc.prevVer, tc.curVer, got, tc.want)
			}
		})
	}
}

func TestConsumeRestartMarkerDeletesInEveryCase(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fresh := "update " + now.Format(time.RFC3339)
	stale := "update " + now.Add(-restartMarkerMaxAge-time.Minute).Format(time.RFC3339)
	for name, tc := range map[string]struct{ content, want string }{
		"fresh": {fresh, "update"},
		// The window must cover a slow update, not just the restart itself.
		"slow update": {"update " + now.Add(-45*time.Minute).Format(time.RFC3339), "update"},
		"stale":       {stale, ""},
		"garbage":     {"???", ""},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ".restart-reason")
			if err := os.WriteFile(path, []byte(tc.content), 0600); err != nil {
				t.Fatal(err)
			}
			if got := consumeRestartMarker(dir, now); got != tc.want {
				t.Fatalf("consumeRestartMarker = %q, want %q", got, tc.want)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("marker not deleted (stat err = %v)", err)
			}
		})
	}
	t.Run("absent", func(t *testing.T) {
		if got := consumeRestartMarker(t.TempDir(), now); got != "" {
			t.Fatalf("consumeRestartMarker on empty dir = %q, want empty", got)
		}
	})
}

// detectStartup ties the markers and the version file together. Each case
// starts from a fresh state dir and a fresh startupDiag.
// restartReasonCase is one end-to-end startup: which files are present in the
// state dir, and what detectStartup should conclude from them.
type restartReasonCase struct {
	name      string
	marker    string // "" = no file
	clean     bool
	version   string // "" = no file
	want      string
	wantClean bool
}

func TestDetectStartupRestartReason(t *testing.T) {
	fresh := "update " + time.Now().UTC().Format(time.RFC3339)
	// past the marker window, so it is still treated as abandoned
	stale := "update " + time.Now().UTC().Add(-restartMarkerMaxAge-time.Minute).Format(time.RFC3339)

	// A version CHANGE now means update, so the "same version" cases have to
	// write exactly what the running process would write. RequireVersion() is
	// empty in a test binary (no ldflags), and an empty previous version means
	// first-start, so the same-version path cannot be expressed through the
	// file at all here; TestClassifyRestart covers it by passing the versions
	// in directly. These cases therefore all describe a version change.
	prior := RequireVersion() + "-previous"
	cases := []restartReasonCase{
		{"fresh marker and clean", fresh, true, prior, "update", true},
		{"fresh marker, crash", fresh, false, prior, "update", false},
		{"stale marker, clean", stale, true, prior, "clean", true},
		{"stale marker, version change", stale, false, prior, "update", false},
		{"garbage marker, version change", "junk", false, prior, "update", false},
		{"garbage marker, first start", "junk", false, "", "first-start", false},
		{"clean only", "", true, prior, "clean", true},
		// The bug this fixes: a version change is an upgrade, not a crash.
		// Reporting it as unclean is what made three upgraded boxes claim they
		// had restarted uncleanly for the life of the new version.
		{"version change is an update, not a crash", "", false, prior, "update", false},
		{"first start", "", false, "", "first-start", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("USERPROFILE", home)
			dir := filepath.Join(home, ".urnetwork")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			write := func(name, content string) {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.marker != "" {
				write(".restart-reason", tc.marker)
			}
			if tc.clean {
				write(".clean-shutdown", time.Now().UTC().Format(time.RFC3339))
			}
			if tc.version != "" {
				write(".provider_version", tc.version)
			}

			saved := startupDiag
			startupDiag = &startupDiagnostics{}
			defer func() { startupDiag = saved }()

			detectStartup()
			if startupDiag.restartReason != tc.want {
				t.Fatalf("restartReason = %q, want %q", startupDiag.restartReason, tc.want)
			}
			if startupDiag.cleanShutdown != tc.wantClean {
				t.Fatalf("cleanShutdown = %v, want %v", startupDiag.cleanShutdown, tc.wantClean)
			}
			// The restart marker describes exactly one restart, so it is consumed.
			if _, err := os.Stat(filepath.Join(dir, ".restart-reason")); !os.IsNotExist(err) {
				t.Fatalf("restart marker survived detectStartup (stat err = %v)", err)
			}
			// The clean-shutdown marker IS consumed: it describes exactly one
			// restart, and leaving it behind is what made a crash look clean.
			_, statErr := os.Stat(filepath.Join(dir, ".clean-shutdown"))
			if tc.clean && !os.IsNotExist(statErr) {
				t.Fatalf("clean-shutdown marker must be consumed by detectStartup, stat err = %v", statErr)
			}
		})
	}
}
