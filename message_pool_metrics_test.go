package connect

import (
	"testing"
)

func TestEnhancedMetricsShape(t *testing.T) {
	m := EnhancedMetrics()
	for _, key := range []string{"hits", "misses", "returns", "active_buffers", "pooled_buffers", "size_distribution", "last_reset_time", "gc_pauses"} {
		if _, ok := m[key]; !ok {
			t.Fatalf("EnhancedMetrics missing key %q", key)
		}
	}
	// A Get/Put round-trip must move the active count back to zero and
	// increment hits/misses/returns without panicking on any pool size.
	sizes := []int{2048, 4096, 16384, 32768, 65536, 12345} // 12345 is NOT in the fixed set -> exercises lazy size-distribution
	for _, size := range sizes {
		pool := newMessagePool(size, 4)
		b := pool.Get()
		pool.Put(b)
	}
	m2 := EnhancedMetrics()
	if m2["active_buffers"].(uint64) != 0 {
		t.Fatalf("expected active_buffers to return to 0 after round-trip, got %v", m2["active_buffers"])
	}
	if m2["returns"].(uint64) == 0 {
		t.Fatalf("expected returns > 0 after round-trip")
	}
}
