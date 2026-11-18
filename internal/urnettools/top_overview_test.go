package urnettools

import (
	"fmt"
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

func overviewFixtures(t *testing.T) ([]Provider, []*NodeSnapshot, []ExtenderReading) {
	t.Helper()
	providers := topProviders(3)
	s0 := topFlowing(t)
	s1 := topFlowing(t)
	s1.State = "idle"
	s1.Proxies = SnapshotProxies{Up: 20, Degraded: 4, Connecting: 2, Dead: 4}
	s0.Proxies = SnapshotProxies{Up: 58, Degraded: 1, Connecting: 1, Dead: 0}
	s0.Clients = 212
	s1.Clients = 7
	snaps := []*NodeSnapshot{s0, s1, nil}
	exts := []ExtenderReading{{Source: SourceControlSocket, Running: true}, {Source: SourceControlSocket, Running: false}}
	return providers, snaps, exts
}

func TestBuildOverviewRows(t *testing.T) {
	providers, snaps, exts := overviewFixtures(t)
	rows := buildOverviewRows(providers, snaps, exts, tui.DefaultTheme())
	if len(rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(rows))
	}

	r0 := rows[0]
	if r0.name != "tornado" || r0.index != 0 {
		t.Errorf("row 0 = %+v", r0)
	}
	if r0.version == "" || r0.state != "FLOWING" || r0.level != topGood {
		t.Errorf("row 0 version/state/level = %+v", r0)
	}
	if r0.billable == "" || r0.clients != 212 {
		t.Errorf("row 0 billable/clients = %+v", r0)
	}
	if r0.proxiesUp != 58 || r0.proxiesTotal != 60 {
		t.Errorf("row 0 proxies = %d/%d", r0.proxiesUp, r0.proxiesTotal)
	}
	if r0.extender != "serving" {
		t.Errorf("row 0 extender = %q", r0.extender)
	}
	if r0.spark == "" {
		t.Error("row 0 spark is empty")
	}

	r1 := rows[1]
	if r1.state != "IDLE" || r1.level != topWarn {
		t.Errorf("row 1 state/level = %q/%v", r1.state, r1.level)
	}
	if r1.clients != 7 || r1.proxiesUp != 20 || r1.proxiesTotal != 30 {
		t.Errorf("row 1 clients/proxies = %+v", r1)
	}
	if r1.extender != "off" {
		t.Errorf("row 1 extender = %q", r1.extender)
	}

	r2 := rows[2]
	if r2.state != "no data" || r2.version != "" || r2.clients != 0 || r2.proxiesUp != 0 || r2.extender != "—" || r2.spark != "" {
		t.Errorf("row 2 = %+v", r2)
	}
}

func TestOverviewSparkIsBounded(t *testing.T) {
	providers, snaps, exts := overviewFixtures(t)
	rows := buildOverviewRows(providers, snaps, exts, tui.DefaultTheme())
	for _, r := range rows[:2] {
		if w := tui.StringWidth(r.spark); w > 24 {
			t.Fatalf("spark %q is %d cells, want at most 24", r.spark, w)
		}
	}
}

func TestDrawOverviewReturnsTheSelectedProvider(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(3), clock)
	m.providerSnaps[0] = topFlowing(t)
	m.overviewSel = 2
	b := tui.New(120, 40)
	m.overview = true
	m.render(b)
	got := b.String()
	for _, want := range []string{"Providers", "tornado", "alpha", "bravo", "FLOWING", "212", "58/60"} {
		if !strings.Contains(got, want) {
			t.Errorf("overview is missing %q:\n%s", want, got)
		}
	}
	if got := m.overviewSel; got != 2 {
		t.Fatalf("overviewSel = %d", got)
	}
	// the screen returns the provider index of the selected row
	if idx := m.drawOverview(tui.New(120, 40)); idx != 2 {
		t.Fatalf("drawOverview returned %d, want 2", idx)
	}
}

func TestDrawOverviewRendersAt80x24(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(3), clock)
	m.providerSnaps[0] = topFlowing(t)
	m.overview = true
	b := tui.New(80, 24)
	m.render(b)
	for i, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		if w := tui.StringWidth(line); w > 80 {
			t.Fatalf("row %d is %d cells in an 80 wide screen", i, w)
		}
	}
}

func TestOverviewRowAtHitTest(t *testing.T) {
	providers, snaps, exts := overviewFixtures(t)
	rows := buildOverviewRows(providers, snaps, exts, tui.DefaultTheme())
	if got := overviewRowAt(rows, 0); got != -1 {
		t.Fatalf("header row -> %d", got)
	}
	if got := overviewRowAt(rows, 1); got != 0 {
		t.Fatalf("first row -> %d", got)
	}
	if got := overviewRowAt(rows, 3); got != 2 {
		t.Fatalf("third row -> %d", got)
	}
	if got := overviewRowAt(rows, 4); got != -1 {
		t.Fatalf("past the last row -> %d", got)
	}
}

