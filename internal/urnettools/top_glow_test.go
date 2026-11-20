package urnettools

import (
	"fmt"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
)

func TestProxyAfterglowDecayAndReset(t *testing.T) {
	m, _ := menuModel(t)
	now := topBase
	m.now = func() time.Time { return now }
	m.proxyAfterglowSeconds = 10
	cell := proxyCell{index: 3, state: connect.ProxyCellUp, bucket: 6}
	m.applyProxyCells(m.gen, []proxyCell{cell}, nil)
	now = now.Add(5 * time.Second)
	cell.bucket = 0
	m.applyProxyCells(m.gen, []proxyCell{cell}, nil)
	if got := m.proxyGlowBucket(cell, now); got != 3 {
		t.Fatalf("halfway glow = %d, want 3", got)
	}
	now = now.Add(5 * time.Second)
	if got := m.proxyGlowBucket(cell, now); got != 0 {
		t.Fatalf("expired glow = %d, want 0", got)
	}
	cell.bucket = 7
	m.applyProxyCells(m.gen, []proxyCell{cell}, nil)
	m.applyProxyCells(m.gen, nil, nil)
	cell.bucket = 0
	if got := m.proxyGlowBucket(cell, now); got != 0 {
		t.Fatalf("cleared glow = %d, want 0", got)
	}
}

func TestProxyAfterglowRendered(t *testing.T) {
	m, _ := menuModel(t)
	now := topBase
	m.now = func() time.Time { return now }
	m.proxyAfterglowSeconds = 10
	cell := proxyCell{index: 1, state: connect.ProxyCellUp, bucket: 6}
	m.applyProxyCells(m.gen, []proxyCell{cell}, nil)
	cell.bucket = 0
	now = now.Add(time.Second)
	m.applyProxyCells(m.gen, []proxyCell{cell}, nil)
	b := tui.New(40, 3)
	m.drawProxyGrid(b)
	before := b.Cell(0, 0).Style
	now = now.Add(10 * time.Second)
	b = tui.New(40, 3)
	m.drawProxyGrid(b)
	if after := b.Cell(0, 0).Style; before == after {
		t.Fatal("glow did not fade in the rendered grid")
	}
}

func TestProxyAfterglowThemes(t *testing.T) {
	for _, name := range []string{"mono", "colorblind"} {
		t.Run(name, func(t *testing.T) {
			m, _ := menuModel(t)
			m.theme, _ = tui.ThemeByName(name)
			now := topBase
			m.now = func() time.Time { return now }
			m.proxyAfterglowSeconds = 10
			cell := proxyCell{index: 1, state: connect.ProxyCellUp, bucket: 7}
			m.applyProxyCells(m.gen, []proxyCell{cell}, nil)
			cell.bucket = 0
			m.applyProxyCells(m.gen, []proxyCell{cell}, nil)
			b := tui.New(40, 3)
			m.drawProxyGrid(b)
			before := b.Cell(0, 0)
			now = now.Add(11 * time.Second)
			b = tui.New(40, 3)
			m.drawProxyGrid(b)
			after := b.Cell(0, 0)
			if before == after {
				t.Fatal("glow did not fade")
			}
			if name == "colorblind" && (before.Style.FG != m.theme.OK.FG || after.Style.FG != m.theme.OK.FG) {
				t.Fatal("glow did not preserve the theme's healthy color")
			}
		})
	}
}

func TestTopGlowFrameGoldens(t *testing.T) {
	for _, theme := range []string{"default", "mono", "colorblind"} {
		for _, size := range []struct{ w, h int }{{80, 24}, {100, 30}, {120, 40}} {
			name := fmt.Sprintf("top_glow_%s_%dx%d", theme, size.w, size.h)
			t.Run(name, func(t *testing.T) {
				th, ok := tui.ThemeByName(theme)
				if !ok {
					t.Fatalf("unknown theme %q", theme)
				}
				m := topHeatGridModel(t, th, 4400)
				m.applyProxyCells(m.gen, m.proxyCells, nil)
				for i := range m.proxyCells {
					m.proxyCells[i].bucket = 0
				}
				got, _ := showOnSim(t, m, size.w, size.h)
				assertGolden(t, name, got)
			})
		}
	}
}

func TestProxyAfterglowMenuPersists(t *testing.T) {
	m, path := menuModel(t)
	m.menuSel = topMenuAfterglow
	m.menuCycle(1)
	if got := loadTopSettings(path).ProxyAfterglowSeconds; got != 6 {
		t.Fatalf("saved afterglow = %d, want 6", got)
	}
}

func TestProxyCellsDue(t *testing.T) {
	for _, tc := range []struct {
		interval, elapsed time.Duration
		want              bool
	}{
		{100 * time.Millisecond, 99 * time.Millisecond, false},
		{100 * time.Millisecond, 100 * time.Millisecond, true},
		{5 * time.Second, 999 * time.Millisecond, false},
		{5 * time.Second, time.Second, true},
	} {
		if got := proxyCellsDue(topBase.Add(tc.elapsed), topBase, tc.interval); got != tc.want {
			t.Errorf("interval %s elapsed %s: got %v", tc.interval, tc.elapsed, got)
		}
	}
}
