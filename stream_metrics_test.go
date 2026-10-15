package connect

import (
	"strings"
	"testing"
	"time"
)

type streamMetricsSnapshot struct {
	opened        [streamKindCount]int64
	closed        [streamKindCount][streamEndReasonCount]int64
	lifetimeCount [streamKindCount]int64
	openRequests  [streamOpenResultCount]int64
	live          [streamKindCount]int64
}

func snapshotStreamMetrics(m *streamMetricsState) streamMetricsSnapshot {
	var s streamMetricsSnapshot
	for k := streamKind(0); k < streamKindCount; k += 1 {
		s.opened[k] = m.opened[k].Load()
		s.lifetimeCount[k] = m.lifetimeCount[k].Load()
		s.live[k] = m.liveByKind(k)
		for r := 0; r < streamEndReasonCount; r += 1 {
			s.closed[k][r] = m.closed[k][r].Load()
		}
	}
	for r := 0; r < streamOpenResultCount; r += 1 {
		s.openRequests[r] = m.openRequests[r].Load()
	}
	return s
}

func TestStreamKindOf(t *testing.T) {
	a, b := NewId(), NewId()
	if streamKindOf(nil, &b) != streamKindSingle || streamKindOf(&a, nil) != streamKindSingle || streamKindOf(nil, nil) != streamKindSingle {
		t.Fatal("a stream with at most one end is single")
	}
	if streamKindOf(&a, &b) != streamKindTwoEnded {
		t.Fatal("a stream with both ends is two_ended")
	}
}

// TestStreamMetricsAccountForOpensClosesAndReasons drives real OpenStream /
// CloseStream through the fake-transport seam and checks each outcome moves
// exactly its own counters. Counters are process-wide, so the test compares
// deltas.
func TestStreamMetricsAccountForOpensClosesAndReasons(t *testing.T) {
	reaped := make(chan *StreamSequence, 16)
	rec := &transportRecorder{}
	settings := &StreamBufferSettings{
		P2pTransportNewForTest: func(sequence *StreamSequence) []*P2pTransport {
			transport, cancel := fakeChildTransport(sequence)
			rec.add(cancel)
			return []*P2pTransport{transport}
		},
		StreamSequenceReapedForTest: func(sequence *StreamSequence) { reaped <- sequence },
	}
	buffer := newLifetimeTestBuffer(t, settings)
	m := &streamMetrics
	single := int(streamKindSingle)

	base := snapshotStreamMetrics(m)
	destinationId := NewId()
	streamId := NewId()

	// 1. a new stream
	if _, err := buffer.OpenStream(nil, &destinationId, streamId); err != nil {
		t.Fatal(err)
	}
	rec.await(t, 1)
	s := snapshotStreamMetrics(m)
	if s.opened[single]-base.opened[single] != 1 || s.openRequests[streamOpenCreated]-base.openRequests[streamOpenCreated] != 1 || s.live[single]-base.live[single] != 1 {
		t.Fatalf("a new stream must count as one open, one created, one live; got %+v vs %+v", s, base)
	}

	// 2. the same stream asked for again reuses the sequence
	if _, err := buffer.OpenStream(nil, &destinationId, streamId); err != nil {
		t.Fatal(err)
	}
	s = snapshotStreamMetrics(m)
	if s.openRequests[streamOpenExisting]-base.openRequests[streamOpenExisting] != 1 || s.opened[single]-base.opened[single] != 1 {
		t.Fatalf("a repeated open must count as existing without a new sequence; got %+v", s)
	}

	// 3. the transport gives up: reason transport_ended, lifetime observed, live back
	rec.at(t, 0)()
	awaitReap(t, reaped)
	s = snapshotStreamMetrics(m)
	if s.closed[single][streamEndTransportEnded]-base.closed[single][streamEndTransportEnded] != 1 ||
		s.closed[single][streamEndCancelled] != base.closed[single][streamEndCancelled] ||
		s.lifetimeCount[single]-base.lifetimeCount[single] != 1 || s.live[single] != base.live[single] {
		t.Fatalf("a transport-ended reap must count once with that reason; got %+v vs %+v", s, base)
	}

	// 4. a platform close: reason cancelled
	other := NewId()
	if _, err := buffer.OpenStream(nil, &destinationId, other); err != nil {
		t.Fatal(err)
	}
	rec.await(t, 2)
	buffer.CloseStream(other)
	awaitReap(t, reaped)
	s2 := snapshotStreamMetrics(m)
	if s2.closed[single][streamEndCancelled]-s.closed[single][streamEndCancelled] != 1 ||
		s2.closed[single][streamEndTransportEnded] != s.closed[single][streamEndTransportEnded] {
		t.Fatalf("a close frame must count as cancelled; got %+v vs %+v", s2, s)
	}

	// 5. the same stream id under a different source replaces the old sequence
	src1, src2 := NewId(), NewId()
	id := NewId()
	if _, err := buffer.OpenStream(&src1, nil, id); err != nil {
		t.Fatal(err)
	}
	rec.await(t, 3)
	before := snapshotStreamMetrics(m)
	if _, err := buffer.OpenStream(&src2, nil, id); err != nil {
		t.Fatal(err)
	}
	after := snapshotStreamMetrics(m)
	if after.openRequests[streamOpenReplaced]-before.openRequests[streamOpenReplaced] != 1 {
		t.Fatalf("a reopen under another source must count as replaced; got %+v vs %+v", after, before)
	}
	awaitReap(t, reaped) // the replaced sequence is cancelled and reaped
}

