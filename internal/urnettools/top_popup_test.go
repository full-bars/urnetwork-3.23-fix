package urnettools

import (
	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
	"strings"
	"testing"
)

func TestProxyPopupMouseDenseAndLeave(t *testing.T) {
	m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	m.proxyCells = makeProxyCells([]uint8{connect.ProxyCellUp, connect.ProxyCellDead})
	m.proxyDetail = map[int]ProxyCellDetail{1: {Index: 1, Addr: "proxy.example:1080"}}
	b := tui.New(1, 2)
	m.drawProxyGrid(b)
	m.proxyPopupRect = tui.Rect{X: 2, Y: 2, W: 45, H: 6}
	m.mouseHandle(mouseEv(tcellui.MouseMove, tcellui.MouseNone, 2, 2))
	if !m.proxyPopup || len(m.proxyPopupIndices) != 2 || m.proxyPopupIndices[0] != 0 || m.proxyPopupIndices[1] != 1 {
		t.Fatalf("popup indices: %v", m.proxyPopupIndices)
	}
	frame := tui.New(50, 10)
	m.drawProxyPopup(frame)
	if s := frame.String(); !strings.Contains(s, "direct (this host)") || !strings.Contains(s, "proxy.example") {
		t.Fatalf("popup: %s", s)
	}
	m.mouseHandle(mouseEv(tcellui.MouseMove, tcellui.MouseNone, 49, 9))
	if m.proxyPopup {
		t.Fatal("popup stayed open")
	}
	m.mouseHandle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, 2, 2))
	if !m.proxyPopup {
		t.Fatal("click did not open popup")
	}
	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 'w'})
	if m.proxyPopup {
		t.Fatal("key did not close popup")
	}
}

func TestProxyPopupSmallBoundsAndGrouped(t *testing.T) {
	m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	m.proxyCells = makeProxyCells(repeatState(connect.ProxyCellUp, 10))
	m.proxyCells[8].state = connect.ProxyCellDead
	m.drawProxyGrid(tui.New(1, 2))
	m.proxyPopupRect = tui.Rect{X: 1, Y: 1, W: 3, H: 3}
	if !m.proxyPopupAt(1, 1) || len(m.proxyPopupIndices) != 10 || m.proxyPopupIndices[0] != 8 {
		t.Fatalf("group: %v", m.proxyPopupIndices)
	}
	b := tui.New(5, 5)
	b.Fill(b.Rect(), '!', tui.Style{})
	m.drawProxyPopup(b)
	for y := 0; y < 5; y++ {
		for x := 0; x < 5; x++ {
			if (x < 1 || x >= 4 || y < 1 || y >= 4) && b.Cell(x, y).Rune != '!' {
				t.Fatal("popup crossed panel bounds")
			}
		}
	}
}

func TestProxyLegendDirectAndHelp(t *testing.T) {
	for _, name := range tui.ThemeNames() {
		th, ok := tui.ThemeByName(name)
		if !ok {
			t.Fatal(name)
		}
		m := newTestModel(th, topProviders(1), &fakeClock{t: topBase})
		m.proxyCells = makeProxyCells([]uint8{connect.ProxyCellUp, connect.ProxyCellUp})
		b := tui.New(70, 3)
		m.drawProxyGrid(b)
		if s := b.String(); !strings.Contains(s, "direct up") || !strings.Contains(s, "1 up") {
			t.Fatalf("%s: %s", name, s)
		}
	}
	key := strings.Join(helpLines, " ")
	for _, term := range []string{"Grid state", "brightness is throughput", "fades", "upper and lower", "worst state", "Hover or click", "bold and dim"} {
		if !strings.Contains(key, term) {
			t.Errorf("help missing %s", term)
		}
	}
}

func TestProxyPopupRejectsCredentialsFromOldProvider(t *testing.T) {
	m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	m.proxyCells = []proxyCell{{index: 1}}
	m.proxyDetail = map[int]ProxyCellDetail{1: {Index: 1, Addr: "socks5://user:password@proxy.example:1080/?token=secret"}}
	got := strings.Join(m.proxyCellInfo(1), " ")
	if !strings.Contains(got, "proxy.example:1080") || strings.Contains(got, "password") || strings.Contains(got, "token") || strings.Contains(got, "user") {
		t.Fatalf("unsafe popup %s", got)
	}
}

func TestProxyHeatmapDenseHalfStyles(t *testing.T) {
	m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	m.proxyCells = makeProxyCells([]uint8{connect.ProxyCellUp, connect.ProxyCellDead})
	b := tui.New(1, 2)
	m.drawProxyGrid(b)
	c := b.Cell(0, 0)
	if c.Rune != '▀' || c.Style.FG != m.proxyCellStyle(connect.ProxyCellUp, 0).FG || c.Style.BG != m.proxyCellStyle(connect.ProxyCellDead, 1).FG {
		t.Fatalf("halves: %+v", c)
	}
	for _, name := range []string{"mono", "colorblind"} {
		th, _ := tui.ThemeByName(name)
		m.theme = th
		b = tui.New(1, 2)
		m.drawProxyGrid(b)
		if c := b.Cell(0, 0); c.Rune != '█' {
			t.Fatalf("%s dense cell: %+v", name, c)
		}
	}
}
