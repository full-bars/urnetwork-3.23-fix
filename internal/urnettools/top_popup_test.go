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
	m.proxyDetail = map[int]ProxyCellDetail{
		0: {Index: 0},
		1: {Index: 1, Addr: "proxy.example:1080"},
	}
	b := tui.New(2, 2)
	m.drawProxyGrid(b)
	m.proxyPopupRect = tui.Rect{X: 2, Y: 2, W: 45, H: 6}
	// Direct slot (0, 0) at x=2, y=2:
	m.mouseHandle(mouseEv(tcellui.MouseMove, tcellui.MouseNone, 2, 2))
	if !m.proxyPopup || len(m.proxyPopupIndices) != 1 || m.proxyPopupIndices[0] != 0 {
		t.Fatalf("direct popup indices: %v", m.proxyPopupIndices)
	}
	frame := tui.New(50, 10)
	m.drawProxyPopup(frame)
	if s := frame.String(); !strings.Contains(s, "direct (this host)") {
		t.Fatalf("direct popup missing this host: %s", s)
	}
	// Proxy 1 at x=3, y=2:
	m.mouseHandle(mouseEv(tcellui.MouseMove, tcellui.MouseNone, 3, 2))
	if !m.proxyPopup || len(m.proxyPopupIndices) != 1 || m.proxyPopupIndices[0] != 1 {
		t.Fatalf("proxy 1 popup indices: %v", m.proxyPopupIndices)
	}
	frame = tui.New(50, 10)
	m.drawProxyPopup(frame)
	if s := frame.String(); !strings.Contains(s, "proxy.example") {
		t.Fatalf("proxy 1 popup missing addr: %s", s)
	}
	// Mouse leaving closes popup:
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
	// 10 proxies without direct slot (indices 1..10)
	cells := make([]proxyCell, 10)
	for i := range cells {
		cells[i] = proxyCell{index: i + 1, state: connect.ProxyCellUp}
	}
	cells[7].state = connect.ProxyCellDead // proxy 8
	m.proxyCells = cells
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
	// Proxies 1 and 2 (dense half dots, neither is direct slot)
	m.proxyCells = []proxyCell{{index: 1, state: connect.ProxyCellUp}, {index: 2, state: connect.ProxyCellDead}}
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

	// When direct slot (index 0) is present, cell (0, 0) is a full cell star
	m.theme = tui.DefaultTheme()
	m.proxyCells = []proxyCell{{index: 0, state: connect.ProxyCellUp}, {index: 1, state: connect.ProxyCellDead}}
	b = tui.New(1, 2)
	m.drawProxyGrid(b)
	if c := b.Cell(0, 0); c.Rune != '★' || c.Style.FG != m.proxyCellStyle(connect.ProxyCellUp, 0).FG {
		t.Fatalf("direct star cell: %+v", c)
	}
	// ASCII fallback: '*' bold
	thMono, _ := tui.ThemeByName("mono")
	m.theme = thMono
	b = tui.New(1, 2)
	m.drawProxyGrid(b)
	if c := b.Cell(0, 0); c.Rune != '*' || c.Style.Attrs&tui.AttrBold == 0 {
		t.Fatalf("mono star cell: %+v", c)
	}
}

func TestProxyPopupLifetimeTrafficAndMissingData(t *testing.T) {
	m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	m.proxyCells = makeProxyCells([]uint8{connect.ProxyCellUp})
	m.proxyPopupIndices = []int{0}
	m.proxyPopupRect = tui.Rect{X: 0, Y: 0, W: 64, H: 6}
	m.proxyDetail = map[int]ProxyCellDetail{0: {Index: 0, LifetimeBillable: &ProxyCellTraffic{Rx: 1024, Tx: 2048}, LifetimeTotal: &ProxyCellTraffic{Rx: 3072, Tx: 4096}}}
	frame := tui.New(64, 6)
	m.drawProxyPopup(frame)
	got := frame.String()
	for _, want := range []string{"traffic, last second", "lifetime billable in " + tui.Bytes(1024) + " out " + tui.Bytes(2048), "lifetime total in " + tui.Bytes(3072) + " out " + tui.Bytes(4096)} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	m.proxyDetail = nil
	got = strings.Join(m.proxyCellInfo(0), "\n")
	if !strings.Contains(got, "lifetime billable ?") || !strings.Contains(got, "lifetime total ?") {
		t.Fatalf("missing unknown values: %s", got)
	}
}