func TestStreamLifetimeHistogramBuckets(t *testing.T) {
	var m streamMetricsState
	k := streamKindSingle
	for _, d := range []time.Duration{500 * time.Millisecond, 5 * time.Second, 2 * time.Hour, 48 * time.Hour} {
		m.observeLifetime(k, d)
	}
	// bounds: 1,10,60,300,1800,3600,21600,86400 then +Inf
	want := map[int]int64{0: 1, 1: 1, 6: 1, 8: 1}
	for i := 0; i < streamLifetimeBucketCount; i += 1 {
		if got := m.lifetimeBuckets[k][i].Load(); got != want[i] {
			t.Fatalf("bucket %d: want %d got %d", i, want[i], got)
		}
	}
	if m.lifetimeCount[k].Load() != 4 {
		t.Fatalf("count: %d", m.lifetimeCount[k].Load())
	}
	var b strings.Builder
	m.writePrometheus(&b)
	out := b.String()
	// cumulative buckets: <=1s:1, <=10s:2, ..., <=21600:3, <=86400:3, +Inf:4
	for _, line := range []string{
		`urnet_stream_sequence_lifetime_seconds_bucket{kind="single",le="1"} 1`,
		`urnet_stream_sequence_lifetime_seconds_bucket{kind="single",le="10"} 2`,
		`urnet_stream_sequence_lifetime_seconds_bucket{kind="single",le="21600"} 3`,
		`urnet_stream_sequence_lifetime_seconds_bucket{kind="single",le="+Inf"} 4`,
		`urnet_stream_sequence_lifetime_seconds_count{kind="single"} 4`,
		`urnet_stream_sequences_by_kind{kind="two_ended"} 0`,
	} {
		if !strings.Contains(out, line) {
			t.Fatalf("missing %q in:\n%s", line, out)
		}
	}
}

func TestStreamMetricsLiveNeverDriftsFromOpenedMinusClosed(t *testing.T) {
	var m streamMetricsState
	m.opened[streamKindTwoEnded].Add(5)
	m.closed[streamKindTwoEnded][streamEndCancelled].Add(2)
	m.closed[streamKindTwoEnded][streamEndTransportEnded].Add(1)
	if got := m.liveByKind(streamKindTwoEnded); got != 2 {
		t.Fatalf("live = opened - closed, got %d", got)
	}
}
