package urnettools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
)

func TestDirectStarGlyphAndThemes(t *testing.T) {
	clock := &fakeClock{t: topBase}
	states := []uint8{connect.ProxyCellUp, connect.ProxyCellUp, connect.ProxyCellDead}

	for _, name := range tui.ThemeNames() {
		th, ok := tui.ThemeByName(name)
		if !ok {
			t.Fatalf("missing theme %s", name)
		}
		m := newTestModel(th, topProviders(1), clock)
		m.proxyCells = makeProxyCells(states)

		b := tui.New(40, 10)
		m.drawProxyGrid(b)

		c := b.Cell(0, 0)
		if th.ASCII {
			if c.Rune != '*' {
				t.Fatalf("theme %s: expected ASCII star '*', got %c (%U)", name, c.Rune, c.Rune)
			}
			if c.Style.Attrs&tui.AttrBold == 0 {
				t.Fatalf("theme %s: ASCII star must have bold attribute, style: %+v", name, c.Style)
			}
		} else {
			if c.Rune != '★' {
				t.Fatalf("theme %s: expected Unicode star '★' (U+2605), got %c (%U)", name, c.Rune, c.Rune)
			}
		}

		// Verify distinct shape in mono and colorblind themes
		if name == "mono" {
			// In mono theme, proxies are '.' or ':' or '+' in sparse, or '█' in dense
			// Direct star is '*' with bold attribute, which is visually distinct
			if c.Rune != '*' {
				t.Fatalf("mono theme must use '*' for direct star, got %c", c.Rune)
			}
		}
		if name == "colorblind" {
			// In colorblind theme, proxies are half blocks '▀'/'▄' or '█' or '∙'
			// Direct star is '★', maintaining distinct shape
			if c.Rune != '★' {
				t.Fatalf("colorblind theme must use '★' for direct star, got %c", c.Rune)
			}
		}
	}
}

func TestDirectStarTerminalSizesAndProxyCounts(t *testing.T) {
	clock := &fakeClock{t: topBase}
	sizes := []struct{ w, h int }{
		{40, 10},
		{80, 24},
		{100, 30},
		{120, 40},
		{160, 50},
	}
	counts := []int{1, 50, 5000, 65535}

	for _, sz := range sizes {
		for _, n := range counts {
			testName := fmt.Sprintf("%dx%d_n%d", sz.w, sz.h, n)
			t.Run(testName, func(t *testing.T) {
				m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
				cells := make([]proxyCell, n)
				for i := range cells {
					cells[i] = proxyCell{
						index:   i,
						state:   connect.ProxyCellUp,
						bucket:  uint8(i % 8),
						clients: uint8(i % 100),
					}
				}
				m.proxyCells = cells

				b := tui.New(sz.w, sz.h)
				m.drawProxyGrid(b)

				// Direct slot at (0, 0) must always be the star
				starCell := b.Cell(0, 0)
				if starCell.Rune != '★' {
					t.Fatalf("cell (0, 0) must be star '★', got %c (%U)", starCell.Rune, starCell.Rune)
				}

				// Hit testing at (0, 0) must return index 0
				firstIdx, ok := m.proxyCellFirstAt(0, 0)
				if !ok || firstIdx != 0 {
					t.Fatalf("proxyCellFirstAt(0, 0) = %d, %v; want 0, true", firstIdx, ok)
				}

				indices := m.proxyCellAt(0, 0)
				if len(indices) != 1 || indices[0] != 0 {
					t.Fatalf("proxyCellAt(0, 0) = %v; want [0] (never grouped with proxies)", indices)
				}

				// If there are proxies (n > 1), verify proxy 1 is not in cell (0, 0)
				if n > 1 {
					found1At00 := false
					for _, idx := range indices {
						if idx == 1 {
							found1At00 = true
							break
						}
					}
					if found1At00 {
						t.Fatalf("proxy 1 must NOT be grouped into cell (0, 0)")
					}
				}
			})
		}
	}
}

