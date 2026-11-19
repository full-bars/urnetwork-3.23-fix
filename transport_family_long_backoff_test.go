package connect

import (
	"context"
	"testing"
	"time"
)

func newLongTestBackoff() *pinnedDialBackoff {
	b := newDialFailureBackoff(5*time.Second, 60*time.Second)
	b.setLongTier(10, 10*time.Minute)
	return b
}

// An outage fails every dial, but none of those failures count as isolated, so
// the wait never passes the 60 s cap and recovery is as fast as before.
func TestDialBackoffLongTierIgnoresPlatformWideFailures(t *testing.T) {
	b := newLongTestBackoff()
	for i := 0; i < 60; i++ {
		b.failIsolated(false)
		if d := b.delay(); 60*time.Second < d {
			t.Fatalf("failure %d: a platform-wide failure waited %v, past the 60 s cap", i+1, d)
		}
	}
}

func TestDialBackoffLongTierEntersAfterIsolatedFailuresOnly(t *testing.T) {
	b := newLongTestBackoff()
	enteredBefore := streamMetrics.dialBackoffLongEntered.Load()
	activeBefore := streamMetrics.dialBackoffLongActive.Load()

	for i := 1; i < 10; i++ {
		b.failIsolated(true)
		if d := b.delay(); 60*time.Second < d {
			t.Fatalf("isolated failure %d is below the threshold but waited %v", i, d)
		}
	}
	if streamMetrics.dialBackoffLongEntered.Load() != enteredBefore {
		t.Fatal("the long tier must not be entered before the threshold")
	}

	b.failIsolated(true) // the 10th
	d := b.delay()
	if d < 5*time.Minute || 10*time.Minute < d {
		t.Fatalf("in the long tier the wait must be jittered in [5m, 10m], got %v", d)
	}
	if streamMetrics.dialBackoffLongEntered.Load()-enteredBefore != 1 || streamMetrics.dialBackoffLongActive.Load()-activeBefore != 1 {
		t.Fatal("entering the tier must move the counters once")
	}
	b.failIsolated(true) // staying in it is not a second entry
	if streamMetrics.dialBackoffLongEntered.Load()-enteredBefore != 1 {
		t.Fatal("a further failure must not count as another entry")
	}

	b.reset()
	if b.delay() != 0 {
		t.Fatal("a success must clear the wait")
	}
	if streamMetrics.dialBackoffLongActive.Load() != activeBefore {
		t.Fatal("leaving the tier must give the active gauge back")
	}
}

// Platform-wide failures mixed in do not count toward the long tier.
func TestDialBackoffLongTierCountsOnlyIsolatedFailures(t *testing.T) {
	b := newLongTestBackoff()
	for i := 0; i < 9; i++ {
		b.failIsolated(true)
	}
	for i := 0; i < 30; i++ { // an outage window
		b.failIsolated(false)
	}
	if d := b.delay(); 60*time.Second < d {
		t.Fatalf("9 isolated failures plus an outage must stay under the cap, got %v", d)
	}
	b.failIsolated(true)
	if d := b.delay(); d < 5*time.Minute {
		t.Fatalf("the 10th isolated failure must enter the long tier, got %v", d)
	}
	b.reset()
}

func TestDialBackoffLongTierDisabledKeepsTheSingleCap(t *testing.T) {
	b := newDialFailureBackoff(5*time.Second, 60*time.Second)
	for i := 0; i < 50; i++ {
		b.failIsolated(true)
		if d := b.delay(); 60*time.Second < d {
			t.Fatalf("with no long tier the wait must stay under 60 s, got %v", d)
		}
	}
	b.setLongTier(10, 30*time.Second) // a long cap not above the normal one is ignored
	b.failIsolated(true)
	if d := b.delay(); 60*time.Second < d {
		t.Fatalf("a long cap below the normal cap must be ignored, got %v", d)
	}
	b.reset()
}

func TestPlatformReachedRecently(t *testing.T) {
	saved := lastPlatformSuccessNano.Load()
	defer lastPlatformSuccessNano.Store(saved)

	lastPlatformSuccessNano.Store(0)
	if platformReachedRecently() {
		t.Fatal("no success yet means unknown, which is not reachable")
	}
	noteBackendSuccess()
	if !platformReachedRecently() {
		t.Fatal("a success just now means the platform is reachable")
	}
	lastPlatformSuccessNano.Store(time.Now().Add(-3 * time.Minute).UnixNano())
	if platformReachedRecently() {
		t.Fatal("a success older than the window is no longer evidence")
	}
}

// noteDialFailure feeds the long tier only while the platform is reachable.
func TestNoteDialFailureUsesReachabilityToEnterTheLongTier(t *testing.T) {
	saved := lastPlatformSuccessNano.Load()
	defer lastPlatformSuccessNano.Store(saved)

	outage := &PlatformTransport{failBackoff: newLongTestBackoff()}
	lastPlatformSuccessNano.Store(0)
	for i := 0; i < 40; i++ {
		outage.noteDialFailure(context.DeadlineExceeded)
	}
	if d := outage.failBackoff.delay(); 60*time.Second < d {
		t.Fatalf("failures with no platform success anywhere must not enter the tier, got %v", d)
	}

	dead := &PlatformTransport{failBackoff: newLongTestBackoff()}
	lastPlatformSuccessNano.Store(time.Now().UnixNano())
	for i := 0; i < 10; i++ {
		dead.noteDialFailure(context.DeadlineExceeded)
	}
	if d := dead.failBackoff.delay(); d < 5*time.Minute {
		t.Fatalf("10 failures while the platform answers others must enter the tier, got %v", d)
	}
	dead.failBackoff.reset()
	outage.failBackoff.reset()
}
