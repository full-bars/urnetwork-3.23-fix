package urnettools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

func makeTestProxies(n int, hasDirect bool) []proxyCell {
	out := make([]proxyCell, 0, n)
	start := 1
	if hasDirect {
		out = append(out, proxyCell{index: 0, state: connect.ProxyCellUp, bucket: 5, clients: 3})
	}
	for i := 0; i < n; i++ {
		var st uint8 = connect.ProxyCellUp
		switch i % 5 {
		case 1:
			st = connect.ProxyCellDegraded
		case 2:
			st = connect.ProxyCellConnecting
		case 3:
			st = connect.ProxyCellDead
		case 4:
			st = connect.ProxyCellParked
		}
		out = append(out, proxyCell{
			index:   start + i,
			state:   st,
			bucket:  uint8(i % 8),
			clients: uint8(i % 10),
		})
	}
	return out
}

func bufferToString(b *tui.Buffer) string {
	var sb strings.Builder
	for y := 0; y < b.Height(); y++ {
		for x := 0; x < b.Width(); x++ {
			c := b.Cell(x, y).Rune
			if c == 0 {
				c = ' '
			}
			sb.WriteRune(c)
		}
		sb.WriteByte('\n')
	}
	return sb.String()
}

func TestProxyWindowOpenCloseKeyHandling(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.proxyCells = makeTestProxies(50, true)

	// Initially not open
	if m.panel == "proxies" {
		t.Fatalf("panel initially %q; want empty", m.panel)
	}

	// Press 'p' opens the window
	pressRune(m, 'p')
	if m.panel != "proxies" {
		t.Fatalf("after 'p': panel = %q; want 'proxies'", m.panel)
	}

	// Press 'p' closes the window
	pressRune(m, 'p')
	if m.panel != "" {
		t.Fatalf("after second 'p': panel = %q; want empty", m.panel)
	}

	// Press 'P' opens the window
	pressRune(m, 'P')
	if m.panel != "proxies" {
		t.Fatalf("after 'P': panel = %q; want 'proxies'", m.panel)
	}

	// Esc closes the window
	m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyEsc})
	if m.panel != "" {
		t.Fatalf("after Esc: panel = %q; want empty", m.panel)
	}

	// Esc closes popup first, then window on next Esc
	pressRune(m, 'p')
	m.proxyPopup = true
	m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyEsc})
	if m.proxyPopup {
		t.Fatalf("first Esc must close popup")
	}
	if m.panel != "proxies" {
		t.Fatalf("first Esc must keep window open; got panel = %q", m.panel)
	}
	m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyEsc})
	if m.panel != "" {
		t.Fatalf("second Esc must close window; got panel = %q", m.panel)
	}
}

func TestProxyWindowLayoutSizesAndProxyCounts(t *testing.T) {
	clock := &fakeClock{t: topBase}
	sizes := []struct {
		w, h int
	}{
		{50, 12},
		{80, 24},
		{100, 30},
		{120, 40},
		{150, 40},
	}
	counts := []int{0, 1, 50, 5000, 65535}

	for _, sz := range sizes {
		for _, n := range counts {
			t.Run(fmt.Sprintf("%dx%d_%d_proxies", sz.w, sz.h, n), func(t *testing.T) {
				m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
				m.panel = "proxies"
				if n > 0 {
					m.proxyCells = makeTestProxies(n, true)
				}
				b := tui.New(sz.w, sz.h)
				m.render(b)
				out := bufferToString(b)

				if n == 0 {
					if !strings.Contains(out, "no proxies") {
						t.Fatalf("expected 'no proxies' in output for n=0:\n%s", out)
					}
					if !strings.Contains(out, "Esc or p closes") {
						t.Fatalf("expected 'Esc or p closes' in output for n=0:\n%s", out)
					}
					return
				}

				if !strings.Contains(out, "Proxies") {
					t.Fatalf("expected 'Proxies' title in output:\n%s", out)
				}
				if !strings.Contains(out, "Esc or p closes") {
					t.Fatalf("expected 'Esc or p closes' footer in output:\n%s", out)
				}
				// Verify direct star glyph is present
				if !strings.Contains(out, "★") {
					t.Fatalf("expected direct star glyph '★' in output:\n%s", out)
				}
				// Verify legend state count is present
				if !strings.Contains(out, "up") {
					t.Fatalf("expected 'up' in legend row:\n%s", out)
				}
			})
		}
	}
}

func TestProxyWindowThemeDegradation(t *testing.T) {
	clock := &fakeClock{t: topBase}
	monoTh := tui.MonoTheme()
	cbTh, ok := tui.ThemeByName("colorblind")
	if !ok {
		t.Fatal("colorblind theme missing")
	}

	// 1. Mono theme
	t.Run("MonoTheme", func(t *testing.T) {
		m := newTestModel(monoTh, topProviders(1), clock)
		m.panel = "proxies"
		m.proxyCells = makeTestProxies(50, true)
		b := tui.New(120, 40)
		m.render(b)
		out := bufferToString(b)
		if !strings.Contains(out, "*") {
			t.Fatalf("mono theme should render star glyph:\n%s", out)
		}
		if !strings.Contains(out, "direct up") {
			t.Fatalf("mono theme should render direct legend:\n%s", out)
		}
	})

	// 2. Colorblind theme
	t.Run("ColorblindTheme", func(t *testing.T) {
		m := newTestModel(cbTh, topProviders(1), clock)
		m.panel = "proxies"
		m.proxyCells = makeTestProxies(50, true)
		b := tui.New(120, 40)
		m.render(b)
		out := bufferToString(b)
		if !strings.Contains(out, "★") {
			t.Fatalf("colorblind theme should render star glyph:\n%s", out)
		}
		if !strings.Contains(out, "direct up") {
			t.Fatalf("colorblind theme should render direct legend:\n%s", out)
		}
	})

	// 3. ASCII theme
	t.Run("ASCIITheme", func(t *testing.T) {
		asciiTh := tui.DefaultTheme()
		asciiTh.ASCII = true
		m := newTestModel(asciiTh, topProviders(1), clock)
		m.panel = "proxies"
		m.proxyCells = makeTestProxies(50, true)
		b := tui.New(120, 40)
		m.render(b)
		out := bufferToString(b)
		if !strings.Contains(out, "* direct up") {
			t.Fatalf("ascii theme should render '*' star in legend:\n%s", out)
		}
		// Must not contain unicode star
		if strings.Contains(out, "★") {
			t.Fatalf("ascii theme must not contain unicode star '★':\n%s", out)
		}
	})
}

