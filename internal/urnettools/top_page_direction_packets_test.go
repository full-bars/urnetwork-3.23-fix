package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

// packetsFixture is directionFixture with the billable packet block filled in.
// Packets do not follow bytes: the histories here run in the hundreds while the
// byte histories peak near 20 MiB/s, so a packet curve drawn on the byte scale
// would sit on the baseline and the tests would see that.
func packetsFixture(t *testing.T) *NodeSnapshot {
	t.Helper()
	s := directionFixture(t)
	bps := s.Traffic.Directions
	rx := make([]float64, len(bps.BillableRx.HistoryBps))
	tx := make([]float64, len(bps.BillableTx.HistoryBps))
	for i := range rx {
		rx[i] = 600 + float64((i*37)%160)
	}
	for i := range tx {
		tx[i] = 120 + float64((i*11)%40)
	}
	s.Traffic.Packets = &SnapshotTrafficPackets{
		BillableRx: SnapshotPacketRate{HistoryPps: rx, NowPps: rx[len(rx)-1]},
		BillableTx: SnapshotPacketRate{HistoryPps: tx, NowPps: tx[len(tx)-1]},
	}
	return s
}

// packetsPageModel renders one direction page over snap at the given size.
func packetsPageModel(t *testing.T, th tui.Theme, snap *NodeSnapshot, w, h int, id string) (*topModel, *tui.Buffer, string) {
	t.Helper()
	m := newTestModel(th, topProviders(1), &fakeClock{t: topBase})
	m.apply(0, snap, nil)
	m.page = pageByID(id)
	if m.page < 0 {
		t.Fatalf("no page with id %q", id)
	}
	b := tui.New(w, h)
	renderFresh(m, b)
	return m, b, b.String()
}

// styleCells returns the rows of the cells drawn in style, top to bottom, and
// how many there are.
func styleCells(b *tui.Buffer, style tui.Style) (rows []int, count int) {
	for y := 0; y < b.Height(); y++ {
		for x := 0; x < b.Width(); x++ {
			c := b.Cell(x, y)
			if c.Rune == ' ' || c.Rune == 0 || c.Style != style {
				continue
			}
			count++
			if len(rows) == 0 || rows[len(rows)-1] != y {
				rows = append(rows, y)
			}
		}
	}
	return rows, count
}

// The billable page at a wide, a narrow and the smallest full-layout size, with
// the packet block present.
func TestTopBillablePacketsGolden(t *testing.T) {
	sizes := []struct {
		name string
		w, h int
	}{
		{"120x40", 120, 40},
		{"80x24", 80, 24},
		{"72x20", 72, 20},
	}
	for _, sz := range sizes {
		_, _, text := packetsPageModel(t, tui.DefaultTheme(), packetsFixture(t), sz.w, sz.h, "billable")
		assertGolden(t, "top_page_billable_packets_"+sz.name, text)
	}
}

// The mono theme carries no color and draws ASCII, so the packet curve has to
// read by its glyph alone.
func TestTopBillablePacketsMonoGolden(t *testing.T) {
	_, _, text := packetsPageModel(t, mustTheme(t, "mono"), packetsFixture(t), 80, 24, "billable")
	assertGolden(t, "top_page_billable_packets_mono_80x24", text)
}

// A color theme on a terminal without block glyphs: the same page with the
// ASCII fallback and the colors kept.
func TestTopBillablePacketsASCIIGolden(t *testing.T) {
	th := tui.DefaultTheme()
	th.ASCII = true
	_, _, text := packetsPageModel(t, th, packetsFixture(t), 120, 40, "billable")
	assertGolden(t, "top_page_billable_packets_ascii_120x40", text)
}

// The packet curve draws with its own glyph in the ASCII fallback, so it stays
// apart from the byte curve with no color to tell them apart.
func TestTopPacketSeriesASCIIGlyph(t *testing.T) {
	count := func(snap *NodeSnapshot) int {
		th := tui.DefaultTheme()
		th.ASCII = true
		m, b, _ := packetsPageModel(t, th, snap, 120, 40, "billable")
		r := regionByName(m.regions, "box:billable")
		if r == nil {
			t.Fatal("the page recorded no hit region")
		}
		n := 0
		for y := r.Rect.Y + 1; y < r.Rect.Y+r.Rect.H-1; y++ {
			for x := r.Rect.X + 1; x < r.Rect.X+r.Rect.W-1; x++ {
				if b.Cell(x, y).Rune == '+' {
					n++
				}
			}
		}
		return n
	}
	if without := count(directionFixture(t)); without != 0 {
		t.Fatalf("the page without packets drew %d packet glyphs", without)
	}
	if with := count(packetsFixture(t)); with == 0 {
		t.Fatal("the page with packets drew no packet glyphs")
	}
}

