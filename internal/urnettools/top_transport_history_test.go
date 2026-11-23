package urnettools

import (
	"errors"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
)

// transportBlock builds a full transport block with the given payload, one
// value per kind. Every kind is present, which is what a provider that runs
// them all reports.
func transportBlock(h3, h1, h1Plus, dns, pump uint64) *LiveTransports {
	return &LiveTransports{
		H3:          &LiveTransport{BytesTx: h3},
		H1WebSocket: &LiveTransport{BytesTx: h1},
		H1Plus:      &LiveTransport{BytesTx: h1Plus},
		DNS:         &LiveTransport{BytesTx: dns},
		DNSPump:     &LiveTransport{BytesTx: pump},
	}
}

const transportTestBase int64 = 1_700_000_000_000_000_000

func nanos(d time.Duration) int64 { return int64(d) }

// A single reading is not a window: the page waits for the second one before
// it draws a share.
func TestTopTransportHistoryNeedsTwoReadings(t *testing.T) {
	var h topTransportHistory
	if _, ok := h.newest(); ok {
		t.Fatal("an empty ring offers a reading")
	}
	if _, _, ok := h.window(); ok {
		t.Fatal("an empty ring offers a window")
	}
	h.push(transportTestBase, transportBlock(1, 0, 0, 0, 0))
	if _, ok := h.newest(); !ok {
		t.Fatal("one reading is not offered")
	}
	if _, _, ok := h.window(); ok {
		t.Fatal("one reading cannot be differenced into a window")
	}
}

// The reference reading is the newest one at or before a window back, so the
// share covers exactly the window and not the whole session.
func TestTopTransportHistoryTakesTheReadingAWindowBack(t *testing.T) {
	var h topTransportHistory
	for _, offset := range []time.Duration{0, time.Second, 30 * time.Second, 59 * time.Second, 60 * time.Second} {
		h.push(transportTestBase+nanos(offset), transportBlock(uint64(offset), 0, 0, 0, 0))
	}
	prev, cur, ok := h.window()
	if !ok {
		t.Fatal("five readings did not produce a window")
	}
	if cur.at != transportTestBase+nanos(60*time.Second) {
		t.Fatalf("cur at %d, want the newest reading", cur.at-transportTestBase)
	}
	if prev.at != transportTestBase {
		t.Fatalf("prev at %+d, want the reading exactly a window back", prev.at-transportTestBase)
	}

	h.push(transportTestBase+nanos(61*time.Second), transportBlock(0, 0, 0, 0, 0))
	prev, _, ok = h.window()
	if !ok {
		t.Fatal("six readings did not produce a window")
	}
	if prev.at != transportTestBase+nanos(time.Second) {
		t.Fatalf("prev at %+d, want the newest reading at or before the window start", prev.at-transportTestBase)
	}
}

// A session shorter than a window still draws: the oldest reading held is the
// reference, and the caller divides by the span it actually covers.
func TestTopTransportHistoryFallsBackToTheOldestReading(t *testing.T) {
	var h topTransportHistory
	h.push(transportTestBase, transportBlock(0, 0, 0, 0, 0))
	h.push(transportTestBase+nanos(time.Second), transportBlock(1, 0, 0, 0, 0))
	prev, cur, ok := h.window()
	if !ok {
		t.Fatal("two readings did not produce a window")
	}
	if prev.at != transportTestBase || cur.at != transportTestBase+nanos(time.Second) {
		t.Fatalf("window %+d..%+d, want the oldest and newest readings",
			prev.at-transportTestBase, cur.at-transportTestBase)
	}
	if span := time.Duration(cur.at - prev.at); span != time.Second {
		t.Fatalf("span %v, want the 1s the readings cover", span)
	}
}

// A provider clock that steps backwards must not leave a reading that later
// readings can never pass, so it is dropped.
func TestTopTransportHistoryDropsABackwardsClock(t *testing.T) {
	var h topTransportHistory
	h.push(transportTestBase+nanos(10*time.Second), transportBlock(10, 0, 0, 0, 0))
	h.push(transportTestBase, transportBlock(1, 0, 0, 0, 0))
	newest, ok := h.newest()
	if !ok {
		t.Fatal("no reading kept")
	}
	if newest.at != transportTestBase+nanos(10*time.Second) {
		t.Fatalf("newest at %+d, want the earlier clock kept", newest.at-transportTestBase)
	}
	if newest.transports.H3 == nil || newest.transports.H3.BytesTx != 10 {
		t.Fatalf("kept the block from the backwards reading")
	}
}

