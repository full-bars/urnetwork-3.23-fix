package urnettools

import (
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

// The page strip is on by default, on every page, and p hides and shows it for
// the session. These tests pin the band it takes, the row it gives back, the
// footer hints in both states, and the pages' own content inside the
// rectangle the frame hands them.

// stripFrame renders a model with a provider that reports the direction split,
// so four pages are on offer, at the given page and size.
func stripFrame(t *testing.T, w, h, page int) (*topModel, *tui.Buffer, *fakeClock) {
	t.Helper()
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, directionFixture(t), nil)
	m.page = page
	b := tui.New(w, h)
	renderFresh(m, b)
	return m, b, clock
}

// expireNotice moves the clock past the one-line message so the footer hints
// are drawn again.
func expireNotice(clock *fakeClock) {
	clock.t = clock.t.Add(topNoticeFor + time.Second)
}

func TestTopStripIsOnByDefaultOnEveryPage(t *testing.T) {
	for _, page := range []int{0, 1} {
		m, b, _ := stripFrame(t, 120, 40, page)
		if m.contentRect.Y != 2 || m.contentRect.H != 37 {
			t.Fatalf("page %d: content rect = %+v, want y=2 h=37 under the strip", page+1, m.contentRect)
		}
		text := b.String()
		for _, want := range []string{"[1 Overview]", "[2 Remote]", "[3 Billable]", "[4 Total]"} {
			if !strings.Contains(text, want) {
				t.Fatalf("page %d: the strip is missing %q:\n%s", page+1, want, text)
			}
		}
		for _, name := range []string{"page:overview", "page:remote", "page:billable", "page:total", "footer:pages", "footer:tabs"} {
			if regionByName(m.regions, name) == nil {
				t.Fatalf("page %d: region %q not recorded with the strip shown", page+1, name)
			}
		}
	}
}

// p hides the strip and gives its row back to the content, and p shows it
// again, from page 1 and from page 2. The footer hints stay either way, and
// the one-line message says what happened while it lasts.
func TestTopStripKeyHidesAndShows(t *testing.T) {
	for _, page := range []int{0, 1} {
		m, b, clock := stripFrame(t, 120, 40, page)

		pressRune(m, 'p')
		if !m.stripHidden {
			t.Fatalf("page %d: p did not hide the strip", page+1)
		}
		if m.notice != topNoticeTabsHidden {
			t.Fatalf("page %d: message after hiding = %q", page+1, m.notice)
		}
		renderFresh(m, b)
		if !strings.Contains(b.String(), topNoticeTabsHidden) {
			t.Fatalf("page %d: the hide message is not on screen:\n%s", page+1, b.String())
		}
		if m.contentRect.Y != 1 || m.contentRect.H != 38 {
			t.Fatalf("page %d hidden: content rect = %+v, want the row back at y=1 h=38", page+1, m.contentRect)
		}
		if regionByName(m.regions, "page:overview") != nil {
			t.Fatalf("page %d hidden: a tab is still a hit region", page+1)
		}
		if strings.Contains(b.String(), "[1 Overview]") {
			t.Fatalf("page %d hidden: the strip is still drawn:\n%s", page+1, b.String())
		}

		// With the message gone the footer carries both hints.
		expireNotice(clock)
		renderFresh(m, b)
		for _, want := range []string{"[/] page", "p tabs"} {
			if !strings.Contains(b.String(), want) {
				t.Fatalf("page %d hidden: the footer is missing %q:\n%s", page+1, want, b.String())
			}
		}
		for _, name := range []string{"footer:pages", "footer:tabs"} {
			if regionByName(m.regions, name) == nil {
				t.Fatalf("page %d hidden: region %q not recorded", page+1, name)
			}
		}

		pressRune(m, 'p')
		if m.stripHidden {
			t.Fatalf("page %d: the second p did not show the strip", page+1)
		}
		if m.notice != topNoticeTabsShown {
			t.Fatalf("page %d: message after showing = %q", page+1, m.notice)
		}
		expireNotice(clock)
		renderFresh(m, b)
		if m.contentRect.Y != 2 || m.contentRect.H != 37 {
			t.Fatalf("page %d shown: content rect = %+v, want y=2 h=37", page+1, m.contentRect)
		}
		if regionByName(m.regions, "page:remote") == nil {
			t.Fatalf("page %d shown: the tabs are not clickable again", page+1)
		}
		for _, want := range []string{"[/] page", "p tabs"} {
			if !strings.Contains(b.String(), want) {
				t.Fatalf("page %d shown: the footer is missing %q:\n%s", page+1, want, b.String())
			}
		}
	}
}

