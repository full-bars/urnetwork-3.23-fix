package urnettools

import (
	"testing"
	"time"
)

func TestTopCarrierResetAboveMinuteBaseline(t *testing.T) {
	var h topCarrierHistory
	start := time.Unix(1000, 0)
	h.push(start, "a", 100)
	h.push(start.Add(10*time.Second), "a", 200)
	h.push(start.Add(20*time.Second), "a", 150)
	h.push(start.Add(70*time.Second), "a", 180)
	if got := h.per["a"].ring; len(got) != 0 {
		t.Fatalf("reset created premature rate: %v", got)
	}
	h.push(start.Add(80*time.Second), "a", 210)
	if got := h.per["a"].ring; len(got) != 1 || got[0] != 60 {
		t.Fatalf("post-reset rate = %v, want [60]", got)
	}
}
