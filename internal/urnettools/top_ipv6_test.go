package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

func TestTopIPv6Coverage(t *testing.T) {
	themes := []struct {
		name  string
		theme tui.Theme
	}{
		{"default", tui.DefaultTheme()},
		{"mono", tui.MonoTheme()},
		{"colorblind", mustTheme(t, "colorblind")},
	}
	for _, th := range themes {
		t.Run(th.name, func(t *testing.T) {
			m := newTestModel(th.theme, topProviders(1), &fakeClock{t: topBase})
			s := topFlowing(t)
			s.Proxies.IPv6 = &SnapshotProxyIPv6{
				Yes:        12,
				No:         988,
				Unknown:    4,
				NotProbed:  0,
				UDPYes:     10,
				UDPNo:      2,
				UDPUnknown: 0,
				Probes:     1004,
			}
			m.apply(0, s, nil)
			got, _ := showOnSim(t, m, 120, 40)
			for _, want := range []string{"IPv6 proxies: ", "12 yes", "988 no", "4 unknown", "(udp: 10 yes, 2 no)"} {
				if !strings.Contains(got, want) {
					t.Fatalf("theme %s: missing %q:\n%s", th.name, want, got)
				}
			}
		})
	}
}

func TestTopIPv6CoverageNarrow(t *testing.T) {
	m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	s := topFlowing(t)
	s.Proxies.IPv6 = &SnapshotProxyIPv6{
		Yes:        12,
		No:         988,
		Unknown:    4,
		NotProbed:  10,
		UDPYes:     10,
		UDPNo:      2,
		UDPUnknown: 0,
		Probes:     1004,
	}
	m.apply(0, s, nil)

	// Wide (>= 60 cols)
	bufWide := tui.New(100, 4)
	m.drawIPv6ProxyCoverage(bufWide, 0)
	wideLine := strings.SplitN(bufWide.String(), "\n", 2)[0]
	if !strings.Contains(wideLine, "IPv6 proxies: ") || !strings.Contains(wideLine, "12 yes") {
		t.Fatalf("wide line missing expected content: %q", wideLine)
	}

	// Narrow (< 60 cols) changes prefix to "ipv6: " and compresses labels
	for _, w := range []int{55, 45, 35} {
		bufNarrow := tui.New(w, 4)
		m.drawIPv6ProxyCoverage(bufNarrow, 0)
		narrowLine := strings.SplitN(bufNarrow.String(), "\n", 2)[0]
		if !strings.Contains(narrowLine, "ipv6: ") {
			t.Fatalf("width %d: missing 'ipv6: ' prefix: %q", w, narrowLine)
		}
		if !strings.Contains(narrowLine, "12 yes") {
			t.Fatalf("width %d: essential '12 yes' dropped: %q", w, narrowLine)
		}
	}
}

func TestTopProxyPopupIPv6(t *testing.T) {
	m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	m.proxyCells = []proxyCell{{index: 0}, {index: 1}, {index: 2}, {index: 3}}
	m.proxyDetail = map[int]ProxyCellDetail{
		1: {
			IPv6:     "yes",
			IPv6Exit: "2001:db8::1",
			TCPExit:  "192.0.2.1",
			UDPExit:  "192.0.2.1",
		},
		2: {
			IPv6: "no",
		},
		3: {
			IPv6: "unknown",
		},
	}

	// Proxy 1 (yes + exit)
	info1 := m.proxyCellInfo(1)
	joined1 := strings.Join(info1, "\n")
	if !strings.Contains(joined1, "IPv6 exit: yes (2001:db8::1)") {
		t.Fatalf("proxy 1 popup missing ipv6 exit: %s", joined1)
	}

	// Proxy 2 (no)
	info2 := m.proxyCellInfo(2)
	joined2 := strings.Join(info2, "\n")
	if !strings.Contains(joined2, "IPv6 exit: no") {
		t.Fatalf("proxy 2 popup missing 'IPv6 exit: no': %s", joined2)
	}

	// Proxy 3 (unknown)
	info3 := m.proxyCellInfo(3)
	joined3 := strings.Join(info3, "\n")
	if !strings.Contains(joined3, "IPv6 exit: unknown") {
		t.Fatalf("proxy 3 popup missing 'IPv6 exit: unknown': %s", joined3)
	}

	// Direct slot (index 0) must not show proxy detail lines
	info0 := m.proxyCellInfo(0)
	joined0 := strings.Join(info0, "\n")
	if strings.Contains(joined0, "IPv6 exit:") {
		t.Fatalf("direct slot unexpectedly showed IPv6 exit: %s", joined0)
	}
}

