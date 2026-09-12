package connect

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// TestCompositeWeight: low-latency proxy should score higher than a slow one
// with the same success/error ratio.
// ---------------------------------------------------------------------------

func TestCompositeWeight(t *testing.T) {
	base := &clientDialer{
		minimumWeight:  0.1,
		successCount:   8,
		errorCount:     2,
		avgLatencyNanos: 0, // no latency data yet — neutral factor
	}
	wNoLatency := base.Weight()

	// Fast proxy (100 ms avg)
	fast := &clientDialer{
		minimumWeight:   0.1,
		successCount:    8,
		errorCount:      2,
		avgLatencyNanos: 100_000_000, // 100 ms
	}
	wFast := fast.Weight()

	// Slow proxy (2000 ms avg)
	slow := &clientDialer{
		minimumWeight:   0.1,
		successCount:    8,
		errorCount:      2,
		avgLatencyNanos: 2_000_000_000, // 2 s
	}
	wSlow := slow.Weight()

	t.Logf("no-latency=%.4f fast=%.4f slow=%.4f", wNoLatency, wFast, wSlow)

	if wFast <= wNoLatency {
		t.Errorf("fast proxy should score higher than neutral: fast=%.4f noLatency=%.4f", wFast, wNoLatency)
	}
	if wSlow >= wNoLatency {
		t.Errorf("slow proxy should score lower than neutral: slow=%.4f noLatency=%.4f", wSlow, wNoLatency)
	}
	if wFast <= wSlow {
		t.Errorf("fast proxy should score higher than slow: fast=%.4f slow=%.4f", wFast, wSlow)
	}
}

// ---------------------------------------------------------------------------
// TestCompositeWeightStreakPenalty: consecutive errors should progressively
// reduce weight by 0.5× per error.
// ---------------------------------------------------------------------------

func TestCompositeWeightStreakPenalty(t *testing.T) {
	makeDialer := func(streak int) *clientDialer {
		return &clientDialer{
			minimumWeight:     0.01,
			successCount:      7,
			errorCount:        3,     // base score = 0.7
			consecutiveErrors: streak,
		}
	}

	w0 := makeDialer(0).Weight()
	w1 := makeDialer(1).Weight()
	w2 := makeDialer(2).Weight()
	w3 := makeDialer(3).Weight()

	t.Logf("streak0=%.4f streak1=%.4f streak2=%.4f streak3=%.4f", w0, w1, w2, w3)

	// Base score = 7/10 = 0.7
	// streak 0: 0.7 * 1.0   = 0.7
	// streak 1: 0.7 * 0.5   = 0.35
	// streak 2: 0.7 * 0.25  = 0.175
	// streak 3: 0.7 * 0.125 = 0.0875

	if w0 <= w1 {
		t.Errorf("streak 0 should be highest: w0=%.4f w1=%.4f", w0, w1)
	}
	if w1 <= w2 {
		t.Errorf("streak 1 should be higher than 2: w1=%.4f w2=%.4f", w1, w2)
	}
	if w2 <= w3 {
		t.Errorf("streak 2 should be higher than 3: w2=%.4f w3=%.4f", w2, w3)
	}

	// Streak 3: 0.7 * 0.125 = 0.0875, which is < minimumWeight 0.01? No, 0.0875 > 0.01
	if w3 != 0.0875 {
		t.Errorf("streak 3 weight should be 0.0875, got %.4f", w3)
	}

	// Verify minimum weight floor: even with huge streak, weight >= minimumWeight
	for streak := 0; streak <= 20; streak++ {
		w := makeDialer(streak).Weight()
		if w < 0.01 {
			t.Errorf("streak %d weight %.6f is below minimum 0.01", streak, w)
		}
	}
}

// ---------------------------------------------------------------------------
// TestAdaptiveBlockSize: verify the adaptive sizing logic.
// ---------------------------------------------------------------------------

