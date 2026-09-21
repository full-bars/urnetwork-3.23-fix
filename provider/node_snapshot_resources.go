package main

import (
	"math"
	"runtime"
	"runtime/debug"
)

// collectResources reads the process resource figures for the node snapshot
// and the resource gauges. Fields the platform cannot supply stay zero and
// are omitted from the JSON.
func collectResources() SnapshotResources {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	r := SnapshotResources{
		HeapInuseBytes: ms.HeapInuse,
		Goroutines:     runtime.NumGoroutine(),
	}
	// A negative input reads the limit without changing it. MaxInt64 is the
	// runtime's "no limit".
	if lim := debug.SetMemoryLimit(-1); lim != math.MaxInt64 && lim > 0 {
		r.MemLimitBytes = uint64(lim)
	}
	readProcResources(&r)
	return r
}
