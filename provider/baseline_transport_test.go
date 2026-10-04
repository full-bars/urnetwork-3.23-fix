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