// The band the strip takes at each size, shown and hidden, and the compact
// view below the full layout's minimum.
func TestTopStripBandHeightAtEachSize(t *testing.T) {
	full := []struct {
		w, h    int
		shownY  int
		shownH  int
		hiddenY int
		hiddenH int
	}{
		{120, 40, 2, 37, 1, 38},
		{80, 24, 2, 21, 1, 22},
	}
	for _, c := range full {
		m, b, clock := stripFrame(t, c.w, c.h, 0)
		if m.compact {
			t.Fatalf("%dx%d: the full layout must be drawn", c.w, c.h)
		}
		if m.contentRect.Y != c.shownY || m.contentRect.H != c.shownH {
			t.Fatalf("%dx%d shown: content rect = %+v, want y=%d h=%d", c.w, c.h, m.contentRect, c.shownY, c.shownH)
		}
		pressRune(m, 'p')
		expireNotice(clock)
		renderFresh(m, b)
		if m.contentRect.Y != c.hiddenY || m.contentRect.H != c.hiddenH {
			t.Fatalf("%dx%d hidden: content rect = %+v, want y=%d h=%d", c.w, c.h, m.contentRect, c.hiddenY, c.hiddenH)
		}
	}

	// Below the full layout's minimum there is no strip: the compact panel
	// keeps every row for its graph, whatever p says.
	for _, c := range []struct{ w, h int }{{60, 18}, {44, 14}, {72, 19}} {
		m, b, _ := stripFrame(t, c.w, c.h, 0)
		if !m.compact {
			t.Fatalf("%dx%d: the compact view was expected", c.w, c.h)
		}
		if !m.contentRect.Empty() {
			t.Fatalf("%dx%d: the compact view set a content rect: %+v", c.w, c.h, m.contentRect)
		}
		if regionByName(m.regions, "page:overview") != nil || regionByName(m.regions, "footer:tabs") != nil {
			t.Fatalf("%dx%d: the compact view recorded page or tab regions", c.w, c.h)
		}
		if strings.Contains(b.String(), "[1 Overview]") {
			t.Fatalf("%dx%d: the compact view drew a strip:\n%s", c.w, c.h, b.String())
		}
	}
}

// stripShown is the rule itself: a second page, the operator's choice, and a
// terminal tall enough for the full layout.
func TestTopStripShownRule(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, directionFixture(t), nil)
	cases := []struct {
		name   string
		rect   tui.Rect
		pages  int
		hidden bool
		want   bool
	}{
		{"one page", tui.Rect{W: 120, H: 40}, 1, false, false},
		{"two pages", tui.Rect{W: 120, H: 40}, 2, false, true},
		{"operator hid it", tui.Rect{W: 120, H: 40}, 2, true, false},
		{"one row under the minimum", tui.Rect{W: 120, H: topStripMinHeight - 1}, 2, false, false},
		{"at the minimum", tui.Rect{W: 120, H: topStripMinHeight}, 2, false, true},
	}
	for _, c := range cases {
		m.stripHidden = c.hidden
		if got := m.stripShown(c.rect, c.pages); got != c.want {
			t.Errorf("%s: stripShown = %v, want %v", c.name, got, c.want)
		}
	}
	if topStripMinHeight != 20 {
		t.Fatalf("the strip's minimum height = %d, want the full layout's 20 rows", topStripMinHeight)
	}
}

// A provider that leaves one page has nothing to list, so no strip and no
// page hints, and p changes nothing.
func TestTopStripNeedsASecondPage(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	b := tui.New(120, 40)
	renderFresh(m, b)
	if m.contentRect.Y != 1 || m.contentRect.H != 38 {
		t.Fatalf("content rect = %+v, want the full band at y=1 h=38", m.contentRect)
	}
	if regionByName(m.regions, "page:overview") != nil || regionByName(m.regions, "footer:pages") != nil {
		t.Fatal("a single page recorded page regions")
	}
	if strings.Contains(b.String(), "p tabs") {
		t.Fatalf("a single page offered the tab hint:\n%s", b.String())
	}
	pressRune(m, 'p')
	if m.stripHidden {
		t.Fatal("p hid a strip that was never drawn")
	}
}

// p is ignored while a popup, the overview, the help, the menu or the compact
// view owns the keys, like the page switch keys.
func TestTopStripKeyIgnoredInPopups(t *testing.T) {
	for name, open := range map[string]func(*topModel){
		"help":     func(m *topModel) { m.help = true },
		"menu":     func(m *topModel) { m.menu = true },
		"panel":    func(m *topModel) { m.panel = "alerts" },
		"overview": func(m *topModel) { m.overview = true },
		"popup":    func(m *topModel) { m.proxyPopup = true },
		"compact":  func(m *topModel) { m.compact = true },
	} {
		clock := &fakeClock{t: topBase}
		m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
		m.apply(0, directionFixture(t), nil)
		open(m)
		pressRune(m, 'p')
		if m.stripHidden {
			t.Errorf("%s open: p hid the strip", name)
		}
	}
}

