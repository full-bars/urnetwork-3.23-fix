package main

import (
	"context"
	"math/bits"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/urnetwork/connect"
)

const (
	udpProbeWorkers = 2
	// At full pressure the grade pipeline already runs at its one-worker
	// floor; defer this optional sweep to preserve capacity for that work.
	udpProbeSkipPressure = 1.0
)

// runUDPProbeTick checks a bounded batch and joins every worker before returning.
// The supplied settings are a snapshot of online identities, not state entries.
func runUDPProbeTick(ctx context.Context, store *proxyUDPCheckStore, online []*connect.ProxySettings, now time.Time, recheckWindow, tick time.Duration, pressure float64, enabled bool) {
	if !enabled || pressure >= udpProbeSkipPressure || ctx.Err() != nil {
		return
	}
	workers := min(udpProbeWorkers, scaledProbeConcurrency(pressure), cap(store.slots))
	if workers <= 0 {
		return
	}
	settingsByKey := make(map[string]*connect.ProxySettings, len(online))
	candidates := make([]udpProbeCandidate, 0, len(online))
	store.mu.Lock()
	for _, settings := range online {
		if settings == nil {
			continue
		}
		key := settings.Key()
		if _, exists := settingsByKey[key]; exists {
			continue
		}
		settingsByKey[key] = settings
		candidate := udpProbeCandidate{key: key, online: true}
		if entry := store.entries[key]; entry != nil {
			if entry.done != nil {
				// An active check will publish a fresh answer itself.
				candidate.lastChecked = now
			} else {
				candidate.lastChecked = entry.at
				candidate.status = entry.result.Status
			}
		}
		candidates = append(candidates, candidate)
	}
	store.mu.Unlock()
	batch := selectUDPProbeBatch(now, candidates, recheckWindow, tick, rand.New(rand.NewSource(now.UnixNano())))
	workers = min(workers, len(batch))
	if workers == 0 {
		return
	}
	jobs := make(chan udpProbeCandidate)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for candidate := range jobs {
				if ctx.Err() != nil {
					return
				}
				store.Check(ctx, settingsByKey[candidate.key], time.Now())
			}
		}()
	}
	for _, candidate := range batch {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case jobs <- candidate:
		}
	}
	close(jobs)
	wg.Wait()
}

type udpProbeCandidate struct {
	key         string
	online      bool
	status      proxyUDPStatus
	lastChecked time.Time
	changed     bool
}

// selectUDPProbeBatch selects due online candidates without modifying the input.
// Invalid intervals return no batch. A nil rng disables jitter. Jitter is a
// whole number of candidates, up to floor(20% of the unjittered batch size).
func selectUDPProbeBatch(now time.Time, candidates []udpProbeCandidate, recheckWindow, tick time.Duration, rng *rand.Rand) []udpProbeCandidate {
	if recheckWindow <= 0 || tick <= 0 {
		return nil
	}
	var due []udpProbeCandidate
	online := 0
	for _, candidate := range candidates {
		if !candidate.online {
			continue
		}
		online++
		if candidate.lastChecked.IsZero() || !now.Before(candidate.lastChecked.Add(proxyUDPTTL(candidate.status))) {
			due = append(due, candidate)
		}
	}
	if len(due) == 0 {
		return nil
	}

	sort.Slice(due, func(i, j int) bool {
		a, b := due[i], due[j]
		if a.lastChecked.IsZero() != b.lastChecked.IsZero() {
			return a.lastChecked.IsZero()
		}
		if a.changed != b.changed {
			return a.changed
		}
		if !a.lastChecked.Equal(b.lastChecked) {
			return a.lastChecked.Before(b.lastChecked)
		}
		return a.key < b.key
	})

	// ceil(online * tick / recheckWindow), without overflow or float rounding.
	hi, lo := bits.Mul64(uint64(online), uint64(tick))
	// At twice the online count, even maximum negative jitter covers all due.
	if hi >= uint64(recheckWindow) {
		return due
	}
	quotient, remainder := bits.Div64(hi, lo, uint64(recheckWindow))
	if quotient >= 2*uint64(online) {
		return due
	}
	if remainder != 0 {
		quotient++
	}
	// Counts this large cannot be represented as an int or allocated in a
	// candidate slice. The available due candidates are the practical limit.
	if quotient > uint64(^uint(0)>>1) {
		return due
	}
	batch := int(quotient)
	if rng != nil {
		span := batch / 5
		if span > 0 {
			delta := rng.Intn(2*span+1) - span
			if delta > 0 && delta > len(due)-batch {
				batch = len(due)
			} else {
				batch += delta
			}
		}
	}
	if batch < 1 {
		batch = 1
	}
	if batch > len(due) {
		batch = len(due)
	}
	return due[:batch]
}
