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
