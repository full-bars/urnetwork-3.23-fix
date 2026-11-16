package urnettools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect"

	"github.com/urnetwork/connect/internal/tui"
)

func makeProxyCells(states []uint8) []proxyCell {
	out := make([]proxyCell, len(states))
	for i, s := range states {
		out[i] = proxyCell{index: i, state: s, bucket: uint8(i % 8), clients: uint8(i % 255)}
	}
	return out
}

func TestDecodeProxyCellsRoundtrip(t *testing.T) {
	in := []proxyCell{
		{index: 0, state: 0, bucket: 3, clients: 10},
		{index: 1, state: 3, bucket: 7, clients: 200},
		{index: 500, state: 4, bucket: 1, clients: 1},
		{index: 65535, state: 2, bucket: 0, clients: 255},
	}
	// pack manually
	buf := make([]byte, 0, 4*len(in))
	for _, c := range in {
		buf = append(buf, byte(c.index), byte(c.index>>8), c.state<<5|c.bucket, c.clients)
	}
	got := decodeProxyCells(buf)
	if len(got) != len(in) {
		t.Fatalf("got %d cells, want %d", len(got), len(in))
	}
	for i, c := range got {
		if c.index != in[i].index || c.state != in[i].state || c.bucket != in[i].bucket || c.clients != in[i].clients {
			t.Fatalf("cell %d mismatch: got %+v want %+v", i, c, in[i])
		}
	}
	// extra bytes ignored
	got2 := decodeProxyCells(append(buf, 0xFF))
	if len(got2) != len(in) {
		t.Fatalf("trailing byte changed count: %d", len(got2))
	}
}

func TestDrawProxyGridUpAndLegend(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	const n = 88
	m.proxyCells = makeProxyCells(repeatState(connect.ProxyCellUp, n))

	b := tui.New(20, 9)
	m.drawProxyGrid(b)
	s := b.String()
	if !strings.Contains(s, "88 up") {
		t.Fatalf("legend should show 88 up, got:\n%s", s)
	}
	// no crash, and the grid area is drawn with filled dots.
	if !strings.Contains(s, "●") {
		t.Fatalf("expected filled dots in the grid, got:\n%s", s)
	}
}

func TestDrawProxyGridWorstStateGrouping(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	states := append(repeatState(connect.ProxyCellUp, 7), connect.ProxyCellDead)
	m.proxyCells = makeProxyCells(states)

	b := tui.New(60, 3) // capacity = 60 cols * 4 dotRows = 240 dots, group = 1
	m.drawProxyGrid(b)
	s := b.String()
	// the dead proxy must be visible and counted
	if !strings.Contains(s, "1 dead") {
		t.Fatalf("legend should show 1 dead, got:\n%s", s)
	}
}

func TestDrawProxyGrid4400ProxiesSmallBox(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	states := make([]uint8, 4400)
	for i := range states {
		states[i] = connect.ProxyCellUp
	}
	states[100] = connect.ProxyCellDead
	states[200] = connect.ProxyCellDegraded
	m.proxyCells = makeProxyCells(states)

	b := tui.New(70, 33) // capacity 70*64 = 4480 >= 4400, group = 1
	m.drawProxyGrid(b)
	s := b.String()
	if !strings.Contains(s, "1 dead") || !strings.Contains(s, "1 dropped") {
		t.Fatalf("legend missing counts, got:\n%s", s)
	}
}

func TestDrawProxyGridGroupingWorstState(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	// 4400 proxies into a 60x5 box (gridH=4, dotRows=8, capacity=480, group=10):
	// the lone dead proxy claims its whole dot-position.
	states := make([]uint8, 4400)
	for i := range states {
		states[i] = connect.ProxyCellUp
	}
	states[4400-1] = connect.ProxyCellDead
	m.proxyCells = makeProxyCells(states)

	b := tui.New(60, 5)
	m.drawProxyGrid(b)
	s := b.String()
	if !strings.Contains(s, "1 dead") {
		t.Fatalf("legend should show 1 dead, got:\n%s", s)
	}
}

func TestDrawProxyGridASCIIMono(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.MonoTheme(), topProviders(1), clock)
	states := []uint8{connect.ProxyCellUp, connect.ProxyCellDegraded, connect.ProxyCellDead, connect.ProxyCellConnecting, connect.ProxyCellParked}
	m.proxyCells = makeProxyCells(states)

	b := tui.New(60, 4)
	m.drawProxyGrid(b)
	s := b.String()
	// mono theme uses ASCII glyphs and no custom colors
	for _, want := range []string{"1 up", "1 dropped", "1 dead", "1 connecting", "1 parked"} {
		if !strings.Contains(s, want) {
			t.Fatalf("legend missing %q, got:\n%s", want, s)
		}
	}
}

func repeatState(s uint8, n int) []uint8 {
	out := make([]uint8, n)
	for i := range out {
		out[i] = s
	}
	return out
}

func TestProxyCellAtHitTests(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	// 3 proxies fill cell(0,0) upper/lower and cell(0,1) upper
	states := []uint8{connect.ProxyCellUp, connect.ProxyCellUp, connect.ProxyCellDead}
	m.proxyCells = makeProxyCells(states)

	// draw to set proxyGridW/H
	b := tui.New(4, 3)
	m.drawProxyGrid(b)

	if idx, ok := m.proxyCellAt(0, 0); !ok || idx != 0 {
		t.Fatalf("cell(0,0) -> %d,%v; want 0,true", idx, ok)
	}
	if idx, ok := m.proxyCellAt(0, 1); !ok || idx != 2 {
		t.Fatalf("cell(0,1) -> %d,%v; want 2,true (dead proxy)", idx, ok)
	}
	if idx, ok := m.proxyCellAt(3, 0); ok {
		t.Fatalf("cell(3,0) -> %d,true; want not ok (empty)", idx)
	}
	if _, ok := m.proxyCellAt(-1, 0); ok {
		t.Fatal("negative x should not hit")
	}
	if _, ok := m.proxyCellAt(0, 99); ok {
		t.Fatal("out of range y should not hit")
	}
}

