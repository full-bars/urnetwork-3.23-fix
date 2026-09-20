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
		want    string
	}{
		{"marker beats clean", "update", true, "v1", "update"},
		{"marker beats unclean", "hotswap", false, "v1", "hotswap"},
		{"marker beats first start", "manual", false, "", "manual"},
		{"clean marker", "", true, "v1", "clean"},
		{"clean marker without version", "", true, "", "clean"},
		{"unclean", "", false, "v1", "unclean"},
		{"first start", "", false, "", "first-start"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyRestart(tc.marker, tc.clean, tc.prevVer); got != tc.want {
				t.Fatalf("classifyRestart(%q, %v, %q) = %q, want %q", tc.marker, tc.clean, tc.prevVer, got, tc.want)
			}
		})
	}
}

func TestConsumeRestartMarkerDeletesInEveryCase(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	fresh := "update " + now.Format(time.RFC3339)
	stale := "update " + now.Add(-time.Hour).Format(time.RFC3339)
	for name, tc := range map[string]struct{ content, want string }{
		"fresh":   {fresh, "update"},
		"stale":   {stale, ""},
		"garbage": {"???", ""},
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
func TestDetectStartupRestartReason(t *testing.T) {
	fresh := "update " + time.Now().UTC().Format(time.RFC3339)
	stale := "update " + time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	cases := []struct {
		name      string
		marker    string // "" = no file
		clean     bool
		version   string // "" = no file
		want      string
		wantClean bool
	}{
		{"fresh marker and clean", fresh, true, "v1", "update", true},
		{"fresh marker, crash", fresh, false, "v1", "update", false},
		{"stale marker, clean", stale, true, "v1", "clean", true},
		{"stale marker, crash", stale, false, "v1", "unclean", false},
		{"garbage marker, crash", "junk", false, "v1", "unclean", false},
		{"garbage marker, first start", "junk", false, "", "first-start", false},
		{"clean only", "", true, "v1", "clean", true},
		{"crash", "", false, "v1", "unclean", false},
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
			if _, err := os.Stat(filepath.Join(dir, ".restart-reason")); !os.IsNotExist(err) {
				t.Fatalf("restart marker survived detectStartup (stat err = %v)", err)
			}
		})
	}
}