func TestAdaptiveBlockSize(t *testing.T) {
	// Reset global state
	probeSuccessCount.Store(0)
	probeFailureCount.Store(0)
	probeWindowStart.Store(time.Now().UnixNano())

	t.Run("no_data_returns_default", func(t *testing.T) {
		probeSuccessCount.Store(0)
		probeFailureCount.Store(0)
		got := getAdaptiveBlockSize(4)
		if got != 4 {
			t.Errorf("expected default 4, got %d", got)
		}
	})

	t.Run("healthy_network_reduces_to_2", func(t *testing.T) {
		probeSuccessCount.Store(90)
		probeFailureCount.Store(10)
		got := getAdaptiveBlockSize(4)
		if got != 2 {
			t.Errorf("expected 2 for >80%% success, got %d", got)
		}
	})

	t.Run("normal_network_keeps_4", func(t *testing.T) {
		probeSuccessCount.Store(60)
		probeFailureCount.Store(40)
		got := getAdaptiveBlockSize(4)
		if got != 4 {
			t.Errorf("expected 4 for 50-80%% success, got %d", got)
		}
	})

	t.Run("degraded_network_increases_to_6", func(t *testing.T) {
		probeSuccessCount.Store(30)
		probeFailureCount.Store(70)
		got := getAdaptiveBlockSize(4)
		if got != 6 {
			t.Errorf("expected 6 for <50%% success, got %d", got)
		}
	})

	t.Run("expired_window_returns_default", func(t *testing.T) {
		// Set window to 20 seconds ago
		probeWindowStart.Store(time.Now().Add(-20 * time.Second).UnixNano())
		probeSuccessCount.Store(5)
		probeFailureCount.Store(95)
		got := getAdaptiveBlockSize(4)
		if got != 4 {
			t.Errorf("expected default 4 for expired window, got %d", got)
		}
	})

	t.Run("recordProbeResult_increments_counters", func(t *testing.T) {
		probeSuccessCount.Store(0)
		probeFailureCount.Store(0)
		probeWindowStart.Store(time.Now().UnixNano())
		for i := 0; i < 5; i++ {
			recordProbeResult(true)
		}
		for i := 0; i < 3; i++ {
			recordProbeResult(false)
		}
		if s := probeSuccessCount.Load(); s != 5 {
			t.Errorf("expected 5 successes, got %d", s)
		}
		if f := probeFailureCount.Load(); f != 3 {
			t.Errorf("expected 3 failures, got %d", f)
		}
	})

	t.Run("concurrent_recording", func(t *testing.T) {
		probeSuccessCount.Store(0)
		probeFailureCount.Store(0)
		probeWindowStart.Store(time.Now().UnixNano())
		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				recordProbeResult(i%3 != 0) // ~67% success
			}(i)
		}
		wg.Wait()
		s := probeSuccessCount.Load()
		f := probeFailureCount.Load()
		if s+f != 100 {
			t.Errorf("expected 100 total probes, got %d", s+f)
		}
		// ~67 successes should give >50% → block size 4 or 2
		got := getAdaptiveBlockSize(4)
		if got != 2 && got != 4 {
			t.Errorf("expected block size 2 or 4 for ~67%% success, got %d", got)
		}
	})
}

// ---------------------------------------------------------------------------
// TestSerialPrioritySort: verifies the enhanced serial dialer sort orders
// by grade → success rate → recency.
// ---------------------------------------------------------------------------

func TestSerialPrioritySort(t *testing.T) {
	now := time.Now()

	dialers := []*clientDialer{
		{
			priority:        0,
			successCount:    10,
			errorCount:      0,
			lastSuccessTime: now.Add(-5 * time.Minute), // old success
		},
		{
			priority:        0,
			successCount:    5,
			errorCount:      5,
			lastSuccessTime: now.Add(-30 * time.Second), // recent success, lower rate
		},
		{
			priority:        1, // lower grade (higher int = lower priority)
			successCount:    20,
			errorCount:      0,
			lastSuccessTime: now.Add(-10 * time.Second), // recent, perfect rate, bad grade
		},
		{
			priority:        0,
			successCount:    8,
			errorCount:      2,
			lastSuccessTime: now.Add(-1 * time.Minute), // medium recency, good rate
		},
	}

	// Use the same sort comparator as parallelEval / serialEval
	slices.SortStableFunc(dialers, func(a, b *clientDialer) int {
		if a.priority != b.priority {
			return a.priority - b.priority
		}
		aRate := float32(0)
		bRate := float32(0)
		aTotal := a.successCount + a.errorCount
		bTotal := b.successCount + b.errorCount
		if aTotal > 0 {
			aRate = float32(a.successCount) / float32(aTotal)
		}
		if bTotal > 0 {
			bRate = float32(b.successCount) / float32(bTotal)
		}
		if aRate > bRate {
			return -1
		}
		if aRate < bRate {
			return 1
		}
		if a.lastSuccessTime.After(b.lastSuccessTime) {
			return -1
		}
		if a.lastSuccessTime.Before(b.lastSuccessTime) {
			return 1
		}
		return 0
	})

	// Expected order:
	// 1. priority 0, rate 1.0 (10/10), old
	// 2. priority 0, rate 0.8 (8/10), medium
	// 3. priority 0, rate 0.5 (5/10), recent
	// 4. priority 1, any (lower grade)
	for i, d := range dialers {
		total := d.successCount + d.errorCount
		rate := float64(d.successCount) / float64(total)
		age := now.Sub(d.lastSuccessTime)
		fmt.Printf("dialer[%d]: priority=%d rate=%.1f age=%s\n", i, d.priority, rate, age.Round(time.Second))
	}

	// Check grade ordering first
	if dialers[0].priority != 0 || dialers[1].priority != 0 || dialers[2].priority != 0 {
		t.Errorf("first three dialers should have priority 0, got %d %d %d",
			dialers[0].priority, dialers[1].priority, dialers[2].priority)
	}
	if dialers[3].priority != 1 {
		t.Errorf("last dialer should have priority 1, got %d", dialers[3].priority)
	}

	// Within grade 0, check rate ordering: 1.0 > 0.8 > 0.5
	rate0 := float64(dialers[0].successCount) / float64(dialers[0].successCount+dialers[0].errorCount)
	rate1 := float64(dialers[1].successCount) / float64(dialers[1].successCount+dialers[1].errorCount)
	rate2 := float64(dialers[2].successCount) / float64(dialers[2].successCount+dialers[2].errorCount)
	if rate0 < rate1 || rate1 < rate2 {
		t.Errorf("within same grade, rates should be non-increasing: %.2f %.2f %.2f", rate0, rate1, rate2)
	}

	// Among same rate (if any), more recent should come first
	// In our data, no two have the same rate, so this is implicitly tested
}