func TestProxyCellAtGroupedReturnsFirst(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	// 4400 proxies into a tiny grid forces grouping; every cell maps back to
	// its first group member's index.
	states := make([]uint8, 4400)
	m.proxyCells = makeProxyCells(states)

	b := tui.New(8, 3) // gridH=2, dotRows=4, capacity=32, group=138
	m.drawProxyGrid(b)
	idx, ok := m.proxyCellAt(0, 0)
	if !ok {
		t.Fatal("cell(0,0) should hit")
	}
	if idx != 0 {
		t.Fatalf("grouped cell(0,0) -> %d, want 0 (first of group)", idx)
	}
}

func TestProxyCellInfoFromDetail(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	states := []uint8{connect.ProxyCellUp}
	m.proxyCells = makeProxyCells(states)
	m.proxyDetail = map[int]ProxyCellDetail{
		0: {Index: 0, Addr: "10.0.0.9:1080", State: connect.ProxyCellUp, Clients: 4, IntervalBytes: 1 << 20, AgeSeconds: 3600},
	}

	lines := m.proxyCellInfo(0)
	joined := ""
	for _, l := range lines {
		joined += l + "\n"
	}
	for _, want := range []string{"proxy[0] 10.0.0.9:1080", "state up", "clients 4", "bytes in interval 1048576", "age 1h"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("proxyCellInfo missing %q:\n%s", want, joined)
		}
	}
}

func TestProxyCellInfoWithoutDetailShowsUnknown(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.proxyCells = makeProxyCells([]uint8{connect.ProxyCellUp})
	m.proxyDetail = nil

	lines := m.proxyCellInfo(0)
	joined := ""
	for _, l := range lines {
		joined += l + "\n"
	}
	for _, want := range []string{"proxy[0] ?", "state up", "age ?"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("proxyCellInfo missing %q:\n%s", want, joined)
		}
	}
}

func TestProxyCellInfoOutOfRange(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.proxyCells = makeProxyCells([]uint8{connect.ProxyCellUp})
	if got := m.proxyCellInfo(1); got != nil {
		t.Fatalf("out of range index should return nil, got %v", got)
	}
	if got := m.proxyCellInfo(-1); got != nil {
		t.Fatalf("negative index should return nil, got %v", got)
	}
}

// topHeatGridModel builds a model whose provider answered proxy_cells with n
// synthetic proxies in a deterministic state pattern, so the goldens show a
// stable mix of every state.
func topHeatGridModel(t *testing.T, th tui.Theme, n int) *topModel {
	t.Helper()
	clock := &fakeClock{t: topBase}
	m := newTestModel(th, topProviders(1), clock)
	s := topFlowing(t)
	s.Proxies = SnapshotProxies{Up: n}
	m.apply(0, s, nil)
	states := make([]uint8, n)
	for i := range states {
		states[i] = connect.ProxyCellUp
	}
	for i := 0; i < n; i++ {
		switch i % 50 {
		case 7:
			states[i] = connect.ProxyCellDead
		case 13:
			states[i] = connect.ProxyCellDegraded
		case 21:
			states[i] = connect.ProxyCellConnecting
		case 33:
			states[i] = connect.ProxyCellParked
		}
	}
	m.proxyCells = makeProxyCells(states)
	return m
}

func TestTopProxyGridGoldens(t *testing.T) {
	for _, n := range []int{900, 4400} {
		for _, sz := range []struct{ w, h int }{{100, 30}, {120, 40}, {150, 40}} {
			name := fmt.Sprintf("top_proxy_grid_%dx%d_%d", sz.w, sz.h, n)
			t.Run(name, func(t *testing.T) {
				m := topHeatGridModel(t, tui.DefaultTheme(), n)
				got, _ := showOnSim(t, m, sz.w, sz.h)
				assertGolden(t, name, got)
			})
		}
	}
}

// The mono theme renders the grid with ASCII glyphs (. o x -), which makes the
// state-to-glyph mapping visible in the golden text.
func TestTopProxyGridMonoGolden(t *testing.T) {
	m := topHeatGridModel(t, tui.MonoTheme(), 900)
	got, _ := showOnSim(t, m, 120, 40)
	assertGolden(t, "top_proxy_grid_mono_120x40_900", got)
}

func TestProxiesBoxMode(t *testing.T) {
	cases := []struct {
		h, w int
		want int
	}{
		{3, 20, proxyBoxBars}, // too short
		{7, 20, proxyBoxBars}, // one row short of the minimum
		{8, 20, proxyBoxGrid}, // minimum height
		{8, 19, proxyBoxBars}, // one column short
		{8, 20, proxyBoxGrid},
		{20, 80, proxyBoxGrid},
		{0, 0, proxyBoxBars},
	}
	for _, c := range cases {
		if got := proxiesBoxMode(c.h, c.w); got != c.want {
			t.Errorf("proxiesBoxMode(%d, %d) = %d, want %d", c.h, c.w, got, c.want)
		}
	}
}