// Bytes and packets are two scale groups, so each curve reaches its own peak.
// Drawn on the byte scale the packet curve would sit on the baseline.
func TestTopPacketSeriesScalesOnItsOwnPeak(t *testing.T) {
	s := packetsFixture(t)
	th := tui.DefaultTheme()
	m, b, _ := packetsPageModel(t, th, s, 120, 40, "billable")
	_, _, pUp, pDown, _, interval, ok := m.directionData(true)
	if !ok {
		t.Fatal("direction data missing")
	}
	for _, c := range []struct {
		group string
		vals  []float64
	}{
		{"billable upload packets", pUp},
		{"billable download packets", pDown},
	} {
		want := remoteWindowPeak(remoteChart{Interval: interval, Window: remoteDefaultWindow}, c.vals)
		if want <= 0 {
			t.Fatalf("%s: no packet peak in the fixture", c.group)
		}
		scale := m.directionScales[c.group]
		if scale == nil {
			t.Fatalf("%s: no scale state", c.group)
		}
		if got := scale.valueAt(m.now()); got-want > 1e-6 || want-got > 1e-6 {
			t.Fatalf("%s scales to %f, want the packet peak %f", c.group, got, want)
		}
	}
	bytesStyle, packetsStyle := remotePalette(th)
	byteRows, byteCells := styleCells(b, bytesStyle)
	pktRows, pktCells := styleCells(b, packetsStyle)
	if byteCells == 0 || pktCells == 0 {
		t.Fatalf("curves are missing: bytes %d cells, packets %d cells", byteCells, pktCells)
	}
	// Both curves reach their own peak, so their top rows are close. One shared
	// scale would leave the packet curve near the center line, half a page away.
	if d := byteRows[0] - pktRows[0]; d > 3 || d < -3 {
		t.Fatalf("the packet curve peaks %d rows from the byte curve: %v vs %v", d, pktRows, byteRows)
	}
}

// The corner figures carry both directions the way the client app does: bytes
// then packets, each in its own color, then the arrow. The packet part is
// muted when the direction carries no packets.
func TestTopCornerFiguresCarryPackets(t *testing.T) {
	s := packetsFixture(t)
	th := tui.DefaultTheme()
	m, b, text := packetsPageModel(t, th, s, 120, 40, "billable")
	_, _, pUp, pDown, _, _, _ := m.directionData(true)
	_, packetsStyle := remotePalette(th)
	for _, c := range []struct {
		name string
		vals []float64
		fig  string
	}{
		{"upload", s.Traffic.Directions.BillableTx.HistoryBps, tui.PacketRate(remoteAverage(pUp)) + "  ▲"},
		{"download", s.Traffic.Directions.BillableRx.HistoryBps, tui.PacketRate(remoteAverage(pDown)) + "  ▼"},
	} {
		bytesFig := tui.Rate(remoteAverage(c.vals))
		if !strings.Contains(text, bytesFig+"  "+c.fig) {
			t.Fatalf("%s: the corner has no %q after %q:\n%s", c.name, c.fig, bytesFig, text)
		}
	}
	// The packet figure is painted in the packet color on the averages row.
	found := false
	want := tui.PacketRate(remoteAverage(pUp))
Want:
	for y := 0; y < b.Height(); y++ {
		line := make([]rune, 0, b.Width())
		for x := 0; x < b.Width(); x++ {
			line = append(line, b.Cell(x, y).Rune)
		}
		row := string(line)
		at := strings.Index(row, want)
		if at < 0 {
			continue
		}
		start := len([]rune(row[:at]))
		for x := start; x < start+len([]rune(want)); x++ {
			if c := b.Cell(x, y); c.Style != packetsStyle {
				t.Fatalf("the packet figure cell %d,%d is %+v, want the packet color", x, y, c.Style)
			}
		}
		found = true
		break Want
	}
	if !found {
		t.Fatalf("the packet figure is missing from the corners:\n%s", text)
	}
}

// Zero packets: the corner says so in the muted style and no pink dot is drawn.
func TestTopZeroPackets(t *testing.T) {
	s := packetsFixture(t)
	zero := make([]float64, len(s.Traffic.Directions.BillableRx.HistoryBps))
	s.Traffic.Packets = &SnapshotTrafficPackets{
		BillableRx: SnapshotPacketRate{HistoryPps: zero},
		BillableTx: SnapshotPacketRate{HistoryPps: zero},
	}
	th := tui.DefaultTheme()
	_, b, text := packetsPageModel(t, th, s, 120, 40, "billable")
	if !strings.Contains(text, "0 pkt/s") {
		t.Fatalf("the corner has no zero packet figure:\n%s", text)
	}
	_, packetsStyle := remotePalette(th)
	if rows, cells := styleCells(b, packetsStyle); cells != 0 {
		t.Fatalf("zero packets drew %d colored cells on rows %v", cells, rows)
	}
}

