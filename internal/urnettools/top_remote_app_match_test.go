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
