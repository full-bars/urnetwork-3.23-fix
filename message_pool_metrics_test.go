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
	// ActiveBuffers is a process-global gauge shared with other tests that
	// called Get() without Put(), so it is NOT expected to be 0 here. We
	// assert on the DELTA my round-trip produces instead.
	before := m["active_buffers"].(uint64)
	beforeReturns := m["returns"].(uint64)

	// A Get/Put round-trip must net out the active count and increment returns,
	// without panicking on any pool size.
	sizes := []int{2048, 4096, 16384, 32768, 65536, 12345} // 12345 is NOT in the fixed set -> exercises lazy size-distribution
	for _, size := range sizes {
		pool := newMessagePool(size, 4)
		b := pool.Get()
		pool.Put(b)
	}
	m2 := EnhancedMetrics()
	after := m2["active_buffers"].(uint64)
	if after != before {
		t.Fatalf("expected active_buffers to net back to %d after round-trip, got %d", before, after)
	}
	if m2["returns"].(uint64)-beforeReturns != uint64(len(sizes)) {
		t.Fatalf("expected %d returns after round-trip, got %d", len(sizes), m2["returns"].(uint64)-beforeReturns)
	}
	// Each new pool size must appear in the distribution (incl. 12345).
	sd := m2["size_distribution"].(map[string]uint64)
	if _, ok := sd["12345"]; !ok || sd["12345"] == 0 {
		t.Fatalf("expected size 12345 in size_distribution, got %v", sd)
	}
}
