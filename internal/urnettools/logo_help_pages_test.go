package urnettools

import (
	"strings"
	"testing"
)

// The commands that print a hand-written help page put the small icon beside
// its first paragraph, the way the help template does for the other commands,
// and print the page unchanged when the logo is off.
func TestHandWrittenHelpPagesCarryTheLogo(t *testing.T) {
	pages := map[string]func(){
		"auth":           func() { _ = cmdAuth([]string{"--help"}) },
		"choose-network": func() { _ = cmdChooseNetwork([]string{"--help"}) },
		"proxy":          func() { _ = cmdProxy([]string{"add", "--help"}, false, false) },
		"session":        func() { _ = cmdSession([]string{"--help"}) },
		"set":            printSetHelp,
	}
	for name, show := range pages {
		resetLogoFlag(t)
		fakeLogoTerminal(t, 120, 40)
		fakeLogoStderr(t, true, 120, 40)
		fakeLogoPrefs(t, t.TempDir())
		fakeLogoEnv(t, map[string]string{"TERM": "xterm-256color"})
		with := captureStderr(t, show)
		if !strings.Contains(with, "▀") {
			t.Errorf("%s: the help page has no icon:\n%s", name, with)
		}
		logoOffByFlag = true
		without := captureStderr(t, show)
		if strings.Contains(without, "▀") || strings.Contains(without, "\x1b[") {
			t.Errorf("%s: --no-logo must print the page alone:\n%s", name, without)
		}
		if !strings.HasPrefix(without, "urnet-tools ") {
			t.Errorf("%s: the plain page must start with its title:\n%s", name, without)
		}
	}
}

func TestLogoHelpKeepsThePageWhenOff(t *testing.T) {
	resetLogoFlag(t)
	logoOffByFlag = true
	page := "urnet-tools x — title\n\nUsage: urnet-tools x\n"
	if got := logoHelp(page); got != page {
		t.Fatalf("logoHelp changed the page with the logo off:\n%q", got)
	}
}

// fakeLogoStderr stands in for the terminal behind stderr, where the
// hand-written help pages are written.
func fakeLogoStderr(t *testing.T, terminal bool, width, height int) {
	t.Helper()
	origTTY, origSize := logoStderrIsTerminal, logoStderrSize
	t.Cleanup(func() { logoStderrIsTerminal, logoStderrSize = origTTY, origSize })
	logoStderrIsTerminal = func() bool { return terminal }
	logoStderrSize = func() (int, int, bool) { return width, height, terminal }
}

// A help page is written to stderr, so the icon follows stderr. Redirecting
// stderr to a file keeps the artwork out of it even when stdout is a terminal,
// and a terminal on stderr gets it even when stdout is redirected.
func TestHandWrittenHelpFollowsStderr(t *testing.T) {
	page := "urnet-tools x — title\n\nUsage: urnet-tools x\n"
	for _, tc := range []struct {
		name           string
		stdout, stderr bool
		want           bool
	}{
		{"stderr redirected, stdout a terminal", true, false, false},
		{"stdout redirected, stderr a terminal", false, true, true},
		{"both terminals", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetLogoFlag(t)
			origTTY, origSize := logoStdoutIsTerminal, logoStdoutSize
			t.Cleanup(func() { logoStdoutIsTerminal, logoStdoutSize = origTTY, origSize })
			logoStdoutIsTerminal = func() bool { return tc.stdout }
			logoStdoutSize = func() (int, int, bool) { return 120, 40, tc.stdout }
			fakeLogoStderr(t, tc.stderr, 120, 40)
			fakeLogoPrefs(t, t.TempDir())
			fakeLogoEnv(t, map[string]string{"TERM": "xterm-256color"})
			got := logoHelp(page)
			if has := strings.Contains(got, "▀"); has != tc.want {
				t.Fatalf("icon present = %v, want %v:\n%s", has, tc.want, got)
			}
			if !tc.want && got != page {
				t.Fatalf("the page changed without a terminal on stderr:\n%q", got)
			}
		})
	}
}
