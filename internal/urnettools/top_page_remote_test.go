package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

// The dashboard is still the frame it always was, even with a second page
// registered: no strip row while page 1 is on screen.
func TestTopDashboardKeepsTheFrameWithPagesRegistered(t *testing.T) {
	m, _, text := pageModel(t, 120, 40)
	if pages, _ := m.pageStrip(m.pageArea()); len(pages) < 2 {
		t.Fatalf("the fixture should offer both pages, got %v", pages)
	}
	if m.contentRect.Y != 1 || m.contentRect.H != 38 {
		t.Fatalf("content rect = %+v, want the full band at y=1 h=38", m.contentRect)
	}
	for _, label := range []string{"Overview", "Remote", "[/] page"} {
		if strings.Contains(text, label) {
			t.Fatalf("the dashboard frame grew %q; it must stay as it was:\n%s", label, text)
		}
	}
}

// The strip draws both tabs, in order, with the current page marked and the
// content starting below it.
func TestTopPageStripDrawsBothTabs(t *testing.T) {
	m, b, _ := pageModel(t, 120, 40)
	m.page = 1
	renderFresh(m, b)
	lines := strings.Split(b.String(), "\n")
	if len(lines) < 3 {
		t.Fatalf("frame too short: %d lines", len(lines))
	}
	strip := lines[1]
	if !strings.Contains(strip, "[1 Overview]") || !strings.Contains(strip, "[2 Remote]") {
		t.Fatalf("strip line %q is missing a tab", strip)
	}
	if strings.Index(strip, "[1 Overview]") > strings.Index(strip, "[2 Remote]") {
		t.Fatalf("tabs are out of order: %q", strip)
	}
	if m.contentRect.Y != 2 || m.contentRect.H != 37 {
		t.Fatalf("content rect = %+v, want y=2 h=37 under the strip", m.contentRect)
	}
	// The current tab is drawn in reverse video and the other one is not.
	remote := regionRect(t, m, "page:remote")
	if got := b.Cell(remote.X, remote.Y).Style.Attrs; got&tui.AttrReverse == 0 {
		t.Fatalf("current tab style attrs = %v, want reverse", got)
	}
	overview := regionRect(t, m, "page:overview")
	if got := b.Cell(overview.X, overview.Y).Style.Attrs; got&tui.AttrReverse != 0 {
		t.Fatalf("the other tab must not be reversed")
	}
}

// A theme with no colour and no attributes marks the current tab with a star
// instead, and carries no attributes anywhere.
func TestTopPageStripMonoAndASCII(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.MonoTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	m.page = 1
	b := tui.New(100, 30)
	renderFresh(m, b)
	strip := strings.Split(b.String(), "\n")[1]
	if !strings.Contains(strip, "*[2 Remote]") {
		t.Fatalf("mono strip %q must mark the current tab with a star", strip)
	}
	if !strings.Contains(strip, "[1 Overview]") {
		t.Fatalf("mono strip %q lost the other tab", strip)
	}
	for y := 0; y < b.Height(); y++ {
		for x := 0; x < b.Width(); x++ {
			if a := b.Cell(x, y).Style.Attrs; a != 0 {
				t.Fatalf("cell (%d,%d) carries attrs %v in the mono theme", x, y, a)
			}
		}
	}
}

// The Remote page draws the transfer chart and none of the dashboard's other
// panels, and a provider with no history never offers the page.
func TestTopRemotePage(t *testing.T) {
	m, b, _ := pageModel(t, 120, 40)
	m.page = 1
	renderFresh(m, b)
	text := b.String()
	for _, want := range []string{"Remote", "billable", "total"} {
		if !strings.Contains(text, want) {
			t.Fatalf("page 2 is missing %q:\n%s", want, text)
		}
	}
	for _, unwanted := range []string{"Proxies", "Events", "Resources"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("page 2 drew the dashboard's %s panel:\n%s", unwanted, text)
		}
	}
	if regionByName(m.regions, "box:remote") == nil {
		t.Fatalf("page 2 recorded no hit region")
	}

	// No snapshot: the page is not offered at all.
	clock := &fakeClock{t: topBase}
	empty := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	b2 := tui.New(120, 40)
	renderFresh(empty, b2)
	if pages, _ := empty.pageStrip(empty.pageArea()); len(pages) != 1 {
		t.Fatalf("a provider with no history offers %v, want the dashboard only", pages)
	}
}
