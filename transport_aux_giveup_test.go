package connect

import (
	"testing"
	"time"
)

func TestAuxGiveUpDisabledByZero(t *testing.T) {
	var g auxCarrierGiveUp
	now := time.Now()
	for i := 0; i < 10; i++ {
		if g.recordFailure(now, 0, time.Minute) {
			t.Fatal("a zero give-up threshold must never park")
		}
	}
	if got := g.waitUntil(now); got != 0 {
		t.Fatalf("waitUntil = %v, want 0 while disabled", got)
	}
}

func TestAuxGiveUpParksAfterConsecutiveFailures(t *testing.T) {
	var g auxCarrierGiveUp
	now := time.Now()
	window := 30 * time.Minute

	if g.recordFailure(now, 3, window) {
		t.Fatal("first failure must not park")
	}
	if got := g.waitUntil(now); got != 0 {
		t.Fatalf("waitUntil = %v before the threshold", got)
	}
	if g.recordFailure(now, 3, window) {
		t.Fatal("second failure must not park")
	}
	if !g.recordFailure(now, 3, window) {
		t.Fatal("the third consecutive failure must park")
	}
	if got := g.waitUntil(now); got != window {
		t.Fatalf("waitUntil = %v, want %v", got, window)
	}
	if got := g.waitUntil(now.Add(window)); got != 0 {
		t.Fatalf("waitUntil after the window = %v, want 0 (one probe allowed)", got)
	}
	// the probe failed: one more failure parks again for a fresh window
	if !g.recordFailure(now.Add(window), 3, window) {
		t.Fatal("a probe failure after the window must re-park")
	}
	if got := g.waitUntil(now.Add(window + time.Second)); got != window-time.Second {
		t.Fatalf("re-park remaining = %v", got)
	}
}

func TestAuxGiveUpHealthyClearsTheRun(t *testing.T) {
	var g auxCarrierGiveUp
	now := time.Now()
	window := time.Minute

	g.recordFailure(now, 3, window)
	g.recordFailure(now, 3, window)
	g.recordHealthy()
	if g.failures != 0 {
		t.Fatalf("healthy must clear the consecutive count, got %d", g.failures)
	}
	if g.recordFailure(now, 3, window) || g.recordFailure(now, 3, window) {
		t.Fatal("two failures after a healthy session must not park")
	}
	if !g.recordFailure(now, 3, window) {
		t.Fatal("the third failure after a healthy session must park")
	}
}

func TestAuxGiveUpYoungDeathCountsLikeFailure(t *testing.T) {
	// A session that authenticated and died young travels through the same
	// recordFailure path by design; this pins the accounting.
	var g auxCarrierGiveUp
	now := time.Now()
	window := time.Minute
	g.recordFailure(now, 2, window)
	if !g.recordFailure(now, 2, window) {
		t.Fatal("two young deaths must park like two failed connects")
	}
}

func TestAuxGiveUpClearOnEvents(t *testing.T) {
	var g auxCarrierGiveUp
	now := time.Now()
	window := time.Minute
	g.recordFailure(now, 2, window)
	g.recordFailure(now, 2, window)
	if got := g.waitUntil(now); got == 0 {
		t.Fatal("expected a park before the clear")
	}
	g.clear()
	if got := g.waitUntil(now); got != 0 {
		t.Fatalf("clear must unpark, got %v", got)
	}
	if g.failures != 0 {
		t.Fatalf("clear must reset the count, got %d", g.failures)
	}
}

func TestAuxRetryWaitPicksTheLonger(t *testing.T) {
	if got := auxRetryWait(5*time.Second, 10*time.Minute); got != 10*time.Minute {
		t.Fatalf("park must extend the wait, got %v", got)
	}
	if got := auxRetryWait(10*time.Minute, 5*time.Second); got != 10*time.Minute {
		t.Fatalf("backoff must win when longer, got %v", got)
	}
	if got := auxRetryWait(5*time.Second, 0); got != 5*time.Second {
		t.Fatalf("no park must leave the backoff alone, got %v", got)
	}
}

func TestAuxGiveUpDefaultsStayDisabled(t *testing.T) {
	settings := DefaultPlatformTransportSettings()
	if settings.AuxCarrierGiveUpAfter != 0 {
		t.Fatalf("AuxCarrierGiveUpAfter default = %d, want 0 (disabled)", settings.AuxCarrierGiveUpAfter)
	}
	if settings.AuxCarrierParkWindow <= 0 || settings.AuxCarrierHealthyFloor <= 0 {
		t.Fatal("the park window and healthy floor defaults must be positive even while disabled")
	}
}
