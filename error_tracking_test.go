package connect

import (
	"testing"
	"time"
)

func TestRecordErrorAndMetrics(t *testing.T) {
	RecordError(ErrorTransport, "dial timeout")
	RecordError(ErrorTransport, "reset")
	RecordError(ErrorIP, "no route")
	got := ErrorMetrics()
	if _, ok := got["transport"]; !ok {
		t.Fatalf("expected transport category in metrics, got %v", got)
	}
	if got["transport"].(map[string]interface{})["count"].(int) < 1 {
		t.Fatalf("expected >=1 transport error, got %v", got)
	}
}

func TestRateLimiterAllow(t *testing.T) {
	// Use a 1s window so all three calls land within it; the limiter
	// allows 2 then throttles the 3rd. (window=0 would prune every
	// stored timestamp via strict After, so it never rate-limits.)
	rl := NewRateLimiter(2, time.Second)
	rl.Allow()
	rl.Allow()
	if rl.Allow() {
		t.Fatalf("expected 3rd Allow to be false")
	}
}
