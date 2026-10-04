package connect

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTransportModeStatsIgnoreEmptyFrames(t *testing.T) {
	var stats transportModeStats
	stats.addTx(0)
	stats.addRx(0)
	stats.addTx(-1)
	if stats.framesTx.Load() != 0 || stats.framesRx.Load() != 0 || stats.bytesTx.Load() != 0 {
		t.Fatal("keepalive (empty) frames must not count as carried traffic")
	}
	stats.addTx(1200)
	stats.addTx(40)
	stats.addRx(100)
	if stats.framesTx.Load() != 2 || stats.bytesTx.Load() != 1240 {
		t.Fatalf("tx: frames=%d bytes=%d", stats.framesTx.Load(), stats.bytesTx.Load())
	}
	if stats.framesRx.Load() != 1 || stats.bytesRx.Load() != 100 {
		t.Fatalf("rx: frames=%d bytes=%d", stats.framesRx.Load(), stats.bytesRx.Load())
	}
}

func TestH3TxSharePercent(t *testing.T) {
	cases := []struct {
		name   string
		h1, h3 uint64
		want   int
	}{
		{"nothing sent", 0, 0, -1},
		{"h1 only", 10, 0, 0},
		{"h3 only", 0, 10, 100},
		{"even", 50, 50, 50},
		{"quarter", 75, 25, 25},
	}
	for _, c := range cases {
		snap := TransportModeStatsSnapshot{H1FramesTx: c.h1, H3FramesTx: c.h3}
		if got := snap.H3TxSharePercent(); got != c.want {
			t.Fatalf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func TestHealthSuffixHiddenUntilH3Attempted(t *testing.T) {
	if got := (TransportModeStatsSnapshot{H1FramesTx: 5}).HealthSuffix(); got != "" {
		t.Fatalf("a box that never tried H3 must see no new fields, got %q", got)
	}
	got := TransportModeStatsSnapshot{
		H1FramesTx: 50, H3FramesTx: 50, H3Attempts: 3, H3Up: 1, H3Drops: 2, H3ConnectFailures: 1,
	}.HealthSuffix()
	want := " h3_up=1 h3_tx_share=50% h3_drops=2 h3_conn_fail=1"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// attempted but nothing sent yet: the share is n/a, not 0%
	got = TransportModeStatsSnapshot{H3Attempts: 1}.HealthSuffix()
	if !strings.Contains(got, "h3_tx_share=n/a") {
		t.Fatalf("got %q", got)
	}
}

func TestPrometheusExposesTransportModeStats(t *testing.T) {
	before := TransportModeStats()
	h3ModeStats.addTx(500)
	h1ModeStats.addRx(700)
	defer func() {
		// leave the process counters as found
		h3ModeStats.framesTx.Add(^uint64(0))
		h3ModeStats.bytesTx.Add(^uint64(499))
		h1ModeStats.framesRx.Add(^uint64(0))
		h1ModeStats.bytesRx.Add(^uint64(699))
	}()

	w := httptest.NewRecorder()
	PrometheusHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	for _, family := range []string{
		"urnet_transport_frames_total",
		"urnet_transport_payload_bytes_total",
		"urnet_h3_up",
		"urnet_h3_connect_attempts_total",
		"urnet_h3_connects_total",
		"urnet_h3_connect_failures_total",
		"urnet_h3_drops_total",
	} {
		if !strings.Contains(body, "# TYPE "+family+" ") {
			t.Fatalf("missing family %s", family)
		}
	}
	if !strings.Contains(body, `urnet_transport_payload_bytes_total{mode="h3",dir="tx"} `) {
		t.Fatal("missing h3 tx sample")
	}
	after := TransportModeStats()
	if after.H3BytesTx != before.H3BytesTx+500 || after.H1BytesRx != before.H1BytesRx+700 {
		t.Fatalf("snapshot did not reflect the adds: before=%+v after=%+v", before, after)
	}
}

func TestH3DatagramHealthSuffixHiddenUntilOffered(t *testing.T) {
	if got := (H3DatagramSnapshot{Accepted: 0, Offered: 0, RxMessages: 9}).HealthSuffix(); got != "" {
		t.Fatalf("a box that never offered DATAGRAM must see no new fields, got %q", got)
	}
	got := H3DatagramSnapshot{Offered: 3, Accepted: 2, RxMessages: 40, RxDrops: 1}.HealthSuffix()
	want := " h3_dg=2/3 dg_rx=40 dg_rx_drop=1"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// offered but never accepted is the signal an old server is in the way
	if got := (H3DatagramSnapshot{Offered: 2}).HealthSuffix(); got != " h3_dg=0/2 dg_rx=0 dg_rx_drop=0" {
		t.Fatalf("got %q", got)
	}
}

func TestPrometheusExposesH3DatagramCounters(t *testing.T) {
	w := httptest.NewRecorder()
	PrometheusHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	for _, family := range []string{
		"urnet_h3_datagram_offered_total",
		"urnet_h3_datagram_accepted_total",
		"urnet_h3_datagram_rx_messages_total",
		"urnet_h3_datagram_rx_bytes_total",
		"urnet_h3_datagram_rx_dropped_total",
		"urnet_h3_datagram_rx_rejected_total",
	} {
		if !strings.Contains(body, "# TYPE "+family+" counter") {
			t.Fatalf("missing family %s", family)
		}
	}
	if !strings.Contains(body, `urnet_h3_datagram_rx_rejected_total{reason="malformed"} `) {
		t.Fatal("missing rejected sample")
	}
}

func TestH3CountersForSeparatesTheModes(t *testing.T) {
	if h3CountersFor(TransportModeH3) == h3CountersFor(TransportModeH3Dns) ||
		h3CountersFor(TransportModeH3Dns) == h3CountersFor(TransportModeH3DnsPump) ||
		h3CountersFor(TransportModeH3) == h3CountersFor(TransportModeH3DnsPump) {
		t.Fatal("H3, DNS and DNS pump must not share counters")
	}
	if h3CountersFor(TransportModeH3) != &h3Counters {
		t.Fatal("H3 must map to the H3 counters")
	}
	// anything else that reaches runH3 counts as H3, as before
	if h3CountersFor(TransportModeAuto) != &h3Counters {
		t.Fatal("an unrecognised mode must fall back to the H3 counters")
	}
}

func TestDnsModesAppearInTheSnapshotAndHealthSuffixOnlyOnceAttempted(t *testing.T) {
	if got := (TransportModeStatsSnapshot{}).HealthSuffix(); got != "" {
		t.Fatalf("nothing attempted: got %q", got)
	}
	got := TransportModeStatsSnapshot{
		Dns:     PtModeSnapshot{Attempts: 2, Up: 1, ConnectFailures: 1},
		DnsPump: PtModeSnapshot{Attempts: 1, ConnectFailures: 1},
	}.HealthSuffix()
	want := " dns_up=1 dns_conn_fail=1 dnspump_up=0 dnspump_conn_fail=1"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	// h3 untouched: no h3 fields from the DNS modes alone
	if strings.Contains(got, "h3_") {
		t.Fatalf("DNS modes must not produce h3 fields: %q", got)
	}
}

func TestPrometheusExposesPacketTranslationModes(t *testing.T) {
	w := httptest.NewRecorder()
	PrometheusHandler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	for _, family := range []string{
		"urnet_transport_pt_up",
		"urnet_transport_pt_connect_attempts_total",
		"urnet_transport_pt_connects_total",
		"urnet_transport_pt_connect_failures_total",
		"urnet_transport_pt_drops_total",
	} {
		if !strings.Contains(body, "# TYPE "+family+" ") {
			t.Fatalf("missing family %s", family)
		}
		for _, mode := range []string{"h3dns", "h3dnspump"} {
			if !strings.Contains(body, family+`{mode="`+mode+`"} `) {
				t.Fatalf("missing %s sample for %s", family, mode)
			}
		}
	}
	for _, mode := range []string{"h3dns", "h3dnspump"} {
		if !strings.Contains(body, `urnet_transport_payload_bytes_total{mode="`+mode+`",dir="tx"} `) {
			t.Fatalf("missing bytes sample for %s", mode)
		}
	}
}

// runH3 started for the DNS mode must count under the DNS counters, not H3, even
// though it is the same function. The dial fails (closed port), but the attempt
// is the point.
func TestRunH3CountsADnsModeAttemptUnderDns(t *testing.T) {
	before := TransportModeStats()
	transport, cancel := newClosedPortTransport(t, TransportModeH3Dns, false)
	done := make(chan struct{})
	go func() { defer close(done); transport.runH3(TransportModeH3Dns, 0, 1) }()
	time.Sleep(600 * time.Millisecond)
	cancel()
	<-done
	after := TransportModeStats()
	if after.Dns.Attempts <= before.Dns.Attempts {
		t.Fatal("a DNS-mode attempt was not counted under the DNS counters")
	}
	if after.H3Attempts != before.H3Attempts {
		t.Fatalf("a DNS-mode attempt leaked into the H3 counters: %d -> %d", before.H3Attempts, after.H3Attempts)
	}
}