// A packet history that rose and then fell shows the newest average in the
// corner, not the window peak, and the peak label stays bytes.
func TestTopPacketDecrease(t *testing.T) {
	s := packetsFixture(t)
	rx := make([]float64, len(s.Traffic.Directions.BillableRx.HistoryBps))
	for i := range rx {
		rx[i] = 5000
	}
	for i := len(rx) - 5; i < len(rx); i++ {
		rx[i] = 10
	}
	s.Traffic.Packets.BillableRx = SnapshotPacketRate{HistoryPps: rx, NowPps: 10}
	_, _, text := packetsPageModel(t, tui.DefaultTheme(), s, 120, 40, "billable")
	if !strings.Contains(text, "10 pkt/s") {
		t.Fatalf("the corner lost the newest packet average:\n%s", text)
	}
	if strings.Contains(text, "5.0k pkt/s") {
		t.Fatalf("the corner reports the packet peak instead of the newest average:\n%s", text)
	}
}

// The total page counts socket reads, not packets, so it draws no packet curve
// and says nothing about packets even when the provider reports them.
func TestTopTotalPageHasNoPackets(t *testing.T) {
	th := tui.DefaultTheme()
	_, b, text := packetsPageModel(t, th, packetsFixture(t), 120, 40, "total")
	if strings.Contains(text, "pkt/s") {
		t.Fatalf("the total page reported packets:\n%s", text)
	}
	_, packetsStyle := remotePalette(th)
	if rows, cells := styleCells(b, packetsStyle); cells != 0 {
		t.Fatalf("the total page drew %d packet cells on rows %v", cells, rows)
	}
}

// A provider without the packet block keeps the billable page exactly as it was
// before the series existed: the goldens are unchanged byte for byte.
func TestTopBillableWithoutPacketsKeepsTheGoldens(t *testing.T) {
	for _, sz := range []struct {
		name string
		w, h int
	}{
		{"120x40", 120, 40},
		{"80x24", 80, 24},
		{"72x20", 72, 20},
	} {
		_, _, text := directionPageModel(t, sz.w, sz.h, "billable")
		assertGolden(t, "top_page_billable_"+sz.name, text)
	}
}

// directionData hands the page the billable packet histories on the billable
// page, mapped the same way as the bytes, and nothing on the total page.
func TestTopDirectionDataReturnsPacketsForBillableOnly(t *testing.T) {
	s := packetsFixture(t)
	m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	m.apply(0, s, nil)
	_, _, pUp, pDown, _, _, ok := m.directionData(true)
	if !ok {
		t.Fatal("billable direction data missing")
	}
	if len(pUp) != len(s.Traffic.Packets.BillableTx.HistoryPps) || pUp[0] != s.Traffic.Packets.BillableTx.HistoryPps[0] {
		t.Fatalf("upload packets are not the billable tx history")
	}
	if len(pDown) != len(s.Traffic.Packets.BillableRx.HistoryPps) || pDown[0] != s.Traffic.Packets.BillableRx.HistoryPps[0] {
		t.Fatalf("download packets are not the billable rx history")
	}
	_, _, tUp, tDown, _, _, _ := m.directionData(false)
	if tUp != nil || tDown != nil {
		t.Fatalf("the total page got packet histories: %d %d", len(tUp), len(tDown))
	}
}

// A row too narrow for both figures drops the packet figure first: the bytes
// reading and the arrow stay, and both figures appear again when the row fits.
func TestTopCornerDropsPacketsBeforeBytes(t *testing.T) {
	c := remoteTestChart(remoteFlat(60, 4096), nil)
	c.Up = append(c.Up, remoteSeries{
		Group:   "billable packets",
		Glyph:   '+',
		Packets: true,
		Values:  remoteFlat(60, 120),
	})
	narrow := remoteRender(c, 16, 8)
	if !strings.Contains(narrow, tui.Rate(4096)) {
		t.Fatalf("the narrow row lost the bytes figure:\n%s", narrow)
	}
	if strings.Contains(narrow, "pkt/s") {
		t.Fatalf("the narrow row kept the packet figure:\n%s", narrow)
	}
	wide := remoteRender(c, 60, 8)
	if !strings.Contains(wide, tui.PacketRate(120)) {
		t.Fatalf("the wide row lost the packet figure:\n%s", wide)
	}
}
