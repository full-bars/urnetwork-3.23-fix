package urnettools

import (
	"math"
	"testing"
	"time"
)

const ms = int64(time.Millisecond)

func TestTopLiveSteadyRate(t *testing.T) {
	var l topLive
	at := int64(5_000_000_000)
	for i := 0; i <= 20; i++ { // a poll every 100ms
		l.add(at, uint64(i)*1000, uint64(i)*2500)
		at += 100 * ms
	}
	b, tot, ok := l.rates()
	if !ok || math.Abs(b-10_000) > 1 || math.Abs(tot-25_000) > 1 {
		t.Fatalf("rates = %.1f / %.1f ok=%v, want 10000 / 25000", b, tot, ok)
	}
}

// The rate must follow the traffic, not a stale average: once the counters stop
// moving for a full window, the rate is zero.
func TestTopLiveRateFallsToZeroWhenTrafficStops(t *testing.T) {
	var l topLive
	at, bytes := int64(0), uint64(0)
	for i := 0; i < 10; i++ {
		bytes += 5000
		l.add(at, bytes, bytes)
		at += 100 * ms
	}
	if b, _, _ := l.rates(); b < 40_000 {
		t.Fatalf("rate while flowing = %.0f, want about 50000", b)
	}
	for i := 0; i < 15; i++ { // a second and a half of silence
		l.add(at, bytes, bytes)
		at += 100 * ms
	}
	if b, tot, ok := l.rates(); !ok || b != 0 || tot != 0 {
		t.Fatalf("rate after silence = %.1f / %.1f ok=%v, want 0", b, tot, ok)
	}
}

func TestTopLiveWindowIsBounded(t *testing.T) {
	var l topLive
	at := int64(0)
	for i := 0; i < 10_000; i++ {
		l.add(at, uint64(i), uint64(i))
		at += 100 * ms
	}
	if len(l.samples) > 13 {
		t.Fatalf("kept %d samples for a one second window at 100ms polls", len(l.samples))
	}
}

// A proxy respawn hands back zeroed counters, so the sum drops. That is not
// negative traffic and must not poison the rate.
func TestTopLiveCounterDropStartsANewWindow(t *testing.T) {
	var l topLive
	at := int64(0)
	for i := 0; i <= 10; i++ {
		l.add(at, uint64(i)*1000, uint64(i)*1000)
		at += 100 * ms
	}
	l.add(at, 200, 200) // the sum dropped
	at += 100 * ms
	if _, _, ok := l.rates(); ok {
		t.Fatal("a rate was reported straight after a counter drop")
	}
	l.add(at, 1200, 1200)
	b, _, ok := l.rates()
	if !ok || b < 0 || math.Abs(b-10_000) > 1 {
		t.Fatalf("rate after the drop = %.1f ok=%v, want 10000 and never negative", b, ok)
	}
}

// At a slow poll (2s and up) the window still spans two readings.
func TestTopLiveSlowPollUsesTheLastTwoReadings(t *testing.T) {
	var l topLive
	l.add(0, 0, 0)
	l.add(2*int64(time.Second), 20_000, 40_000)
	b, tot, ok := l.rates()
	if !ok || b != 10_000 || tot != 20_000 {
		t.Fatalf("rates = %.0f / %.0f ok=%v, want 10000 / 20000", b, tot, ok)
	}
}

func TestTopLiveNeedsTwoUsableReadings(t *testing.T) {
	var l topLive
	if _, _, ok := l.rates(); ok {
		t.Fatal("rate from no readings")
	}
	l.add(100, 5, 5)
	if _, _, ok := l.rates(); ok {
		t.Fatal("rate from one reading")
	}
	l.add(100, 9, 9) // the same instant again
	l.add(90, 9, 9)  // the clock went backwards
	if _, _, ok := l.rates(); ok || len(l.samples) != 1 {
		t.Fatalf("a non-advancing clock was accepted: ok=%v samples=%d", ok, len(l.samples))
	}
	l.add(100+10*ms, 6, 6) // too close together to say anything
	if _, _, ok := l.rates(); ok {
		t.Fatal("rate from readings 10ms apart")
	}
}