func TestSelectProviderIndex(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(3), clock)
	m.apply(0, topFlowing(t), nil)
	if eff := m.selectProviderIndex(2); eff != topRefetch {
		t.Fatalf("jump -> %v", eff)
	}
	if m.cur != 2 {
		t.Fatalf("cur = %d, want 2", m.cur)
	}
	if eff := m.selectProviderIndex(2); eff != topNone {
		t.Fatal("jumping to the current provider must do nothing")
	}
	if eff := m.selectProviderIndex(9); eff != topNone {
		t.Fatal("an out of range index must do nothing")
	}
	// Tab still wraps from the new selection
	if eff := m.selectProvider(1); eff != topRefetch || m.cur != 0 {
		t.Fatalf("tab after jump -> eff=%v cur=%d", eff, m.cur)
	}
}

func TestWantOverviewGates(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(2), clock)
	if _, ok := m.wantOverview(clock.Now()); ok {
		t.Fatal("the overview must not fetch while it is closed")
	}
	m.overview = true
	if _, ok := m.wantOverview(clock.Now()); !ok {
		t.Fatal("the overview should fetch once it opens")
	}
	if _, ok := m.wantOverview(clock.Now()); ok {
		t.Fatal("the overview must not fetch faster than the cadence")
	}
	clock.Advance(topSnapshotEvery)
	if _, ok := m.wantOverview(clock.Now()); !ok {
		t.Fatal("the overview should fetch again after the cadence")
	}
}

func TestApplyOverviewDropsStaleGens(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(2), clock)
	m.gen = 5
	m.applyOverview(4, 1, topFlowing(t), nil, ExtenderReading{})
	if m.providerSnaps[1] != nil {
		t.Fatal("a result for an earlier selection must be dropped")
	}
	m.applyOverview(5, 1, topFlowing(t), nil, ExtenderReading{})
	if m.providerSnaps[1] == nil {
		t.Fatal("a result for the current selection must land")
	}
	m.applyOverview(5, 9, topFlowing(t), nil, ExtenderReading{})
	if len(m.providerSnaps) != 2 {
		t.Fatal("an out of range index must be dropped")
	}
	// extender reading lands too, and only a control-socket one
	ext := ExtenderReading{Source: SourceControlSocket, Running: true}
	m.applyOverview(5, 1, nil, nil, ext)
	if m.providerExt[1].Source != SourceControlSocket {
		t.Fatal("the extender reading did not land")
	}
	m.applyOverview(5, 1, nil, nil, ExtenderReading{})
	if m.providerExt[1].Source != SourceControlSocket {
		t.Fatal("an empty extender reading must not overwrite the last one")
	}
}

func TestOverviewShowsEveryRibbonOfTheHost(t *testing.T) {
	// [3/3] in the header is the case that needs the screen: several providers
	// on one host. Each gets a row, in the order of the list.
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(3), clock)
	m.providerSnaps[0] = topFlowing(t)
	m.providerSnaps[1] = topFlowing(t)
	m.providerSnaps[1].State = "degraded"
	m.providerSnaps[2] = topFlowing(t)
	m.providerSnaps[2].State = "idle"
	b := tui.New(120, 40)
	m.overview = true
	m.render(b)
	got := b.String()
	for _, want := range []string{"FLOWING", "DEGRADED", "IDLE", "tornado", "alpha", "bravo"} {
		if !strings.Contains(got, want) {
			t.Errorf("overview is missing %q:\n%s", want, got)
		}
	}
}

func TestTopOverviewAndWindowKeys(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(2), clock)
	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 't'})
	if m.windowHours != 1 {
		t.Fatalf("window = %d", m.windowHours)
	}
	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 't'})
	if m.windowHours != 24 {
		t.Fatalf("window = %d", m.windowHours)
	}
	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 't'})
	if m.windowHours != 0 {
		t.Fatalf("window = %d", m.windowHours)
	}
	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 'o'})
	if !m.overview {
		t.Fatal("overview did not open")
	}
	m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyDown})
	m.handle(tcellui.Event{Kind: tcellui.EventKey, Key: tcellui.KeyEnter})
	if m.overview || m.cur != 1 {
		t.Fatalf("overview=%v provider=%d", m.overview, m.cur)
	}
}

func TestTopOverviewScrollAndFailedPoll(t *testing.T) {
	clock := &fakeClock{t: topBase}
	providers := make([]Provider, 30)
	for i := range providers {
		providers[i] = Provider{Unit: fmt.Sprintf("provider-%d", i)}
	}
	m := newTestModel(tui.DefaultTheme(), providers, clock)
	m.overviewSel = 25
	b := tui.New(100, 10)
	m.drawOverview(b)
	if !strings.Contains(b.String(), "26 ") {
		t.Fatal("selected provider not visible")
	}
	m.applyOverview(m.gen, 1, topFlowing(t), nil, ExtenderReading{})
	m.applyOverview(m.gen, 1, nil, fmt.Errorf("offline"), ExtenderReading{})
	if m.providerSnaps[1] != nil {
		t.Fatal("failed poll retained stale snapshot")
	}
}
