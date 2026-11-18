package main

import (
	"math/bits"
	"math/rand"
	"sort"
	"time"
)

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
