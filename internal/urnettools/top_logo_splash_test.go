package urnettools

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/brand"
	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// logoTopModel is a top model on a fake terminal of the given size, with the
// logo switches read from a fake environment and an empty settings file.
func logoTopModel(t *testing.T, th tui.Theme, providers, w, h int, env map[string]string) (*topModel, *fakeClock) {
	t.Helper()
	resetLogoFlag(t)
	fakeLogoTerminal(t, w, h)
	fakeLogoPrefs(t, t.TempDir())
	if env == nil {
		env = map[string]string{"TERM": "xterm-256color", "COLORTERM": "truecolor"}
	}
	fakeLogoEnv(t, env)
	clock := &fakeClock{t: topBase}
	m := newTestModel(th, topProviders(providers), clock)
	return m, clock
}

func renderText(m *topModel, w, h int) string {
	b := tui.New(w, h)
	renderFresh(m, b)
	return b.String()
}

// isSplash reports whether a frame is the splash: the logo and none of the
// dashboard's header.
func isSplash(text string) bool {
	return !strings.Contains(text, "urnet-tools top") && strings.Contains(text, "█")
}

func TestTopSplashShowsOnceAtStart(t *testing.T) {
	m, clock := logoTopModel(t, tui.DefaultTheme(), 2, 120, 40, nil)
	m.enableLogo(false, false)

	if text := renderText(m, 120, 40); !isSplash(text) {
		t.Fatalf("the first frame must be the splash:\n%s", text)
	}
	clock.Advance(topSplashDuration - time.Millisecond)
	if text := renderText(m, 120, 40); !isSplash(text) {
		t.Fatalf("the splash must hold for %s:\n%s", topSplashDuration, text)
	}
	clock.Advance(time.Millisecond)
	if text := renderText(m, 120, 40); isSplash(text) {
		t.Fatalf("the splash must end after %s:\n%s", topSplashDuration, text)
	}

	// A lost provider that comes back, and a provider switch, are not a start.
	m.lost(clock.Now(), errors.New("connection refused"))
	if text := renderText(m, 120, 40); isSplash(text) {
		t.Fatalf("a lost provider must not bring the splash back:\n%s", text)
	}
	m.apply(m.gen, topFlowing(t), nil)
	if text := renderText(m, 120, 40); isSplash(text) {
		t.Fatalf("a reconnect must not bring the splash back:\n%s", text)
	}
	if m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyTab}) != topRefetch {
		t.Fatal("Tab must switch the provider after the splash")
	}
	if text := renderText(m, 120, 40); isSplash(text) {
		t.Fatalf("a provider switch must not bring the splash back:\n%s", text)
	}
}

func TestTopSplashSkippedByAKeyOrAClick(t *testing.T) {
	for _, ev := range []tcellui.Event{
		{Kind: tcellui.EventRune, Rune: 'x'},
		{Kind: tcellui.EventRune, Rune: 'q'},
		{Kind: tcellui.EventKey, Key: tcellui.KeyEsc},
		{Kind: tcellui.EventKey, Key: tcellui.KeyTab},
		{Kind: tcellui.EventMouse, Action: tcellui.MousePress, Button: tcellui.MouseLeft, X: 3, Y: 3},
	} {
		m, _ := logoTopModel(t, tui.DefaultTheme(), 2, 120, 40, nil)
		m.enableLogo(false, false)
		if !isSplash(renderText(m, 120, 40)) {
			t.Fatal("the first frame must be the splash")
		}
		// The key only skips the splash. It does not also quit, switch the
		// provider or open anything.
		if eff := m.handle(ev); eff != topNone {
			t.Fatalf("%+v: the skipping event had effect %v", ev, eff)
		}
		if m.cur != 0 || m.quit || m.help || m.menu {
			t.Fatalf("%+v: the skipping event also acted on the dashboard", ev)
		}
		if text := renderText(m, 120, 40); isSplash(text) {
			t.Fatalf("%+v: the splash must end on the event:\n%s", ev, text)
		}
	}

	// A mouse move is not a click and leaves the splash up.
	m, _ := logoTopModel(t, tui.DefaultTheme(), 1, 120, 40, nil)
	m.enableLogo(false, false)
	renderText(m, 120, 40)
	m.handle(tcellui.Event{Kind: tcellui.EventMouse, Action: tcellui.MouseMove, X: 3, Y: 3})
	if !isSplash(renderText(m, 120, 40)) {
		t.Fatal("a mouse move must not skip the splash")
	}
	// Ctrl-C still quits from the splash.
	if m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyCtrlC}) != topQuit {
		t.Fatal("Ctrl-C must quit during the splash")
	}
}

