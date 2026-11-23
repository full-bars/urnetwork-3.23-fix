package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// fakeBuildInfo pins the build line so the About window is deterministic.
func fakeBuildInfo(t *testing.T) {
	t.Helper()
	orig, origVersion := topBuildInfo, ToolVersion
	t.Cleanup(func() { topBuildInfo, ToolVersion = orig, origVersion })
	topBuildInfo = func() (string, bool) { return "0123456789ab", true }
	ToolVersion = "v36.0.0-test"
}

// aboutModel is a model with the splash over and the About window open.
func aboutModel(t *testing.T, th tui.Theme, w, h int, env map[string]string) *topModel {
	t.Helper()
	fakeBuildInfo(t)
	m, _ := logoTopModel(t, th, 1, w, h, env)
	m.enableLogo(false, false)
	m.splash.done = true
	m.apply(m.gen, topFlowing(t), nil)
	if eff := m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 'A'}); eff != topNone || !m.about {
		t.Fatal("A must open the About window")
	}
	return m
}

func TestTopAboutOpensOnAAndCloses(t *testing.T) {
	closers := []tcellui.Event{
		{Kind: tcellui.EventKey, Key: tcellui.KeyEsc},
		{Kind: tcellui.EventRune, Rune: 'x'},
		{Kind: tcellui.EventRune, Rune: 'q'},
		{Kind: tcellui.EventKey, Key: tcellui.KeyTab},
		{Kind: tcellui.EventMouse, Action: tcellui.MousePress, Button: tcellui.MouseLeft, X: 0, Y: 0},
	}
	for _, ev := range closers {
		m := aboutModel(t, tui.DefaultTheme(), 120, 40, nil)
		renderText(m, 120, 40)
		if eff := m.handle(ev); eff != topNone {
			t.Fatalf("%+v: closing the About window had effect %v", ev, eff)
		}
		if m.about || m.quit {
			t.Fatalf("%+v: the event must close the About window and do nothing else", ev)
		}
	}

	// A click inside the window keeps it open.
	m := aboutModel(t, tui.DefaultTheme(), 120, 40, nil)
	renderText(m, 120, 40)
	r := m.aboutRect
	m.handle(tcellui.Event{Kind: tcellui.EventMouse, Action: tcellui.MousePress, Button: tcellui.MouseLeft, X: r.X + r.W/2, Y: r.Y + r.H/2})
	if !m.about {
		t.Fatal("a click inside the About window must keep it open")
	}
	// Ctrl-C still quits.
	if m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyCtrlC}) != topQuit {
		t.Fatal("Ctrl-C must quit with the About window open")
	}

	// The lowercase key keeps its own job.
	m = aboutModel(t, tui.DefaultTheme(), 120, 40, nil)
	m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyEsc})
	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 'a'})
	if m.about || m.panel != "alerts" {
		t.Fatal("a must still open the alerts")
	}
}

func TestTopAboutContent(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {80, 24}} {
		w, h := size[0], size[1]
		for _, c := range []struct {
			name  string
			th    tui.Theme
			env   map[string]string
			art   bool
			ascii bool
		}{
			{name: "default", th: tui.DefaultTheme(), art: true},
			{name: "logo off", th: tui.DefaultTheme(), env: map[string]string{"TERM": "xterm-256color", "URNET_NO_LOGO": "1"}},
			{name: "mono", th: tui.MonoTheme(), env: map[string]string{"TERM": "xterm", "NO_COLOR": "1"}, ascii: true},
			{name: "ascii", th: tui.MonoTheme(), env: map[string]string{"TERM": "dumb"}, ascii: true},
		} {
			m := aboutModel(t, c.th, w, h, c.env)
			text := renderText(m, w, h)
			for _, want := range []string{"About", "urnet-tools v36.0.0-test", "0123456789ab", "full-bars/urnetwork-3.23-fix", "--no-logo", "URNET_NO_LOGO=1", "ui.logo = off"} {
				if !strings.Contains(text, want) {
					t.Errorf("%dx%d %s: the About window is missing %q:\n%s", w, h, c.name, want, text)
				}
			}
			r := m.aboutRect
			if r.W == 0 || r.X < 0 || r.Y < 0 || r.X+r.W > w || r.Y+r.H > h {
				t.Errorf("%dx%d %s: the window %+v does not fit the screen", w, h, c.name, r)
			}
			win := tui.New(w, h)
			renderFresh(m, win)
			art := strings.ContainsAny(win.Sub(r).String(), "█▄▀▟▙▜▛▐▌▝▘▗▖")
			if art != c.art {
				t.Errorf("%dx%d %s: block art drawn = %v, want %v:\n%s", w, h, c.name, art, c.art, text)
			}
			if !c.art && !strings.Contains(win.Sub(r).String(), "URnetwork") {
				t.Errorf("%dx%d %s: without the art the window names URnetwork:\n%s", w, h, c.name, text)
			}
			if c.ascii && strings.ContainsAny(win.Sub(r).String(), "╭╮╰╯─│") {
				t.Errorf("%dx%d %s: an ASCII theme must draw an ASCII frame:\n%s", w, h, c.name, text)
			}
		}
	}
}

// The wide screen gets the wordmark, a standard one the icon.
func TestTopAboutArtBySize(t *testing.T) {
	m := aboutModel(t, tui.DefaultTheme(), 120, 40, nil)
	if text := renderText(m, 120, 40); !strings.Contains(text, "▐█████████▙▖") {
		t.Fatalf("120x40 must show the wide wordmark:\n%s", text)
	}
	m = aboutModel(t, tui.DefaultTheme(), 80, 24, map[string]string{"TERM": "xterm"})
	if text := renderText(m, 80, 24); !strings.Contains(text, "▄██████▄") || strings.Contains(text, "▐█████████▙▖") {
		t.Fatalf("80x24 must show the icon:\n%s", text)
	}
}

func TestTopAboutKeyIsListed(t *testing.T) {
	found := false
	for _, l := range helpLines {
		if strings.HasPrefix(l, "A   ") {
			found = true
		}
	}
	if !found {
		t.Fatal("the help overlay must list the A key")
	}
}

func TestTopBuildLine(t *testing.T) {
	fakeBuildInfo(t)
	if got := topBuildLine(); !strings.Contains(got, "0123456789ab") || !strings.Contains(got, "go") {
		t.Fatalf("build line = %q", got)
	}
	topBuildInfo = func() (string, bool) { return "", false }
	if got := topBuildLine(); strings.Contains(got, "from") || !strings.Contains(got, "go") {
		t.Fatalf("build line without a revision = %q", got)
	}
}

// Opening About while a divider is being dragged must end the drag, as a
// release would. About takes every mouse event, so the release never reaches
// the drag cleanup, and the old divider would follow the next drag.
func TestTopAboutEndsADividerDrag(t *testing.T) {
	fakeBuildInfo(t)
	m, _ := logoTopModel(t, tui.DefaultTheme(), 1, 120, 40, nil)
	m.enableLogo(false, false)
	m.splash.done = true
	m.apply(m.gen, topFlowing(t), nil)
	m.drag = &topDrag{region: "throughput"}
	m.lastPress = topPress{region: "throughput"}
	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 'A'})
	if !m.about {
		t.Fatal("A must open the About window")
	}
	if m.drag != nil || m.lastPress != (topPress{}) {
		t.Fatalf("the drag survived the About window: drag=%v press=%v", m.drag, m.lastPress)
	}
}
