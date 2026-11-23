package urnettools

import (
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

// rowDots counts the non-blank cells of one row.
func rowDots(b *tui.Buffer, y int) int {
	n := 0
	for x := 0; x < b.Width(); x++ {
		if r := b.Cell(x, y).Rune; r != ' ' && r != 0 && r != 0x2800 {
			n++
		}
	}
	return n
}

// A value of zero lies on the center line, as in the client app: an idle
// chart is one line, not a band of three.
func TestRemoteZeroLiesOnTheCenterLine(t *testing.T) {
	c := remoteTestChart(remoteFlat(60, 0), remoteFlat(60, 0))
	b := tui.New(60, 15)
	drawRemoteChart(b, c)
	l := remoteLayout(15)
	if n := rowDots(b, l.center-1) + rowDots(b, l.center+1); n != 0 {
		t.Fatalf("an idle chart draws %d dots beside the center line:\n%s", n, b.String())
	}

	// A burst still rises from the axis and comes back to it.
	c = remoteTestChart(remoteSpike(60, 30, 1<<20, 0), remoteSpike(60, 30, 1<<20, 0))
	b = tui.New(60, 15)
	drawRemoteChart(b, c)
	up, down := rowDots(b, l.center-1), rowDots(b, l.center+1)
	if up == 0 || down == 0 {
		t.Fatalf("a burst must touch the center line on both sides:\n%s", b.String())
	}
	if up > 10 || down > 10 {
		t.Fatalf("only the burst may touch the center line, got %d and %d cells:\n%s", up, down, b.String())
	}
}

// topmostDots is the first row from the top of b that carries a dot within
// rows [from, to), or to when none does.
func topmostDots(b *tui.Buffer, from, to int) int {
	for y := from; y < to; y++ {
		if rowDots(b, y) > 0 {
			return y
		}
	}
	return to
}

// Background noise stays near the axis. Like the client app, a byte scale
// never goes below 1 KiB/s and a packet scale never below 8 pkt/s, so a few
// bytes or packets do not fill a half.
func TestRemoteScaleFloors(t *testing.T) {
	noise := make([]float64, 60)
	for i := range noise {
		noise[i] = float64(20 + (i%3)*30) // 20 to 80 B/s
	}
	c := remoteTestChart(noise, nil)
	b := tui.New(60, 15)
	drawRemoteChart(b, c)
	l := remoteLayout(15)
	// 80 B/s of a 1 KiB/s scale is under a tenth of the half.
	if top := topmostDots(b, l.upY, l.center); top < l.center-1 {
		t.Fatalf("80 B/s of noise reaches row %d of a half that starts at %d:\n%s", top, l.upY, b.String())
	}

	pkts := remoteFlat(60, 2)
	c = remoteTestChart(remoteFlat(60, 0), nil)
	c.Up = append(c.Up, remoteSeries{Style: remotePink, Group: "billable packets", Packets: true, Values: pkts})
	b = tui.New(60, 15)
	drawRemoteChart(b, c)
	// 2 pkt/s of an 8 pkt/s scale is a quarter of the half.
	rows := l.center - l.upY
	if top := topmostDots(b, l.upY, l.center); top < l.center-(rows+3)/4-1 {
		t.Fatalf("2 pkt/s reaches row %d of a half from %d to %d:\n%s", top, l.upY, l.center, b.String())
	}

	// Above the floor the scale is the window peak, as before.
	c = remoteTestChart(remoteSpike(60, 30, 4096, 0), nil)
	b = tui.New(60, 15)
	drawRemoteChart(b, c)
	if top := topmostDots(b, l.upY, l.center); top != l.upY {
		t.Fatalf("a 4 KiB/s peak must reach the top of its half, reached row %d:\n%s", top, b.String())
	}
}

// Only the byte curve has a fill under it, as in the client app. The packet
// curve is a line, so where the two overlap the byte area stays readable.
func TestRemotePacketCurveHasNoFill(t *testing.T) {
	c := remoteTestChart(remoteSpike(60, 20, 64*1024, 0), nil)
	c.Up = append(c.Up, remoteSeries{Style: remotePink, Group: "billable packets", Packets: true, Values: remoteSpike(60, 40, 200, 0)})
	b := tui.New(60, 15)
	drawRemoteChart(b, c)
	var greenFill, pinkFill, pinkLine int
	for y := 0; y < b.Height(); y++ {
		for x := 0; x < b.Width(); x++ {
			st := b.Cell(x, y).Style
			dim := st.Attrs&tui.AttrDim != 0
			switch {
			case st.FG == remoteGreen.FG && dim:
				greenFill++
			case st.FG == remotePink.FG && dim:
				pinkFill++
			case st.FG == remotePink.FG:
				pinkLine++
			}
		}
	}
	if greenFill == 0 || pinkLine == 0 {
		t.Fatalf("expected a byte fill and a packet line, got %d and %d:\n%s", greenFill, pinkLine, b.String())
	}
	if pinkFill != 0 {
		t.Fatalf("the packet curve has %d fill cells:\n%s", pinkFill, b.String())
	}
}
