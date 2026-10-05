package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

func TestBaselineTransportOmitsH3UntilAttempted(t *testing.T) {
	modes := connect.TransportModeStatsSnapshot{H1FramesTx: 10, H1FramesRx: 20, H1BytesTx: 1000, H1BytesRx: 2000}
	got := buildBaselineTransport(modes, connect.H3DatagramSnapshot{})
	if got.H1.FramesTx != 10 || got.H1.FramesRx != 20 || got.H1.BytesTx != 1000 || got.H1.BytesRx != 2000 {
		t.Fatalf("h1 = %+v", got.H1)
	}
	if got.H3 != nil {
		t.Fatalf("a box that never attempted H3 must write no h3 block, got %+v", got.H3)
	}
	line, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(line), "h3") {
		t.Fatalf("no h3 field may appear before H3 is attempted: %s", line)
	}
}

// A direct-only box has H3 enabled on its native identity, so H1Direct mirrors H1
// exactly. Before H3 has been attempted the block says nothing new, and writing
// it duplicates every counter on every 15-minute sample.
func TestBaselineTransportOmitsH1DirectUntilH3Attempted(t *testing.T) {
	modes := connect.TransportModeStatsSnapshot{
		H1FramesTx: 10, H1FramesRx: 20, H1BytesTx: 1000, H1BytesRx: 2000,
		H1DirectFramesTx: 10, H1DirectFramesRx: 20, H1DirectBytesTx: 1000, H1DirectBytesRx: 2000,
	}
	got := buildBaselineTransport(modes, connect.H3DatagramSnapshot{})
	if got.H1Direct != nil {
		t.Fatalf("H3 never attempted: h1_direct must be omitted, got %+v", got.H1Direct)
	}
	line, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(line), "h1_direct") {
		t.Fatalf("no h1_direct field before H3 is attempted: %s", line)
	}

	// Still identical after H3 was attempted: the direct identity carries all of
	// H1, so there is no split to record.
	modes.H3Attempts = 3
	if got := buildBaselineTransport(modes, connect.H3DatagramSnapshot{}); got.H1Direct != nil {
		t.Fatalf("h1_direct equal to h1 must be withheld, got %+v", got.H1Direct)
	}

	// With a proxy pool the totals differ from the direct identity, and the split
	// is exactly what the panel needs.
	modes.H1FramesTx, modes.H1BytesTx = 30, 3000
	got = buildBaselineTransport(modes, connect.H3DatagramSnapshot{})
	if got.H1Direct == nil {
		t.Fatal("h1_direct must be written when the direct identity differs from h1")
	}
	if got.H1Direct.FramesTx != 10 || got.H1Direct.FramesRx != 20 || got.H1Direct.BytesTx != 1000 || got.H1Direct.BytesRx != 2000 {
		t.Fatalf("h1_direct = %+v", got.H1Direct)
	}
}

func TestBaselineTransportH3AndDatagramBlocks(t *testing.T) {
	modes := connect.TransportModeStatsSnapshot{
		H1FramesTx: 5, H3FramesTx: 7, H3FramesRx: 8, H3BytesTx: 700, H3BytesRx: 800,
		H3Attempts: 4, H3Connects: 3, H3ConnectFailures: 1, H3Drops: 2, H3Up: 1,
	}
	// attempted but DATAGRAM never offered: h3 block, no datagram block
	got := buildBaselineTransport(modes, connect.H3DatagramSnapshot{Accepted: 9, RxMessages: 9})
	if got.H3 == nil {
		t.Fatal("h3 block missing after H3 was attempted")
	}
	if got.H3.FramesTx != 7 || got.H3.BytesRx != 800 || got.H3.Up != 1 || got.H3.Attempts != 4 ||
		got.H3.Connects != 3 || got.H3.ConnectFailures != 1 || got.H3.Drops != 2 {
		t.Fatalf("h3 = %+v", got.H3)
	}
	if got.H3.Datagram != nil {
		t.Fatalf("datagram block must wait for an offer, got %+v", got.H3.Datagram)
	}

	got = buildBaselineTransport(modes, connect.H3DatagramSnapshot{Offered: 3, Accepted: 2, RxMessages: 40, RxBytes: 4000, RxDrops: 1})
	if got.H3.Datagram == nil {
		t.Fatal("datagram block missing after an offer")
	}
	if d := got.H3.Datagram; d.Offered != 3 || d.Accepted != 2 || d.RxMessages != 40 || d.RxBytes != 4000 || d.RxDrops != 1 {
		t.Fatalf("datagram = %+v", d)
	}
}

