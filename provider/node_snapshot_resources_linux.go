package main

import (
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// procCache is the TTL-guarded cache for the per-process resource reads. The
// collector rebuilds at most once a second, so a short TTL still gives every
// second a fresh value while letting two rebuilds inside one window share the
// reads.
var procCache struct {
	mu      sync.Mutex
	at      time.Time
	threads uint64
	tasks   uint64
	swapU   uint64
	swapT   uint64
	hasSwap bool
}

const procCacheTTL = 60 * time.Second

func readProcResources(r *SnapshotResources) {
	if data, err := os.ReadFile("/proc/self/statm"); err == nil {
		// statm: size resident shared text lib data dt, all in pages.
		if f := strings.Fields(string(data)); len(f) >= 2 {
			if pages, err := strconv.ParseUint(f[1], 10, 64); err == nil {
				r.RSSBytes = pages * uint64(os.Getpagesize())
			}
		}
	}
	if entries, err := os.ReadDir("/proc/self/fd"); err == nil && len(entries) > 0 {
		// ReadDir holds one descriptor of its own open while it lists.
		r.OpenFDs = uint64(len(entries) - 1)
	}
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err == nil && rl.Cur != 0 && uint64(rl.Cur) != math.MaxUint64 {
		r.FDLimit = uint64(rl.Cur)
	}
	threads, tasks, swapU, swapT, hasSwap := readCachedProcess()
	r.Threads, r.TasksMax, r.SwapUsedBytes, r.SwapTotalBytes = threads, tasks, swapU, swapT
	_ = hasSwap
}

// readCachedProcess serves the cached figures within the TTL.
func readCachedProcess() (threads, tasks, swapU, swapT uint64, hasSwap bool) {
	now := time.Now()
	procCache.mu.Lock()
	defer procCache.mu.Unlock()
	if !procCache.at.IsZero() && now.Sub(procCache.at) < procCacheTTL {
		return procCache.threads, procCache.tasks, procCache.swapU, procCache.swapT, procCache.hasSwap
	}
	procCache.threads = readThreads()
	procCache.tasks = readCgroupPidsMax()
	procCache.swapU, procCache.swapT, procCache.hasSwap = readSwap()
	procCache.at = now
	return procCache.threads, procCache.tasks, procCache.swapU, procCache.swapT, procCache.hasSwap
}

// readThreads reads the thread count from /proc/self/status.
func readThreads() uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "Threads:" {
			if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				return v
			}
		}
	}
	return 0
}

// readCgroupPidsMax reads the cgroup pids.max for the current process.
func readCgroupPidsMax() uint64 {
	data, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		// v2 lines look like "0::/user.slice/user-1000.slice/session-1.scope"
		// v1 lines carry the hierarchy id; only cgroup v2 has a matching
		// pids controller file in the common layout.
		cgroupPath := strings.TrimPrefix(fields[2], "0=")
		if cgroupPath == "" {
			continue
		}
		for _, root := range []string{"/sys/fs/cgroup", "/sys/fs/cgroup/pids"} {
			p := root + cgroupPath + "/pids.max"
			if raw, err := os.ReadFile(p); err == nil {
				if v, err := strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64); err == nil {
					return v
				}
			}
		}
	}
	return 0
}

// readSwap reads swap total and used from /proc/meminfo. SwapFree is absent on
// some kernels (no swap); then used stays zero and the caller draws no gauge.
func readSwap() (used, total uint64, ok bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0, false
	}
	var free uint64
	var haveFree, haveTotal bool
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "SwapTotal:":
			if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				total, haveTotal = v*1024, true
			}
		case "SwapFree:":
			if v, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				free, haveFree = v*1024, true
			}
		}
	}
	if !haveTotal || !haveFree {
		return 0, 0, false
	}
	used = 0
	if total > free {
		used = total - free
	}
	return used, total, true
}