// Two readings at the same instant are one point in time: the later block
// replaces the first instead of becoming a zero-span pair.
func TestTopTransportHistoryReplacesARepeatedInstant(t *testing.T) {
	var h topTransportHistory
	h.push(transportTestBase, transportBlock(1, 0, 0, 0, 0))
	h.push(transportTestBase, transportBlock(2, 0, 0, 0, 0))
	if n := len(h.readings); n != 1 {
		t.Fatalf("ring holds %d readings for one instant, want 1", n)
	}
	newest, _ := h.newest()
	if newest.transports.H3.BytesTx != 2 {
		t.Fatalf("kept h3=%d, want the later reading", newest.transports.H3.BytesTx)
	}
}

// Readings older than two windows are dropped, which still leaves one at or
// before the window start.
func TestTopTransportHistoryTrimsPastTwoWindows(t *testing.T) {
	var h topTransportHistory
	for i := 0; i <= 4; i++ {
		h.push(transportTestBase+nanos(time.Duration(i)*time.Minute), transportBlock(uint64(i), 0, 0, 0, 0))
	}
	if n := len(h.readings); n != 3 {
		t.Fatalf("ring holds %d readings, want the last 3 (two windows)", n)
	}
	if oldest := h.readings[0].at - transportTestBase; oldest != nanos(2*time.Minute) {
		t.Fatalf("oldest kept at %+d, want the reading two windows back", oldest)
	}
	prev, _, ok := h.window()
	if !ok {
		t.Fatal("the trimmed ring has no window")
	}
	if prev.at != transportTestBase+nanos(3*time.Minute) {
		t.Fatalf("prev at %+d, want the reading a window back", prev.at-transportTestBase)
	}
}

// An old provider sends no transport block, and the ring keeps nothing for it,
// which is what keeps the page out of the strip.
func TestTopTransportHistoryIgnoresAMissingBlock(t *testing.T) {
	var h topTransportHistory
	h.push(transportTestBase, nil)
	h.push(0, transportBlock(1, 0, 0, 0, 0))
	if _, ok := h.newest(); ok {
		t.Fatal("a nil block or a zero reading was kept")
	}
}

// The live model feeds the ring from the traffic polls it already makes, so the
// page needs no poll of its own.
func TestTopLiveTrafficFeedsTheTransportRing(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	at := transportTestBase
	for i := 1; i <= 2; i++ {
		clock.Advance(100 * time.Millisecond)
		gen, _, ok := m.wantTraffic(clock.Now())
		if !ok {
			t.Fatal("traffic not requested")
		}
		at += nanos(100 * time.Millisecond)
		m.applyTraffic(gen, &LiveTraffic{AtUnixNano: at, Transports: transportBlock(uint64(i), 0, 0, 0, 0)}, nil)
	}
	newest, ok := m.transportsHistory.newest()
	if !ok {
		t.Fatal("no reading reached the ring")
	}
	if newest.at != at {
		t.Fatalf("newest at %d, want the last poll at %d", newest.at, at)
	}
	if newest.transports.H3.BytesTx != 2 {
		t.Fatalf("kept h3=%d, want the last poll", newest.transports.H3.BytesTx)
	}
}

// A lost provider empties the ring, so counters from before the outage are not
// differenced against counters from after it.
func TestTopLostProviderEmptiesTheTransportRing(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	gen, _, ok := m.wantTraffic(clock.Now())
	if !ok {
		t.Fatal("traffic not requested")
	}
	m.applyTraffic(gen, &LiveTraffic{AtUnixNano: transportTestBase, Transports: transportBlock(1, 0, 0, 0, 0)}, nil)
	if _, ok := m.transportsHistory.newest(); !ok {
		t.Fatal("no reading reached the ring")
	}
	m.lost(clock.Now(), errors.New("provider went away"))
	if _, ok := m.transportsHistory.newest(); ok {
		t.Fatal("the ring kept a reading across an outage")
	}
}

// A provider that does not know the traffic command cannot report transports
// either, so the ring is dropped with the live window.
func TestTopUnsupportedTrafficEmptiesTheTransportRing(t *testing.T) {
	m, clock := liveModel(t, 100*time.Millisecond)
	gen, _, ok := m.wantTraffic(clock.Now())
	if !ok {
		t.Fatal("traffic not requested")
	}
	m.applyTraffic(gen, &LiveTraffic{AtUnixNano: transportTestBase, Transports: transportBlock(1, 0, 0, 0, 0)}, nil)
	m.applyTraffic(m.gen, nil, errTrafficUnsupported)
	if _, ok := m.transportsHistory.newest(); ok {
		t.Fatal("the ring survived an unsupported traffic reply")
	}
}

// The state the page starts from is empty, so a fresh model offers no page.
func TestTopTransportsPageHiddenForAnOldProvider(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.apply(0, topFlowing(t), nil)
	if m.transportsPageAvailable(tui.Rect{W: 80, H: 24}) {
		t.Fatal("the page is offered before any transport reading")
	}
}
