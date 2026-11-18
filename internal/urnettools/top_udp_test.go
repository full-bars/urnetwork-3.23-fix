package urnettools

import (
	"strings"
	"testing"

	"github.com/urnetwork/connect/internal/tui"
)

func TestTopUDPCoverage(t *testing.T) {
	cases := []struct {
		name  string
		w, h  int
		theme tui.Theme
	}{
		{"top_udp_120x40", 120, 40, tui.DefaultTheme()},
		{"top_udp_100x30", 100, 30, tui.DefaultTheme()},
		{"top_udp_80x24", 80, 24, tui.DefaultTheme()},
		{"top_udp_mono_80x24", 80, 24, tui.MonoTheme()},
		{"top_udp_colorblind_120x40", 120, 40, mustTheme(t, "colorblind")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := newTestModel(c.theme, topProviders(1), &fakeClock{t: topBase})
			s := topFlowing(t)
			s.Proxies.UDP = &SnapshotProxyUDP{OK: 31, NoRelay: 3, Mismatch: 1, Unknown: 2, Unchecked: 17}
			m.apply(0, s, nil)
			got, _ := showOnSim(t, m, c.w, c.h)
			for _, want := range []string{"udp ", "31 ok", "unchecked", "83% of checked, 57% of online"} {
				if !strings.Contains(got, want) {
					t.Fatalf("missing %q:\n%s", want, got)
				}
			}
			assertGolden(t, c.name, got)
		})
	}
}

func TestTopUDPGridAndCompactLayout(t *testing.T) {
	for _, n := range []int{1, 900} {
		m := topHeatGridModel(t, tui.DefaultTheme(), n)
		m.snap.Proxies.UDP = &SnapshotProxyUDP{OK: n}
		got, _ := showOnSim(t, m, 120, 40)
		if !strings.Contains(got, "udp ") {
			t.Fatalf("pool %d: missing UDP row:\n%s", n, got)
		}
		box := regionRect(t, m, "box:proxies")
		if n == 1 && box.H != topProxiesCompactRows+1 {
			t.Fatalf("compact pool expanded to %d rows", box.H)
		}
		if n == 900 && box.H != topProxiesGridRows {
			t.Fatalf("grid pool height = %d, want %d", box.H, topProxiesGridRows)
		}
	}
	m := topHeatGridModel(t, tui.DefaultTheme(), 900)
	m.snap.Proxies.UDP = &SnapshotProxyUDP{OK: 900}
	buf := tui.New(60, proxyGridMinH)
	m.drawProxies(buf)
	if m.proxyGridH != 0 {
		t.Fatalf("sub-minimum grid height %d drawn with UDP rows", m.proxyGridH+1)
	}
}

func TestTopUDPCompactAndEmpty(t *testing.T) {
	for _, tc := range []struct {
		name string
		udp  *SnapshotProxyUDP
		want string
	}{
		{"old", nil, ""},
		{"unchecked", &SnapshotProxyUDP{Unchecked: 1}, "1 unchecked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
			s := topFlowing(t)
			s.Proxies = SnapshotProxies{Up: 1, UDP: tc.udp}
			m.apply(0, s, nil)
			got, _ := showOnSim(t, m, 120, 40)
			if tc.udp == nil && strings.Contains(got, "udp ") {
				t.Fatalf("unexpected UDP row:\n%s", got)
			}
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("missing %q:\n%s", tc.want, got)
			}
		})
	}
}

// A narrow panel keeps the ok and unchecked counts with their labels. Larger
// optional states give way first.
func TestTopUDPCoverageKeepsUncheckedInNarrowPanels(t *testing.T) {
	m := newTestModel(tui.DefaultTheme(), topProviders(1), &fakeClock{t: topBase})
	s := topFlowing(t)
	s.Proxies.UDP = &SnapshotProxyUDP{OK: 4000, NoRelay: 1000, Mismatch: 1000, Unknown: 1000, Unchecked: 1000}
	m.apply(0, s, nil)
	for _, w := range []int{80, 60, 48, 40, 30} {
		buf := tui.New(w, 4)
		m.drawUDPProxyCoverage(buf, 0)
		line := strings.SplitN(buf.String(), "\n", 2)[0]
		for _, want := range []string{"4000 ok", "1000 unchecked"} {
			if !strings.Contains(line, want) {
				t.Fatalf("width %d: %q missing from %q", w, want, line)
			}
		}
	}
	// With room for everything, all five states are drawn.
	buf := tui.New(120, 4)
	m.drawUDPProxyCoverage(buf, 0)
	line := strings.SplitN(buf.String(), "\n", 2)[0]
	for _, want := range []string{"4000 ok", "1000 no relay", "1000 mismatch", "1000 unknown", "1000 unchecked"} {
		if !strings.Contains(line, want) {
			t.Fatalf("wide panel: %q missing from %q", want, line)
		}
	}
}