func TestProxyWindowScrollingAndZoomKeys(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.panel = "proxies"
	// 5000 proxies requires scrolling on 80x24
	m.proxyCells = makeTestProxies(5000, true)

	b := tui.New(80, 24)
	m.render(b)

	if m.proxyWinMaxScroll <= 0 {
		t.Fatalf("expected proxyWinMaxScroll > 0 for 5000 proxies on 80x24; got %d", m.proxyWinMaxScroll)
	}

	// Scroll down with Down key
	m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyDown})
	if m.proxyWinScroll != 1 {
		t.Fatalf("after Down: scroll = %d; want 1", m.proxyWinScroll)
	}

	// Scroll down with 'j'
	pressRune(m, 'j')
	if m.proxyWinScroll != 2 {
		t.Fatalf("after 'j': scroll = %d; want 2", m.proxyWinScroll)
	}

	// Scroll up with Up key
	m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyUp})
	if m.proxyWinScroll != 1 {
		t.Fatalf("after Up: scroll = %d; want 1", m.proxyWinScroll)
	}

	// Scroll up with 'k'
	pressRune(m, 'k')
	if m.proxyWinScroll != 0 {
		t.Fatalf("after 'k': scroll = %d; want 0", m.proxyWinScroll)
	}

	// Zoom toggle with 'z'
	if m.proxyWinZoom {
		t.Fatalf("zoom initially true; want false")
	}
	pressRune(m, 'z')
	if !m.proxyWinZoom {
		t.Fatalf("after 'z': zoom = false; want true")
	}
	m.render(b)
	out := bufferToString(b)
	if !strings.Contains(out, "[fit]") {
		t.Fatalf("zoomed mode should display '[fit]' in legend row:\n%s", out)
	}

	// Toggle zoom back with 'Z'
	pressRune(m, 'Z')
	if m.proxyWinZoom {
		t.Fatalf("after 'Z': zoom = true; want false")
	}
}

func TestProxyWindowMouseInteraction(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.panel = "proxies"
	m.proxyCells = makeTestProxies(50, true)

	b := tui.New(120, 40)
	m.render(b)

	// Move mouse over direct star cell at (2, 2)
	m.handle(tcellui.Event{
		Kind:   tcellui.EventMouse,
		Action: tcellui.MouseMove,
		X:      2,
		Y:      2,
	})
	if !m.proxyPopup {
		t.Fatalf("expected proxyPopup true after hover at (2, 2)")
	}
	if len(m.proxyPopupIndices) != 1 || m.proxyPopupIndices[0] != 0 {
		t.Fatalf("expected direct slot popup [0]; got %v", m.proxyPopupIndices)
	}

	// Mouse click outside window closes it
	m.handle(tcellui.Event{
		Kind:   tcellui.EventMouse,
		Action: tcellui.MousePress,
		Button: tcellui.MouseLeft,
		X:      0,
		Y:      0,
	})
	if m.panel != "" {
		t.Fatalf("click outside window must close panel; got %q", m.panel)
	}

	// Reopen with 'p'
	pressRune(m, 'p')
	m.render(b)

	// Mouse wheel down scrolls
	m.proxyCells = makeTestProxies(65535, true)
	m.render(b)
	prevScroll := m.proxyWinScroll
	m.handle(tcellui.Event{
		Kind:   tcellui.EventMouse,
		Action: tcellui.MouseWheel,
		Button: tcellui.MouseWheelDown,
	})
	if m.proxyWinScroll <= prevScroll {
		t.Fatalf("mouse wheel down must increase scroll; got %d, prev %d", m.proxyWinScroll, prevScroll)
	}
}

func TestProxyWindowFooterHintAndClick(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.proxyCells = makeTestProxies(20, true)

	hints := m.footerHints()
	found := false
	for _, h := range hints {
		if h.name == "footer:proxies" && h.label == "p proxies" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("footerHints must contain 'p proxies' when proxies are present; got %v", hints)
	}

	// Clicking footer:proxies opens the window
	m.handle(tcellui.Event{
		Kind:   tcellui.EventMouse,
		Action: tcellui.MousePress,
		Button: tcellui.MouseLeft,
		X:      1,
		Y:      39,
	})
	m.mouseClick("footer:proxies", tcellui.Event{})
	if m.panel != "proxies" {
		t.Fatalf("clicking footer:proxies must open proxies window; panel = %q", m.panel)
	}

	// Clicking footer:proxies second time must close window
	m.mouseClick("footer:proxies", tcellui.Event{})
	if m.panel != "" {
		t.Fatalf("clicking footer:proxies second time must close window; panel = %q", m.panel)
	}
}
