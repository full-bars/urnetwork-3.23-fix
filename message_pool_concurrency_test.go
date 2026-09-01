package connect

import (
	"fmt"
	"sync"
	"testing"
)

// TestAddSizeDistributionConcurrent hammers addSizeDistribution from many
// goroutines mixing the fixed pre-populated sizes (fast path, RLock+atomic
// Add) with novel sizes (cold path, write-lock + lazy map insert) and the
// EnhancedMetrics snapshot iterator, all at once.
//
// Under `-race` this is a regression guard for the G-M3 data race: the old
// code read the SizeDistribution map with NO lock on the fast path while the
// cold-path writer inserted under a plain Mutex — Go maps require
// synchronization on *every* access, so the unlocked fast-path read raced the
// cold-path write and crashed with `fatal: concurrent map read and map
// write`. This test would reproduce that crash pre-fix.
func TestAddSizeDistributionConcurrent(t *testing.T) {
	// Warm the fast-path entries + init once so the fixed sizes are present.
	initSizeDistOnce.Do(initSizeDist)

	const (
		fixedSizes = 5
		coldSizes  = 32
		iterations = 3000
		workers    = 8
	)
	// A deterministic set of novel sizes so cold-path entries are *shared*
	// across workers (contended writes), not trivially worker-private.
	var cold []int
	for i := 0; i < coldSizes; i++ {
		cold = append(cold, 2048+1+(i*7)) // 2049, 2056, ... all outside fixed set
	}

	fast := []int{2048, 4096, 16384, 32768, 65536}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				if i%20 == 0 {
					// Exercise the snapshot iterator (RLock map walk).
					_ = EnhancedMetrics()
				}
				if w%2 == 0 {
					addSizeDistribution(fast[i%len(fast)])
				} else {
					addSizeDistribution(cold[(w+i)%len(cold)])
				}
			}
		}(w)
	}
	wg.Wait()

	// Verify contends writes actually landed: every cold size we touched
	// must appear with a positive count after all workers finish.
	sd := EnhancedMetrics()["size_distribution"].(map[string]uint64)
	for _, size := range cold {
		key := fmt.Sprintf("%d", size)
		if sd[key] == 0 {
			t.Fatalf("cold-path size %d missing from size_distribution after concurrent add (got %d)", size, sd[key])
		}
	}
	// Each fixed size should have received hits from the workers that drove
	// it (at least some).
	for _, size := range fast {
		key := fmt.Sprintf("%d", size)
		if sd[key] == 0 {
			t.Fatalf("fixed size %d got zero distribution hits under concurrency (got %d)", size, sd[key])
		}
	}
}

// TestAddSizeDistributionColdPathSingle ensures the lazy cold path alone is
// correct and visible in the metric without any concurrency — the smallest
// unit that would regress if the write-lock path were dropped entirely.
func TestAddSizeDistributionColdPathSingle(t *testing.T) {
	initSizeDistOnce.Do(initSizeDist)
	novel := 4096 + 17 // 4113, not in {2048,4096,...}
	addSizeDistribution(novel)
	sd := EnhancedMetrics()["size_distribution"].(map[string]uint64)
	key := fmt.Sprintf("%d", novel)
	if sd[key] == 0 {
		t.Fatalf("expected novel size %d in size_distribution after direct add, got %v", novel, sd)
	}
}