// A click on a tab while the strip is shown switches page, and the strip stays.
func TestTopStripClickWhileShown(t *testing.T) {
	m, b, _ := stripFrame(t, 120, 40, 0)
	remote := regionRect(t, m, "page:remote")
	m.handle(mouseEv(tcellui.MousePress, tcellui.MouseLeft, remote.X+2, remote.Y))
	m.handle(mouseEv(tcellui.MouseRelease, tcellui.MouseLeft, remote.X+2, remote.Y))
	if m.page != 1 {
		t.Fatalf("the click left page %d, want 2", m.page+1)
	}
	renderFresh(m, b)
	if m.contentRect.Y != 2 || m.contentRect.H != 37 {
		t.Fatalf("after the click content rect = %+v, want the strip still shown", m.contentRect)
	}
}

// The strip's row comes out of the graph band: the boxes anchored to the
// bottom of the left column do not move, and the side column follows the
// content down one row.
func TestTopStripTakesItsRowFromTheGraphBand(t *testing.T) {
	m, b, clock := stripFrame(t, 120, 40, 0)
	names := []string{"box:graph", "box:proxies", "box:events", "box:now", "box:resources"}
	shown := map[string]tui.Rect{}
	for _, name := range names {
		shown[name] = regionRect(t, m, name)
	}
	pressRune(m, 'p')
	expireNotice(clock)
	renderFresh(m, b)
	for _, name := range []string{"box:proxies", "box:events"} {
		if got := regionRect(t, m, name); got != shown[name] {
			t.Fatalf("%s moved from %+v to %+v when the strip was hidden", name, shown[name], got)
		}
	}
	if got := regionRect(t, m, "box:graph"); got.H != shown["box:graph"].H+1 {
		t.Fatalf("the graph band went from %d rows to %d, want one more", shown["box:graph"].H, got.H)
	}
	if got := regionRect(t, m, "box:now"); got.Y != shown["box:now"].Y-1 || got.H != shown["box:now"].H {
		t.Fatalf("the side column moved from %+v to %+v, want one row up at the same height", shown["box:now"], got)
	}
}

// The Remote page draws the same chart and figures whether the strip is shown
// or hidden: it paints inside the rectangle the frame hands it and nothing
// else moves in.
func TestTopRemotePageInsideItsRectangle(t *testing.T) {
	shown, b, clock := stripFrame(t, 120, 40, 1)
	for _, state := range []string{"shown", "hidden"} {
		if state == "hidden" {
			pressRune(shown, 'p')
			expireNotice(clock)
			renderFresh(shown, b)
		}
		if box := regionRect(t, shown, "box:remote"); box != shown.contentRect {
			t.Fatalf("%s: the page box %+v is not the content rect %+v", state, box, shown.contentRect)
		}
		text := b.String()
		for _, want := range []string{"Remote", "billable", "total", "▲", "▼"} {
			if !strings.Contains(text, want) {
				t.Fatalf("page 2 %s is missing %q:\n%s", state, want, text)
			}
		}
		for _, unwanted := range []string{"Proxies", "Events", "Resources"} {
			if strings.Contains(text, unwanted) {
				t.Fatalf("page 2 %s drew the dashboard's %s panel:\n%s", state, unwanted, text)
			}
		}
	}
	if shown.contentRect.H != 38 {
		t.Fatalf("hidden: content rect = %+v, want the full band of 38 rows", shown.contentRect)
	}
}

// The direction pages keep their own rectangle and their figures in both
// states, and neither draws the dashboard's panels.
func TestTopDirectionPagesInsideTheirRectangles(t *testing.T) {
	for _, c := range []struct {
		id       string
		billable bool
	}{{"billable", true}, {"total", false}} {
		m, b, clock := stripFrame(t, 120, 40, pageByID(c.id))
		up, down, _, _, _, _, ok := m.directionData(c.billable)
		if !ok {
			t.Fatalf("%s: direction data missing", c.id)
		}
		want := []string{
			"upload", "download", "▲", "▼",
			tui.Rate(remoteAverage(up)) + "  ▲",
			tui.Rate(remoteAverage(down)) + "  ▼",
		}
		for _, state := range []string{"shown", "hidden"} {
			if state == "hidden" {
				pressRune(m, 'p')
				expireNotice(clock)
				renderFresh(m, b)
			}
			if box := regionRect(t, m, "box:"+c.id); box != m.contentRect {
				t.Fatalf("%s %s: the page box %+v is not the content rect %+v", c.id, state, box, m.contentRect)
			}
			text := b.String()
			for _, w := range want {
				if !strings.Contains(text, w) {
					t.Fatalf("%s %s: missing %q:\n%s", c.id, state, w, text)
				}
			}
			for _, unwanted := range []string{"Proxies", "Events", "Resources"} {
				if strings.Contains(text, unwanted) {
					t.Fatalf("%s %s: drew the dashboard's %s panel:\n%s", c.id, state, unwanted, text)
				}
			}
		}
	}
}