func TestTopSplashNeverWhenTheLogoIsOff(t *testing.T) {
	cases := []struct {
		name  string
		th    tui.Theme
		w, h  int
		env   map[string]string
		plain bool
		copy  bool
		flag  bool
		conf  string
	}{
		{name: "--plain", plain: true},
		{name: "--copy", copy: true},
		{name: "--no-logo", flag: true},
		{name: "URNET_NO_LOGO=1", env: map[string]string{"TERM": "xterm-256color", "URNET_NO_LOGO": "1"}},
		{name: "ui.logo = off", conf: "ui.logo = off\n"},
		{name: "mono theme", th: tui.MonoTheme(), env: map[string]string{"TERM": "dumb"}},
		{name: "NO_COLOR", th: tui.MonoTheme(), env: map[string]string{"TERM": "xterm", "NO_COLOR": "1"}},
		{name: "too small", w: 39, h: 20},
		{name: "too short", w: 120, h: 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			th := c.th
			if th.Name == "" {
				th = tui.DefaultTheme()
			}
			w, h := c.w, c.h
			if w == 0 {
				w, h = 120, 40
			}
			m, _ := logoTopModel(t, th, 1, w, h, c.env)
			if c.conf != "" {
				if err := os.WriteFile(logoPrefsPath(), []byte(c.conf), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			logoOffByFlag = c.flag
			m.enableLogo(c.plain, c.copy)
			if text := renderText(m, w, h); isSplash(text) || strings.Contains(text, "▀") {
				t.Fatalf("no splash expected:\n%s", text)
			}
			if eff := m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: '?'}); eff != topNone || !m.help {
				t.Fatal("with no splash the first key must act as usual")
			}
		})
	}
}

// The splash takes the asset the brand package picks for the terminal size,
// centered.
func TestTopSplashChoosesTheAssetBySize(t *testing.T) {
	for _, size := range [][2]int{{120, 40}, {100, 30}, {80, 24}, {60, 20}, {45, 12}} {
		w, h := size[0], size[1]
		m, _ := logoTopModel(t, tui.DefaultTheme(), 1, w, h, nil)
		m.enableLogo(false, false)
		b := tui.New(w, h)
		renderFresh(m, b)
		a := brand.Choose(w, h)
		if a == nil {
			t.Fatalf("%dx%d: brand picks no asset", w, h)
		}
		x, y := (w-a.Cols)/2, (h-a.Rows)/2
		grid := brand.Grid(a, m.logoCaps)
		for ry, row := range grid {
			for rx, c := range row {
				if got := b.Cell(x+rx, y+ry).Rune; got != c.Glyph && !(c.Glyph == ' ' && got == 0) {
					t.Fatalf("%dx%d %s: cell %d,%d is %q, want %q", w, h, a.Name, x+rx, y+ry, got, c.Glyph)
				}
			}
		}
	}
}

// The splash never delays the first data fetch: the loop asks the provider
// while the splash is still on screen.
func TestTopSplashDoesNotDelayTheFirstFetch(t *testing.T) {
	m, _ := logoTopModel(t, tui.DefaultTheme(), 1, 120, 40, nil)
	m.enableLogo(false, false)
	src, calls := fakeSource(t)
	rig := startLoop(t, m, src, 120, 40)
	rig.waitFor(t, "the splash", isSplash)
	deadline := time.Now().Add(3 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatal("the provider was not asked while the splash was up")
	}
	// The clock never moved, so the splash is still the frame on screen.
	if text := rig.text(); !isSplash(text) {
		t.Fatalf("the splash ended before its time:\n%s", text)
	}
	rig.cancel()
	_ = rig.wait(t)
}
