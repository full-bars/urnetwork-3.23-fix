package urnettools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

func pressSpecial(m *topModel, k tcellui.Key) topEffect {
	return m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: k})
}

func menuModel(t *testing.T) (*topModel, string) {
	t.Helper()
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	m.settingsPath = filepath.Join(t.TempDir(), "urnet-tools", "top.conf")
	return m, m.settingsPath
}

func TestTopMenuOpensAndClosesWithoutQuitting(t *testing.T) {
	m, _ := menuModel(t)
	pressKey(m, 'm')
	if !m.menu {
		t.Fatal("m must open the menu")
	}
	if eff := pressSpecial(m, tcellui.KeyEsc); eff != topNone || m.menu || m.quit {
		t.Fatalf("Esc must close the menu and stay in top (eff=%v menu=%v quit=%v)", eff, m.menu, m.quit)
	}
	pressKey(m, 'm')
	pressKey(m, 'm')
	if m.menu {
		t.Fatal("m again must close it")
	}
}

func TestTopMenuStillQuitsWithQAndCtrlC(t *testing.T) {
	for name, ev := range map[string]tcellui.Event{
		"q":      {Kind: tcellui.EventRune, Rune: 'q'},
		"ctrl-c": {Kind: tcellui.EventKey, Key: tcellui.KeyCtrlC},
	} {
		m, _ := menuModel(t)
		pressKey(m, 'm')
		if eff := m.handle(ev); eff != topQuit {
			t.Errorf("%s in the menu must quit, got %v", name, eff)
		}
	}
}

// The menu owns the keyboard while it is open: a stray w or g must not toggle
// the screen behind it.
func TestTopMenuSwallowsOtherKeys(t *testing.T) {
	m, _ := menuModel(t)
	m.rt.gOK = true
	m.rt.ok = true
	pressKey(m, 'm')
	for _, r := range []rune{'w', 'g', '?', '+', '-'} {
		pressKey(m, r)
	}
	pressSpecial(m, tcellui.KeyTab)
	if m.zoomOn || m.rt.showing || m.help || m.interval != topDefaultInterval || m.cur != 0 {
		t.Fatalf("keys leaked through the menu: zoom=%v goroutines=%v help=%v interval=%v", m.zoomOn, m.rt.showing, m.help, m.interval)
	}
}

func TestTopMenuCyclesTheme(t *testing.T) {
	m, path := menuModel(t)
	pressKey(m, 'm')
	names := tui.ThemeNames()
	if m.theme.Name != names[0] {
		t.Fatalf("test starts on %q", m.theme.Name)
	}
	pressSpecial(m, tcellui.KeyRight)
	if m.theme.Name != names[1] {
		t.Fatalf("Right -> %q, want %q", m.theme.Name, names[1])
	}
	pressSpecial(m, tcellui.KeyLeft)
	pressSpecial(m, tcellui.KeyLeft) // wraps to the last
	if m.theme.Name != names[len(names)-1] {
		t.Fatalf("Left from the first must wrap to %q, got %q", names[len(names)-1], m.theme.Name)
	}
	if got := loadTopSettings(path); got.Theme != names[len(names)-1] {
		t.Fatalf("the choice was not saved: %+v", got)
	}
}

func TestTopMenuCyclesGraphStyleAndItShowsOnScreen(t *testing.T) {
	m, path := menuModel(t)
	pressKey(m, 'm')
	pressKey(m, 'j') // select the graph row
	if m.menuSel != topMenuGraph {
		t.Fatalf("sel = %d", m.menuSel)
	}
	pressKey(m, 'l')
	if m.graph != tui.GraphBlock {
		t.Fatalf("graph = %v, want block", m.graph)
	}
	pressSpecial(m, tcellui.KeyEsc)
	txt, _ := showOnSim(t, m, 120, 40)
	if !strings.Contains(txt, "█") || strings.ContainsAny(txt, "⣀⣿⣴") {
		t.Fatalf("block style not drawn:\n%s", txt)
	}
	m.graph = tui.GraphBraille
	txt, _ = showOnSim(t, m, 120, 40)
	if !strings.ContainsAny(txt, "⣀⣿⣴") {
		t.Fatal("braille not drawn")
	}
	if got := loadTopSettings(path); got.Graph != "block" {
		t.Fatalf("saved graph = %+v", got)
	}
}

func TestTopMenuSelectionWraps(t *testing.T) {
	m, _ := menuModel(t)
	pressKey(m, 'm')
	pressKey(m, 'k')
	if m.menuSel != topMenuRows-1 {
		t.Fatalf("up from the first row = %d", m.menuSel)
	}
	pressKey(m, 'j')
	if m.menuSel != 0 {
		t.Fatalf("down from the last row = %d", m.menuSel)
	}
}

func TestTopMenuThemeChangeRecolorsTheScreen(t *testing.T) {
	m, _ := menuModel(t)
	before := tui.New(120, 40)
	m.render(before)
	pressKey(m, 'm')
	pressSpecial(m, tcellui.KeyRight) // default -> nord
	pressSpecial(m, tcellui.KeyEsc)
	after := tui.New(120, 40)
	m.render(after)
	// A theme changes color, not layout: the text is the same.
	if before.String() != after.String() {
		t.Fatal("changing the theme changed the text on screen")
	}
	th, _ := tui.ThemeByName("nord")
	found := false
	w, h := after.Width(), after.Height()
	for y := 0; y < h && !found; y++ {
		for x := 0; x < w; x++ {
			if c := after.Cell(x, y); c.Style.FG == th.Graph.FG && c.Rune != ' ' {
				found = true
				break
			}
		}
	}
	if !found {
		t.Fatal("nord's graph color appears nowhere on the screen")
	}
}