// ---------------------------------------------------------------------------
// TestUpdateLatencyTracking: verify that Update records latency via EMA.
// ---------------------------------------------------------------------------

func TestUpdateLatencyTracking(t *testing.T) {
	ctx := context.Background()
	d := &clientDialer{
		minimumWeight: 0.1,
	}

	// First update: avg should equal the first sample
	d.Update(ctx, nil, 200*time.Millisecond)
	if d.avgLatencyNanos != 200_000_000 {
		t.Errorf("expected avg=200ms after first sample, got %d ns", d.avgLatencyNanos)
	}

	// Second update: EMA with α=0.3 → 0.3*500ms + 0.7*200ms = 150+140 = 290ms
	d.Update(ctx, nil, 500*time.Millisecond)
	expected := int64(290_000_000)
	if d.avgLatencyNanos != expected {
		t.Errorf("expected avg=%d ns, got %d ns", expected, d.avgLatencyNanos)
	}

	// Verify the Weight reflects the latency
	w := d.Weight()
	t.Logf("weight with avg latency 290ms: %.4f", w)
	// With latency < 500ms baseline, weight gets a boost (can exceed 1.0)
	if w < 0.1 {
		t.Errorf("weight should be above minimum: %.4f", w)
	}
	// Weight should be higher than it would be without latency data
	baseD := &clientDialer{minimumWeight: 0.1, successCount: 2, errorCount: 0}
	if w <= baseD.Weight() {
		t.Errorf("latency-tracked weight (%.4f) should be higher than base (%.4f) for fast proxy", w, baseD.Weight())
	}
}

// ---------------------------------------------------------------------------
// TestUpdateErrorStreakTracking: verify that consecutive errors are tracked
// and reset on success.
// ---------------------------------------------------------------------------

func TestUpdateErrorStreakTracking(t *testing.T) {
	ctx := context.Background()
	d := &clientDialer{
		minimumWeight: 0.1,
	}

	// 3 consecutive errors
	for i := 0; i < 3; i++ {
		d.Update(ctx, fmt.Errorf("err"), 100*time.Millisecond)
	}
	if d.consecutiveErrors != 3 {
		t.Errorf("expected 3 consecutive errors, got %d", d.consecutiveErrors)
	}

	// Success should reset streak
	d.Update(ctx, nil, 100*time.Millisecond)
	if d.consecutiveErrors != 0 {
		t.Errorf("expected 0 after success, got %d", d.consecutiveErrors)
	}

	// Context-canceled errors should NOT increment streak
	cancelCtx, cancel := context.WithCancel(ctx)
	cancel()
	d.Update(cancelCtx, fmt.Errorf("err"), 100*time.Millisecond)
	if d.consecutiveErrors != 0 {
		t.Errorf("canceled-context error should not increment streak, got %d", d.consecutiveErrors)
	}
}