// The block reaches the written sample, under the field names a comparison
// reads, and is absent when the builder was not given one.
func TestBaselineSampleCarriesTransport(t *testing.T) {
	in := baselineInputs{now: time.Unix(1700000000, 0), snap: &NodeSnapshot{Version: "x"}}
	in.transport = buildBaselineTransport(
		connect.TransportModeStatsSnapshot{H1FramesTx: 1, H3Attempts: 1, H3FramesRx: 2},
		connect.H3DatagramSnapshot{Offered: 1, Accepted: 1},
	)
	line, err := json.Marshal(buildBaselineSample(in))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"transport":{`, `"h1":{"frames_tx":1`, `"h3":{`, `"datagram":{"offered":1,"accepted":1`} {
		if !strings.Contains(string(line), want) {
			t.Fatalf("sample missing %s: %s", want, line)
		}
	}
	in.transport = nil
	line, err = json.Marshal(buildBaselineSample(in))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(line), "transport") {
		t.Fatalf("no transport block expected when none was read: %s", line)
	}
}

func TestBaselineTransportDnsModesAbsentUntilAttempted(t *testing.T) {
	got := buildBaselineTransport(connect.TransportModeStatsSnapshot{H1FramesTx: 1}, connect.H3DatagramSnapshot{})
	if got.Dns != nil || got.DnsPump != nil {
		t.Fatalf("DNS modes must be absent until attempted: %+v %+v", got.Dns, got.DnsPump)
	}
	// the DNS modes are independent of H3: attempted DNS, never attempted H3
	got = buildBaselineTransport(connect.TransportModeStatsSnapshot{
		Dns:     connect.PtModeSnapshot{FramesTx: 3, BytesRx: 90, Attempts: 2, Connects: 1, ConnectFailures: 1, Up: 1},
		DnsPump: connect.PtModeSnapshot{Attempts: 1, ConnectFailures: 1},
	}, connect.H3DatagramSnapshot{})
	if got.H3 != nil {
		t.Fatalf("DNS activity must not create an h3 block: %+v", got.H3)
	}
	if got.Dns == nil || got.Dns.FramesTx != 3 || got.Dns.BytesRx != 90 || got.Dns.Attempts != 2 ||
		got.Dns.Connects != 1 || got.Dns.ConnectFailures != 1 || got.Dns.Up != 1 {
		t.Fatalf("dns = %+v", got.Dns)
	}
	if got.DnsPump == nil || got.DnsPump.Attempts != 1 || got.DnsPump.ConnectFailures != 1 {
		t.Fatalf("dns pump = %+v", got.DnsPump)
	}
	line, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"dns":{"frames_tx":3`, `"dns_pump":{`} {
		if !strings.Contains(string(line), want) {
			t.Fatalf("missing %s in %s", want, line)
		}
	}
}

func TestBaselineTransportDirectH1IsOptional(t *testing.T) {
	// No direct traffic at all: absent.
	got := buildBaselineTransport(connect.TransportModeStatsSnapshot{H1FramesTx: 100, H3Attempts: 1}, connect.H3DatagramSnapshot{})
	if got.H1Direct != nil {
		t.Fatalf("no direct H1 traffic: block must be absent, got %+v", got.H1Direct)
	}
	// A real direct split with H3 attempted: present.
	got = buildBaselineTransport(connect.TransportModeStatsSnapshot{
		H1FramesTx: 100, H1DirectFramesTx: 7, H1DirectBytesTx: 700, H3Attempts: 1,
	}, connect.H3DatagramSnapshot{})
	if got.H1Direct == nil || got.H1Direct.FramesTx != 7 || got.H1Direct.BytesTx != 700 {
		t.Fatalf("direct h1 = %+v", got.H1Direct)
	}
	if got.H1.FramesTx != 100 {
		t.Fatalf("h1 total must still be every identity: %+v", got.H1)
	}
	// Withheld until H3 has been attempted: the block exists for the H3
	// comparison, so before that it records nothing the H1 block does not.
	got = buildBaselineTransport(connect.TransportModeStatsSnapshot{
		H1FramesTx: 100, H1DirectFramesTx: 7, H1DirectBytesTx: 700,
	}, connect.H3DatagramSnapshot{})
	if got.H1Direct != nil {
		t.Fatalf("h1_direct must wait until H3 has been attempted, got %+v", got.H1Direct)
	}
}
