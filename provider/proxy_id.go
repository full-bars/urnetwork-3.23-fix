package main

import "sync/atomic"

// proxyIDCounter is the global monotonic proxy ID counter.
// IDs are never reused — even after removal and re-addition.
var proxyIDCounter int64

// nextProxyID allocates the next never-reused proxy ID.
func nextProxyID() int {
	return int(atomic.AddInt64(&proxyIDCounter, 1) - 1)
}

// initProxyIDCounter fast-forwards the monotonic counter above highestExistingID
// so IDs from previous runs stored in proxy.state are never reused.
func initProxyIDCounter(highestExistingID int) {
	// ID 0 is reserved for the native [direct] connection.
	// All external proxies must start at 1 or higher.
	if highestExistingID < 0 {
		highestExistingID = 0
	}

	target := int64(highestExistingID + 1)
	for {
		cur := atomic.LoadInt64(&proxyIDCounter)
		if cur >= target {
			return
		}
		if atomic.CompareAndSwapInt64(&proxyIDCounter, cur, target) {
			return
		}
	}
}

// currentProxyIDCounter returns the current counter value, for state snapshots.
func currentProxyIDCounter() int {
	return int(atomic.LoadInt64(&proxyIDCounter))
}
