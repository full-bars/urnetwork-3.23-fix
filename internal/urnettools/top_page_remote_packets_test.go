package urnettools

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// remotePacketCells counts the cells of b in the pink packet color, and how
// many of those carry the dim fill attribute.
func remotePacketCells(b *tui.Buffer, th tui.Theme) (line, fill int) {
	_, pink := remotePalette(th)
	for y := 0; y < b.Height(); y++ {
		for x := 0; x < b.Width(); x++ {
			c := b.Cell(x, y)
			if c.Rune == ' ' || c.Rune == 0 || c.Style.FG != pink.FG {
				continue
			}
			if c.Style.Attrs&tui.AttrDim != 0 {
				fill++
			} else {
				line++
			}
		}
	}
	return line, fill
}

// remotePageWith renders page 2 over snap at the given size.
func remotePageWith(t *testing.T, snap *NodeSnapshot, w, h int) (*topModel, *tui.Buffer, string) {
	t.Helper()
	return packetsPageModel(t, tui.DefaultTheme(), snap, w, h, "remote")
}

// With the packet block, page 2 is the client app's chart: upload above,
// download below, a green byte curve and a pink packet curve in each half.
func TestTopRemotePageDrawsPackets(t *testing.T) {
	m, b, text := remotePageWith(t, packetsFixture(t), 120, 40)
	for _, want := range []string{"upload ▲", "download ▼", "pkt/s"} {
		if !strings.Contains(text, want) {
			t.Fatalf("page 2 with packets is missing %q:\n%s", want, text)
		}
	}
	box := regionRect(t, m, "box:remote")
	l := remoteLayout(box.W-2, box.H-2)
	center := box.Y + 1 + l.center
	upper := b.Sub(tui.Rect{X: box.X, Y: box.Y, W: box.W, H: center - box.Y})
	lower := b.Sub(tui.Rect{X: box.X, Y: center + 1, W: box.W, H: box.Y + box.H - center - 1})
	for name, half := range map[string]*tui.Buffer{"upload": upper, "download": lower} {
		line, fill := remotePacketCells(half, m.theme)
		if line == 0 {
			t.Errorf("the %s half has no packet curve:\n%s", name, text)
		}
		if fill != 0 {
			t.Errorf("the %s packet curve has %d fill cells", name, fill)
		}
	}
}

// A provider without the packet block keeps page 2 as it was: billable above,
// total below, bytes only.
func TestTopRemotePageWithoutPacketsIsUnchanged(t *testing.T) {
	for name, snap := range map[string]*NodeSnapshot{"no directions": topFlowing(t), "directions only": directionFixture(t)} {
		m, b, text := remotePageWith(t, snap, 120, 40)
		if !strings.Contains(text, "billable ▲") || !strings.Contains(text, "total ▼") || strings.Contains(text, "pkt/s") {
			t.Fatalf("%s: page 2 must keep billable over total:\n%s", name, text)
		}
		if line, _ := remotePacketCells(b, m.theme); line != 0 {
			t.Fatalf("%s: page 2 without packets draws %d pink cells", name, line)
		}
	}
}

// liveReading is one traffic reply with the direction and packet counters.
func liveReading(at time.Time, i int, packets bool) *LiveTraffic {
	u := func(v int) *uint64 { x := uint64(v); return &x }
	lt := &LiveTraffic{AtUnixNano: at.UnixNano(), BillableBytes: uint64(i) * 6000, TotalBytes: uint64(i) * 9000,
		BillableTxBytes: u(i * 1000), BillableRxBytes: u(i * 5000), TotalTxBytes: u(i * 2000), TotalRxBytes: u(i * 7000)}
	if packets {
		lt.BillableTxPackets, lt.BillableRxPackets = u(i*20), u(i*90)
	}
	return lt
}

// The live 1 s ring carries the packet rates beside the byte rates, so page 2
// draws them once the traffic polls run, and not when the reply has none.
func TestTopLiveRingCarriesPackets(t *testing.T) {
	for _, packets := range []bool{true, false} {
		clock := &fakeClock{t: topBase}
		m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
		m.interval = time.Second
		snap := directionFixture(t)
		m.apply(0, snap, nil)
		for i := 0; i < 30; i++ {
			clock.Advance(time.Second)
			m.applyTraffic(m.gen, liveReading(clock.Now(), i, packets), nil)
		}
		h := &m.trafficHistory
		if len(h.billable) == 0 {
			t.Fatal("the live ring is empty")
		}
		if len(h.upPackets) != len(h.billable) || len(h.downPackets) != len(h.billable) || len(h.upBytes) != len(h.billable) {
			t.Fatalf("packets=%v: the ring series are not aligned: %d %d %d %d", packets, len(h.billable), len(h.upBytes), len(h.upPackets), len(h.downPackets))
		}
		up, down := h.upPackets[len(h.upPackets)-1], h.downPackets[len(h.downPackets)-1]
		if packets && (up != 20 || down != 90) {
			t.Fatalf("newest packet rates = %v up, %v down, want 20 and 90", up, down)
		}
		if got := h.hasPackets(); got != packets {
			t.Fatalf("packets=%v: hasPackets = %v", packets, got)
		}
		m.page = pageByID("remote")
		b := tui.New(120, 40)
		renderFresh(m, b)
		line, _ := remotePacketCells(b, m.theme)
		if packets && line == 0 {
			t.Fatalf("page 2 on the live ring draws no packets:\n%s", b.String())
		}
		if !packets && line != 0 {
			t.Fatalf("page 2 on a reply without packets draws %d pink cells:\n%s", line, b.String())
		}
	}
}

// Page 2 with the packet block at a wide and a standard size, and in the
// ASCII theme.
func TestTopRemotePagePacketsGolden(t *testing.T) {
	for _, c := range []struct {
		name string
		th   tui.Theme
		w, h int
	}{
		{"120x40", tui.DefaultTheme(), 120, 40},
		{"80x24", tui.DefaultTheme(), 80, 24},
		{"ascii_80x24", tui.MonoTheme(), 80, 24},
	} {
		_, _, text := packetsPageModel(t, c.th, packetsFixture(t), c.w, c.h, "remote")
		assertGolden(t, "top_page_remote_packets_"+c.name, text)
	}
}
