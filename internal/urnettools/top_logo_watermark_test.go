package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/brand"
	"github.com/urnetwork/connect/internal/tui"
)

// watermarkCells counts the cells of b that carry a glyph of the 12x6 icon in
// the dim style, and the cells that carry one in any other style.
func watermarkCells(b *tui.Buffer, dim tui.Style) (inDim, other int) {
	for y := 0; y < b.Height(); y++ {
		for x := 0; x < b.Width(); x++ {
			c := b.Cell(x, y)
			if c.Rune != '█' && c.Rune != '▄' && c.Rune != '▀' {
				continue
			}
			if c.Style == dim {
				inDim++
			} else {
				other++
			}
		}
	}
	return inDim, other
}

// iconCells is the number of non-blank cells in the mono 12x6 icon.
func iconCells() int {
	n := 0
	for _, row := range brand.Grid(brand.Assets["icon-flat-12x6"], brand.Capabilities{Depth: brand.Mono, NoColor: true, UTF8: true}) {
		for _, c := range row {
			if c.Glyph != ' ' {
				n++
			}
		}
	}
	return n
}

// watermarkModel is a model with the logo allowed and the splash already over.
func watermarkModel(t *testing.T, th tui.Theme) *topModel {
	t.Helper()
	m, _ := logoTopModel(t, th, 1, 120, 40, nil)
	m.enableLogo(false, false)
	m.splash.done = true
	return m
}

func TestTopWatermarkInAnEmptyGraph(t *testing.T) {
	for _, name := range []string{"default", "neon", "high-contrast"} {
		th := mustTheme(t, name)
		m := watermarkModel(t, th)
		b := tui.New(120, 40)
		renderFresh(m, b)
		inDim, other := watermarkCells(b.Sub(regionRect(t, m, "box:graph")), th.Dim)
		if inDim != iconCells() || other != 0 {
			t.Fatalf("%s: empty graph has %d dim icon cells and %d others, want %d and 0:\n%s", name, inDim, other, iconCells(), b.String())
		}
		if !strings.Contains(b.String(), "waiting for the provider") {
			t.Fatalf("%s: the empty state text must stay:\n%s", name, b.String())
		}

		// The first data takes the watermark away.
		m.apply(m.gen, topFlowing(t), nil)
		renderFresh(m, b)
		if inDim, _ := watermarkCells(b.Sub(regionRect(t, m, "box:graph")), th.Dim); inDim != 0 {
			t.Fatalf("%s: the watermark must go once data is drawn:\n%s", name, b.String())
		}
	}
}

func TestTopWatermarkInAWindowWithNoReadings(t *testing.T) {
	th := tui.DefaultTheme()
	m := watermarkModel(t, th)
	m.apply(m.gen, topFlowing(t), nil)
	b := tui.New(60, 14)
	m.drawGraph(b, topSeries{window: true}, nil, nil, 0, th.Graph)
	if !strings.Contains(b.String(), "collecting readings") {
		t.Fatalf("expected the empty window state:\n%s", b.String())
	}
	if inDim, other := watermarkCells(b, th.Dim); inDim != iconCells() || other != 0 {
		t.Fatalf("an empty window must carry the watermark:\n%s", b.String())
	}
}

func TestTopWatermarkOnEmptyPages(t *testing.T) {
	th := tui.DefaultTheme()
	rect := tui.Rect{W: 100, H: 30}
	draws := map[string]func(m *topModel, b *tui.Buffer){
		"remote":     func(m *topModel, b *tui.Buffer) { m.drawRemotePage(b, rect) },
		"billable":   func(m *topModel, b *tui.Buffer) { m.drawBillablePage(b, rect) },
		"total":      func(m *topModel, b *tui.Buffer) { m.drawTotalPage(b, rect) },
		"transports": func(m *topModel, b *tui.Buffer) { m.drawTransportsPage(b, rect) },
	}
	for name, draw := range draws {
		m := watermarkModel(t, th)
		b := tui.New(rect.W, rect.H)
		draw(m, b)
		if inDim, other := watermarkCells(b, th.Dim); inDim != iconCells() || other != 0 {
			t.Errorf("%s: empty page has %d dim icon cells and %d others, want %d and 0:\n%s", name, inDim, other, iconCells(), b.String())
		}
	}

	// The Transports page keeps the watermark until its first window is
	// complete, and drops it on the window.
	m := watermarkModel(t, th)
	prev, cur := mixedWindow()
	m.transportsHistory.push(transportTestBase, prev)
	b := tui.New(rect.W, rect.H)
	m.drawTransportsPage(b, rect)
	if inDim, _ := watermarkCells(b, th.Dim); inDim != iconCells() {
		t.Fatalf("a Transports page with one reading must carry the watermark:\n%s", b.String())
	}
	m.transportsHistory.push(transportTestBase+nanos(60e9), cur)
	b = tui.New(rect.W, rect.H)
	m.drawTransportsPage(b, rect)
	if inDim, _ := watermarkCells(b, th.Dim); inDim != 0 {
		t.Fatalf("a Transports page with a window must not carry the watermark:\n%s", b.String())
	}
}

func TestTopWatermarkSizeLimits(t *testing.T) {
	th := tui.DefaultTheme()
	m := watermarkModel(t, th)
	// The icon is 12 by 6 and sits under the empty state line, so a plot
	// needs 12 columns and 7 rows.
	for _, size := range [][2]int{{11, 20}, {40, 6}, {5, 3}} {
		b := tui.New(size[0], size[1])
		m.drawWatermark(b, 1)
		if inDim, _ := watermarkCells(b, th.Dim); inDim != 0 {
			t.Errorf("%dx%d: a plot too small for the icon must not carry it:\n%s", size[0], size[1], b.String())
		}
	}
	b := tui.New(12, 7)
	m.drawWatermark(b, 1)
	if inDim, _ := watermarkCells(b, th.Dim); inDim != iconCells() {
		t.Fatalf("12x7 holds the icon:\n%s", b.String())
	}
	// Centered: equal space on the left and the right of a wider plot.
	b = tui.New(40, 13)
	m.drawWatermark(b, 1)
	lines := strings.Split(b.String(), "\n")
	// The plot rows start at 1, the icon three rows lower, and its third row
	// is the first one that is full width.
	row := lines[1+3+2]
	if left := len(row) - len(strings.TrimLeft(row, " ")); left != 14 {
		t.Fatalf("the icon must be centered, its widest row starts at %d:\n%s", left, b.String())
	}
}

func TestTopWatermarkNeverWhenTheLogoIsOff(t *testing.T) {
	th := tui.DefaultTheme()
	off := watermarkModel(t, th)
	off.logo = false
	mono := watermarkModel(t, tui.MonoTheme())
	for name, m := range map[string]*topModel{"logo off": off, "mono": mono} {
		b := tui.New(120, 40)
		renderFresh(m, b)
		if inDim, other := watermarkCells(b, m.theme.Dim); inDim+other != 0 {
			t.Errorf("%s: no watermark expected:\n%s", name, b.String())
		}
	}
}