func TestDirectStarHitTestingAndPopup(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.proxyCells = makeProxyCells([]uint8{connect.ProxyCellUp, connect.ProxyCellUp, connect.ProxyCellDead})
	m.proxyDetail = map[int]ProxyCellDetail{
		0: {Index: 0, State: connect.ProxyCellUp, Clients: 5, AgeSeconds: 120},
		1: {Index: 1, Addr: "192.0.2.1:1080", State: connect.ProxyCellUp, Clients: 2},
	}

	b := tui.New(80, 24)
	m.drawProxyGrid(b)

	// Hit test (0, 0)
	idx, ok := m.proxyCellFirstAt(0, 0)
	if !ok || idx != 0 {
		t.Fatalf("proxyCellFirstAt(0, 0) = %d, %v; want 0, true", idx, ok)
	}

	// Detail lines for direct slot (index 0)
	lines := m.proxyCellInfo(0)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "direct (this host)") {
		t.Fatalf("proxyCellInfo(0) must contain 'direct (this host)', got:\n%s", joined)
	}
	if !strings.Contains(joined, "state up") {
		t.Fatalf("proxyCellInfo(0) missing state up:\n%s", joined)
	}

	// Open popup over (0, 0)
	m.proxyPopupRect = tui.Rect{X: 10, Y: 5, W: 60, H: 10}
	if !m.proxyPopupAt(10, 5) {
		t.Fatalf("proxyPopupAt(10, 5) returned false")
	}
	if len(m.proxyPopupIndices) != 1 || m.proxyPopupIndices[0] != 0 {
		t.Fatalf("proxyPopupIndices = %v; want [0]", m.proxyPopupIndices)
	}

	frame := tui.New(70, 15)
	m.drawProxyPopup(frame)
	s := frame.String()
	if !strings.Contains(s, "direct (this host)") {
		t.Fatalf("drawProxyPopup must contain 'direct (this host)', got:\n%s", s)
	}
}

func TestDirectStarAllHeatmapStyles(t *testing.T) {
	clock := &fakeClock{t: topBase}
	cells := makeProxyCells([]uint8{connect.ProxyCellUp, connect.ProxyCellUp, connect.ProxyCellDead})

	for _, style := range heatmapStyles {
		t.Run(style.String(), func(t *testing.T) {
			m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
			m.proxyCells = cells
			m.proxyHeatmapStyle = style

			b := tui.New(80, 20)
			m.drawProxyHeatmap(b)
			s := b.String()

			// In all styles, the legend reads "★ direct up"
			if !strings.Contains(s, "★ direct up") {
				t.Fatalf("style %s: legend missing '★ direct up', got:\n%s", style.String(), s)
			}
		})
	}

	// ASCII / mono fallback in all styles
	for _, style := range heatmapStyles {
		t.Run(style.String()+"_ASCII", func(t *testing.T) {
			m := newTestModel(tui.MonoTheme(), topProviders(1), clock)
			m.proxyCells = cells
			m.proxyHeatmapStyle = style

			b := tui.New(80, 20)
			m.drawProxyHeatmap(b)
			s := b.String()

			// In ASCII / mono, the legend reads "* direct up"
			if !strings.Contains(s, "* direct up") {
				t.Fatalf("style %s ASCII: legend missing '* direct up', got:\n%s", style.String(), s)
			}
		})
	}
}

func TestDirectStarLegendStates(t *testing.T) {
	clock := &fakeClock{t: topBase}
	stateCases := []struct {
		state uint8
		want  string
		wantA string
	}{
		{connect.ProxyCellUp, "★ direct up", "* direct up"},
		{connect.ProxyCellDegraded, "★ direct dropped", "* direct dropped"},
		{connect.ProxyCellConnecting, "★ direct connecting", "* direct connecting"},
		{connect.ProxyCellDead, "★ direct dead", "* direct dead"},
		{connect.ProxyCellParked, "★ direct parked", "* direct parked"},
	}

	for _, sc := range stateCases {
		t.Run(sc.want, func(t *testing.T) {
			// Unicode theme
			m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
			m.proxyCells = []proxyCell{
				{index: 0, state: sc.state, bucket: 2},
				{index: 1, state: connect.ProxyCellUp, bucket: 4},
			}
			b := tui.New(80, 10)
			m.drawProxyGrid(b)
			s := b.String()
			if !strings.Contains(s, sc.want) {
				t.Fatalf("Unicode legend missing %q, got:\n%s", sc.want, s)
			}

			// ASCII / mono theme
			mMono := newTestModel(tui.MonoTheme(), topProviders(1), clock)
			mMono.proxyCells = m.proxyCells
			bMono := tui.New(80, 10)
			mMono.drawProxyGrid(bMono)
			sMono := bMono.String()
			if !strings.Contains(sMono, sc.wantA) {
				t.Fatalf("ASCII legend missing %q, got:\n%s", sc.wantA, sMono)
			}
		})
	}
}

func TestDirectStarWithoutDirectSlot(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	// Proxies 1..5 only, no direct slot (index 0)
	cells := make([]proxyCell, 5)
	for i := range cells {
		cells[i] = proxyCell{index: i + 1, state: connect.ProxyCellUp}
	}
	m.proxyCells = cells

	b := tui.New(60, 10)
	m.drawProxyGrid(b)
	s := b.String()

	// When index 0 is absent, no direct legend is drawn
	if strings.Contains(s, "direct") {
		t.Fatalf("should not draw direct in legend when index 0 is absent, got:\n%s", s)
	}
}
