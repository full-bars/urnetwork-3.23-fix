package main

// addProxyDeltas folds cur into prev and returns the sum of positive deltas for
// keys that already had a baseline. A key seen for the first time only records
// its baseline: a record can now arrive carrying the totals persisted for its
// identity, and counting that as traffic would report one tick at the lifetime
// rate. Keys that left cur are pruned, so a churned proxy does not keep a slot.
//
// This is the same rule runBillableRateWriter applies inline; the rate and
// profit lines use it so every per-proxy delta in the provider treats a first
// sighting and a counter reset the same way.
func addProxyDeltas(prev, cur map[string]uint64) (delta uint64) {
	for key, v := range cur {
		if p, ok := prev[key]; ok && v >= p {
			delta += v - p
		}
		prev[key] = v
	}
	for key := range prev {
		if _, ok := cur[key]; !ok {
			delete(prev, key)
		}
	}
	return delta
}
