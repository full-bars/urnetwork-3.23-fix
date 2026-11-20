package urnettools

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/internal/tui"
)

func TestDirectStarPopupDualStackLines(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.proxyCells = makeProxyCells([]uint8{connect.ProxyCellUp})
	m.proxyDetail = map[int]ProxyCellDetail{
		0: {
			Index:      0,
			State:      connect.ProxyCellUp,
			Clients:    3,
			AgeSeconds: 120,
			IPv4: &ProxyCellFamilyStats{
				ConnectsOK:      15,
				ConnectsFailed:  1,
				RxBytes:         51200,
				TxBytes:         20480,
				LastSuccessUnix: topBase.Add(-10 * time.Second).Unix(),
			},
			IPv6: &ProxyCellFamilyStats{
				ConnectsOK:      8,
				ConnectsFailed:  0,
				RxBytes:         16384,
				TxBytes:         8192,
				LastSuccessUnix: topBase.Add(-2 * time.Minute).Unix(),
			},
		},
	}

	lines := m.proxyCellInfo(0)
	joined := strings.Join(lines, "\n")

	// Must contain both ipv4 and ipv6 lines
	if !strings.Contains(joined, "ipv4 ok 15 fail 1") {
		t.Fatalf("proxyCellInfo(0) missing ipv4 stats:\n%s", joined)
	}
	if !strings.Contains(joined, "ipv6 ok 8 fail 0") {
		t.Fatalf("proxyCellInfo(0) missing ipv6 stats:\n%s", joined)
	}
	if !strings.Contains(joined, "last 10s ago") {
		t.Fatalf("proxyCellInfo(0) missing ipv4 last timing:\n%s", joined)
	}
	if !strings.Contains(joined, "last 2m ago") {
		t.Fatalf("proxyCellInfo(0) missing ipv6 last timing:\n%s", joined)
	}

	// Now draw popup
	m.proxyPopup = true
	m.proxyPopupX, m.proxyPopupY = 5, 5
	m.proxyPopupIndices = []int{0}
	m.proxyPopupRect = tui.Rect{X: 0, Y: 0, W: 70, H: 12}

	buf := tui.New(70, 12)
	m.drawProxyPopup(buf)
	popupStr := buf.String()

	if !strings.Contains(popupStr, "direct (this host)") {
		t.Fatalf("popup missing direct host:\n%s", popupStr)
	}
	if !strings.Contains(popupStr, "ipv4 ok 15 fail 1") {
		t.Fatalf("popup missing ipv4 line:\n%s", popupStr)
	}
	if !strings.Contains(popupStr, "ipv6 ok 8 fail 0") {
		t.Fatalf("popup missing ipv6 line:\n%s", popupStr)
	}
}

func TestDirectStarPopupOldReplyFallback(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.proxyCells = makeProxyCells([]uint8{connect.ProxyCellUp})
	// Old provider detail without IPv4/IPv6 fields
	m.proxyDetail = map[int]ProxyCellDetail{
		0: {
			Index:      0,
			State:      connect.ProxyCellUp,
			Clients:    1,
			AgeSeconds: 30,
			IPv4:       nil,
			IPv6:       nil,
		},
	}

	lines := m.proxyCellInfo(0)
	joined := strings.Join(lines, "\n")

	// Must cleanly fall back without panicking
	if !strings.Contains(joined, "ipv4 ok 0 fail 0  in 0 B out 0 B  last never") {
		t.Fatalf("fallback ipv4 line missing or wrong:\n%s", joined)
	}
	if !strings.Contains(joined, "ipv6 ok 0 fail 0  in 0 B out 0 B  last never") {
		t.Fatalf("fallback ipv6 line missing or wrong:\n%s", joined)
	}

	m.proxyPopup = true
	m.proxyPopupX, m.proxyPopupY = 5, 5
	m.proxyPopupIndices = []int{0}
	m.proxyPopupRect = tui.Rect{X: 0, Y: 0, W: 60, H: 10}

	buf := tui.New(60, 10)
	m.drawProxyPopup(buf)
	popupStr := buf.String()

	if !strings.Contains(popupStr, "ipv4 ok 0 fail 0") {
		t.Fatalf("popup missing fallback ipv4:\n%s", popupStr)
	}
	if !strings.Contains(popupStr, "ipv6 ok 0 fail 0") {
		t.Fatalf("popup missing fallback ipv6:\n%s", popupStr)
	}
}

func TestDirectStarPopupRedaction(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.proxyCells = makeProxyCells([]uint8{connect.ProxyCellUp})
	m.proxyDetail = map[int]ProxyCellDetail{
		0: {
			Index: 0,
			Addr:  "direct (this host)",
			IPv4:  &ProxyCellFamilyStats{ConnectsOK: 1},
			IPv6:  &ProxyCellFamilyStats{ConnectsOK: 1},
		},
	}

	lines := m.proxyCellInfo(0)
	joined := strings.Join(lines, "\n")

	// Redaction: must not contain raw ip addresses or credentials
	for _, forbidden := range []string{"192.0.2.", "2001:db8:", "password", "token="} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("popup detail contains forbidden content %q:\n%s", forbidden, joined)
		}
	}
}

func TestProxyCellDetailDeserializationCompatibility(t *testing.T) {
	// Old json payload without ipv4/ipv6
	oldPayload := `{"index":0,"addr":"direct (this host)","state":0,"clients":2}`
	var d1 ProxyCellDetail
	if err := json.Unmarshal([]byte(oldPayload), &d1); err != nil {
		t.Fatalf("unmarshal old payload failed: %v", err)
	}
	if d1.IPv4 != nil || d1.IPv6 != nil {
		t.Fatalf("old payload should have nil IPv4 and IPv6: %+v", d1)
	}

	// New payload with ipv4/ipv6
	newPayload := `{"index":0,"addr":"direct (this host)","state":0,"clients":2,"ipv4":{"connects_ok":5,"rx_bytes":100},"ipv6":{"connects_ok":2,"rx_bytes":50}}`
	var d2 ProxyCellDetail
	if err := json.Unmarshal([]byte(newPayload), &d2); err != nil {
		t.Fatalf("unmarshal new payload failed: %v", err)
	}
	if d2.IPv4 == nil || d2.IPv4.ConnectsOK != 5 || d2.IPv4.RxBytes != 100 {
		t.Fatalf("new payload IPv4 deserialization failed: %+v", d2.IPv4)
	}
	if d2.IPv6 == nil || d2.IPv6.ConnectsOK != 2 || d2.IPv6.RxBytes != 50 {
		t.Fatalf("new payload IPv6 deserialization failed: %+v", d2.IPv6)
	}
}
