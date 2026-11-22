package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

// pageModel is a model with a provider that reports history, rendered once at
// the given size.
func pageModel(t *testing.T, w, h int) (*topModel, *tui.Buffer, string) {
	t.Helper()
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	b := tui.New(w, h)
	renderFresh(m, b)
	return m, b, b.String()
}

// renderFresh clears the buffer first, the way the run loop does before every
// frame, so a second render is not read through the first one.
func renderFresh(m *topModel, b *tui.Buffer) {
	b.Fill(b.Rect(), ' ', tui.Style{})
	m.render(b)
}

// The dashboard keeps the frame every earlier version of top drew: no strip
// row, so the content starts directly under the title.
func TestTopDashboardFrameWithoutAPageStrip(t *testing.T) {
	m, _, text := pageModel(t, 120, 40)
	if m.contentRect.Y != 1 || m.contentRect.H != 38 {
		t.Fatalf("content rect = %+v, want the full band at y=1 h=38", m.contentRect)
	}
	for _, label := range []string{"Overview", "Remote", "[/] page"} {
		if strings.Contains(text, label) {
			t.Fatalf("the dashboard frame grew %q; it must stay as it was:\n%s", label, text)
		}
	}
	for _, name := range []string{"footer:pages", "page:overview", "page:remote"} {
		if regionByName(m.regions, name) != nil {
			t.Fatalf("region %q recorded on the dashboard", name)
		}
	}
}

// A strip too narrow for every tab keeps the current page and drops the rest.
func TestTopPageStripFitsWhatItCanAndKeepsTheCurrent(t *testing.T) {
	labels := []string{"[1 Overview]", "[2 Remote]"}
	cases := []struct {
		current, width int
		want           []int
	}{
		{1, 100, []int{0, 1}},
		{0, 100, []int{0, 1}},
		{1, 12, []int{1}},
		{0, 12, []int{0}},
		{0, 23, []int{0, 1}},
	}
	for _, c := range cases {
		got := topStripFit(labels, c.current, c.width)
		if len(got) != len(c.want) {
			t.Fatalf("current %d width %d: got %v want %v", c.current, c.width, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("current %d width %d: got %v want %v", c.current, c.width, got, c.want)
			}
		}
	}
	// A three page strip widens out from the current page, not from the first,
	// so the tabs next to the page on screen win the room.
	wide := []string{"[1 Overview]", "[2 Remote]", "[3 Proxies]"}
	got := topStripFit(wide, 2, 24)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("narrow three-page strip = %v, want the current tab and its neighbour", got)
	}
}
