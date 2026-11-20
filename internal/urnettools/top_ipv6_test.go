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