// The style legend of a non-Glow heatmap goes below the coverage rows, so it
// never overwrites the IPv6 row in a short box (five rows is the smallest box
// that shows the row after the four bars).
func TestTopIPv6CoverageIsNotOverwrittenByTheStyleLegend(t *testing.T) {
	for height := 5; height <= 8; height++ {
		m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
		s := topFlowing(t)
		s.Proxies.UDP = nil
		s.Proxies.IPv6 = &SnapshotProxyIPv6{Yes: 12, No: 988, Unknown: 4}
		m.apply(0, s, nil)
		m.proxyHeatmapStyle = heatmapAurora
		buf := tui.New(100, height)
		m.drawProxies(buf)
		out := buf.String()
		if !strings.Contains(out, "IPv6 proxies: ") || !strings.Contains(out, "12 yes") {
			t.Fatalf("height %d: the IPv6 row was overwritten:\n%s", height, out)
		}
	}
}

// The compact panel of a small pool has three content rows. The IPv6 row must
// keep its place when the optional UDP percentage line would take it.
func TestTopCompactPanelKeepsTheIPv6Row(t *testing.T) {
	m := modelWithProxies(t, 1, 0, 0, 0)
	m.snap.Proxies.UDP = &SnapshotProxyUDP{OK: 1}
	m.snap.Proxies.IPv6 = &SnapshotProxyIPv6{Yes: 1}
	buf := tui.New(100, m.proxiesRows()-2)
	m.drawProxiesCompact(buf)
	out := buf.String()
	for _, want := range []string{"1 up", "udp 1 ok", "IPv6 proxies: ", "1 yes"} {
		if !strings.Contains(out, want) {
			t.Fatalf("compact panel missing %q:\n%s", want, out)
		}
	}
}

// In the bar view the four bars leave two rows at height six. The IPv6 row must
// keep its place when the optional UDP percentage line would take it.
func TestTopBarViewKeepsTheIPv6Row(t *testing.T) {
	for height := 6; height <= 8; height++ {
		m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
		s := topFlowing(t)
		s.Proxies.UDP = &SnapshotProxyUDP{OK: 1, Unchecked: 1}
		s.Proxies.IPv6 = &SnapshotProxyIPv6{Yes: 12, No: 988, Unknown: 4}
		m.apply(0, s, nil)
		buf := tui.New(100, height)
		m.drawProxies(buf)
		out := buf.String()
		if !strings.Contains(out, "udp ") || !strings.Contains(out, "IPv6 proxies: ") || !strings.Contains(out, "12 yes") {
			t.Fatalf("height %d: the bar view lost the UDP or the IPv6 row:\n%s", height, out)
		}
	}
}

// A popup with five content rows cannot hold the title, the four field lines
// and the exit details. The exit details are the lines that were clipped, so
// they keep their rows and the fields give way. A tall popup keeps every line.
func TestFitPopupLinesKeepsTheTitleAndTheExitDetails(t *testing.T) {
	lines := []string{"title", "state", "latency", "tcp", "combined", "IPv6 exit: yes (2001:db8::1)", "TCP exit 192.0.2.1"}
	got := fitPopupLines(lines, 5, 2)
	want := []string{"title", "state", "latency", "IPv6 exit: yes (2001:db8::1)", "TCP exit 192.0.2.1"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("fitted lines = %v, want %v", got, want)
	}
	if got := fitPopupLines(lines, 9, 2); strings.Join(got, "|") != strings.Join(lines, "|") {
		t.Fatalf("a tall popup lost lines: %v", got)
	}
	if got := fitPopupLines(lines, 2, 2); len(got) != 3 || got[0] != "title" {
		t.Fatalf("a popup with no room for the exit details: %v", got)
	}
}

// A single proxy's popup in the shortest box (five content rows) still shows
// its IPv6 exit.
func TestTopProxyPopupShowsTheExitInAShortBox(t *testing.T) {
	m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	m.proxyCells = []proxyCell{{index: 0}, {index: 1}}
	m.proxyDetail = map[int]ProxyCellDetail{
		1: {IPv6: "yes", IPv6Exit: "2001:db8::1", TCPExit: "192.0.2.1", UDPExit: "192.0.2.1"},
	}
	m.proxyPopup = true
	m.proxyPopupIndices = []int{1}
	m.proxyPopupRect = tui.Rect{X: 0, Y: 0, W: 70, H: 7}
	buf := tui.New(70, 7)
	m.drawProxyPopup(buf)
	if out := buf.String(); !strings.Contains(out, "IPv6 exit: yes") {
		t.Fatalf("the short popup clipped the exit details:\n%s", out)
	}
}