func TestTopMenuThemeLockedByEnvironment(t *testing.T) {
	m, path := menuModel(t)
	m.theme = tui.MonoTheme()
	m.themeLocked = true
	pressKey(m, 'm')
	pressSpecial(m, tcellui.KeyRight)
	if m.theme.Name != "mono" {
		t.Fatalf("NO_COLOR must not be overridden by the menu, theme = %q", m.theme.Name)
	}
	if m.menuNote == "" {
		t.Error("the menu must say why nothing changed")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("a refused change must not be saved")
	}
	// A saved theme must not override the environment either.
	m.applySettings(topSettings{Theme: "nord"})
	if m.theme.Name != "mono" {
		t.Fatalf("saved theme overrode the environment: %q", m.theme.Name)
	}
}

func TestTopMenuSaveFailureIsReported(t *testing.T) {
	m, _ := menuModel(t)
	// A path under a regular file cannot be created.
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	m.settingsPath = filepath.Join(f, "sub", "top.conf")
	pressKey(m, 'm')
	pressSpecial(m, tcellui.KeyRight)
	if !strings.HasPrefix(m.menuNote, "could not save") {
		t.Fatalf("note = %q", m.menuNote)
	}
	if m.theme.Name == "default" {
		t.Fatal("a failed save must not undo the change on screen")
	}
}

func TestTopMenuNoPathMeansNoPersistence(t *testing.T) {
	m, _ := menuModel(t)
	m.settingsPath = ""
	pressKey(m, 'm')
	pressSpecial(m, tcellui.KeyRight)
	if m.menuNote != "" {
		t.Fatalf("no path is not an error: %q", m.menuNote)
	}
}

func TestTopSettingsRoundTripAndForgiveness(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "top.conf")
	if got := loadTopSettings(path); got != (topSettings{}) {
		t.Fatalf("missing file = %+v", got)
	}
	if err := saveTopSettings(path, topSettings{Theme: "dracula", Graph: "tty"}); err != nil {
		t.Fatal(err)
	}
	if got := loadTopSettings(path); got != (topSettings{Theme: "dracula", Graph: "tty"}) {
		t.Fatalf("round trip = %+v", got)
	}
	// No temp files left beside it.
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("leftovers: %v", entries)
	}
	// Garbage, stale names and comments are ignored, valid lines kept.
	os.WriteFile(path, []byte("# hi\n\nnonsense\ntheme = vaporwave\ngraph = \"block\"\nunknown = 1\ntheme=nord\n"), 0o644)
	if got := loadTopSettings(path); got != (topSettings{Theme: "nord", Graph: "block"}) {
		t.Fatalf("forgiving read = %+v", got)
	}
	os.WriteFile(path, []byte{0xff, 0x00, '='}, 0o644)
	if got := loadTopSettings(path); got != (topSettings{}) {
		t.Fatalf("binary garbage = %+v", got)
	}
}

func TestTopSettingsApplyAtStart(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.applySettings(topSettings{Theme: "gruvbox", Graph: "tty"})
	if m.theme.Name != "gruvbox" || m.graph != tui.GraphTTY {
		t.Fatalf("theme=%q graph=%v", m.theme.Name, m.graph)
	}
	m.applySettings(topSettings{Theme: "nope", Graph: "nope"})
	if m.theme.Name != "gruvbox" || m.graph != tui.GraphTTY {
		t.Fatal("unknown names must leave the current choice alone")
	}
}

func TestTopMenuGoldenScreens(t *testing.T) {
	clock := &fakeClock{t: topBase}
	cases := []struct {
		name string
		th   tui.Theme
		set  func(m *topModel)
	}{
		{"top_menu_100x30", tui.DefaultTheme(), func(m *topModel) { pressKey(m, 'm') }},
		{"top_menu_graph_block_100x30", tui.DefaultTheme(), func(m *topModel) {
			pressKey(m, 'm')
			pressKey(m, 'j')
			pressKey(m, 'l')
		}},
		{"top_menu_mono_100x30", tui.MonoTheme(), func(m *topModel) {
			m.themeLocked = true
			pressKey(m, 'm')
			pressKey(m, 'l')
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			clock.t = topBase
			m := newTestModel(c.th, topProviders(1), clock)
			m.apply(0, topFlowing(t), nil)
			c.set(m)
			got, _ := showOnSim(t, m, 100, 30)
			assertGolden(t, c.name, got)
		})
	}
}

func TestTopMenuFitsSmallTerminals(t *testing.T) {
	clock := &fakeClock{t: topBase}
	for _, sz := range [][2]int{{40, 8}, {50, 12}, {72, 20}, {30, 6}} {
		m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
		m.apply(0, topFlowing(t), nil)
		pressKey(m, 'm')
		got, _ := showOnSim(t, m, sz[0], sz[1])
		for i, line := range strings.Split(strings.TrimRight(got, "\n"), "\n") {
			if w := tui.StringWidth(line); w > sz[0] {
				t.Fatalf("%dx%d: row %d is %d wide", sz[0], sz[1], i, w)
			}
		}
	}
}
