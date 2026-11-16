package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

func regionByName(regions []topRegion, name string) *topRegion {
	for i := range regions {
		if regions[i].Name == name {
			return &regions[i]
		}
	}
	return nil
}

func regionNames(regions []topRegion) []string {
	names := make([]string, len(regions))
	for i, r := range regions {
		names[i] = r.Name
	}
	return names
}

func TestHitRegionPrefersTheMostRecent(t *testing.T) {
	regions := []topRegion{
		{Name: "box:graph", Rect: tui.Rect{X: 0, Y: 0, W: 10, H: 4}},
		{Name: "title:graph", Rect: tui.Rect{X: 0, Y: 0, W: 10, H: 1}},
	}
	if got := hitRegion(regions, 3, 0); got != "title:graph" {
		t.Fatalf("got %q", got)
	}
	if got := hitRegion(regions, 3, 3); got != "box:graph" {
		t.Fatalf("got %q", got)
	}
	if got := hitRegion(regions, 3, 4); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestHitRegionMissesOutside(t *testing.T) {
	regions := []topRegion{
		{Name: "box:graph", Rect: tui.Rect{X: 2, Y: 2, W: 5, H: 3}},
	}
	for _, p := range [][2]int{{0, 0}, {2, 1}, {1, 2}, {7, 2}, {2, 5}, {7, 4}, {0, 4}} {
		if got := hitRegion(regions, p[0], p[1]); got != "" {
			t.Fatalf("point %v mapped to %q", p, got)
		}
	}
}

// A simulated press at each recorded region must land on it, with titles and
// dividers winning over the box around them.
func TestTopFrameRecordsHitRegions(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(3), clock)
	m.apply(0, topFlowing(t), nil)
	m.extender = ExtenderReading{
		Source: SourceControlSocket, Known: true, Setting: "on", Running: true, RoleState: "serving",
		Families: []ExtenderFamily{},
	}
	b := tui.New(120, 40)
	m.render(b)

	need := []string{
		"box:graph", "title:graph", "box:proxies", "title:proxies", "box:events", "title:events",
		"box:now", "title:now", "box:resources", "title:resources",
		"side", "left:proxies", "left:events", "side:resources",
		"footer:quit", "footer:provider", "footer:help", "footer:menu", "footer:zoom", "footer:interval", "footer:mouse",
		"tab:provider",
	}
	for _, n := range need {
		if regionByName(m.regions, n) == nil {
			t.Fatalf("region %q not recorded; got %s", n, strings.Join(regionNames(m.regions), ", "))
		}
	}

	// The hit map is consistent: every recorded region is reachable, and the
	// title row of a box maps to the title, not the box around it.
	for _, r := range m.regions {
		x := r.Rect.X + r.Rect.W/2
		y := r.Rect.Y + r.Rect.H/2
		got := hitRegion(m.regions, x, y)
		if got == r.Name {
			continue
		}
		if key, ok := strings.CutPrefix(r.Name, "box:"); ok && got == "title:"+key {
			continue
		}
		if key, ok := strings.CutPrefix(r.Name, "title:"); ok && got == "box:"+key {
			continue
		}
		t.Fatalf("region %q center %v mapped to %q", r.Name, [2]int{x, y}, got)
	}

	// The title row of the graph box is the title region, not the box.
	graph := regionByName(m.regions, "box:graph")
	if got := hitRegion(m.regions, graph.Rect.X+graph.Rect.W/2, graph.Rect.Y); got != "title:graph" {
		t.Fatalf("graph top row mapped to %q", got)
	}
}

func TestTopFrameDividerBeatsBoxOnItsRow(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	b := tui.New(120, 40)
	m.render(b)
	d := regionByName(m.regions, "left:proxies")
	if d == nil {
		t.Fatal("divider left:proxies not recorded")
	}
	if got := hitRegion(m.regions, d.Rect.X, d.Rect.Y); got != "left:proxies" {
		t.Fatalf("divider row mapped to %q", got)
	}
}

func TestFooterHintRectsFollowTheDrawOrder(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(2), clock)
	m.apply(0, topFlowing(t), nil)
	hints := m.footerHints()
	frects := footerHintRects(tui.Rect{X: 0, Y: 39, W: 120, H: 1}, hints)
	if len(frects) != len(hints) {
		t.Fatalf("got %d rects for %d hints", len(frects), len(hints))
	}
	// hints are laid out left to right with 3 spaces between them
	prev := 0
	for i, r := range frects {
		if i > 0 {
			prev = frects[i-1].Rect.X + frects[i-1].Rect.W
		}
		wantX := prev + 3
		if i == 0 {
			wantX = 1
		}
		if r.Rect.X != wantX {
			t.Fatalf("hint %d (%s) at x %d, want %d", i, hints[i].name, r.Rect.X, wantX)
		}
		if r.Rect.W != tui.StringWidth(hints[i].label) {
			t.Fatalf("hint %d (%s) width %d, want %d", i, hints[i].name, r.Rect.W, tui.StringWidth(hints[i].label))
		}
	}
}
