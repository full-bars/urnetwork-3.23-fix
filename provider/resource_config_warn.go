package main

import (
	"fmt"
)

// resource_config_warn.go compares the memory limits a provider was started with
// against a conservative estimate of what its proxy pool needs, and reports a
// mismatch once at startup. Two 1 GB boxes ran 1,300 proxies under a
// hand-pinned GOMEMLIMIT=400MiB and a 450M cgroup MemoryHigh: the live heap sat
// above its own soft limit and nothing said so. The provider never changes these
// values (they are the operator's, or systemd's); it only makes the mismatch
// visible.

const (
	// resourceBaseOverhead is the fixed heap of a provider with no proxies
	// (control socket, metrics, stores, pools).
	resourceBaseOverhead = 100 << 20
	// resourceBytesPerProxy is calibrated to the high side of the measured
	// fleet, which spans roughly 0.2-0.55 MiB of heap per proxy.
	resourceBytesPerProxy = 256 << 10
	// Connected clients are unknown at startup but drive memory (about 0.8 MiB
	// heap each). A 2 GiB box reached ~540 clients on 2,001 proxies (0.27 per proxy) and
	// its heap went 415 -> 791 MiB, so reserve a peak-load allowance of 0.15
	// clients per proxy, which is what the estimate adds on top of the proxies.
	resourceClientsPerProxy = 0.15
	resourceBytesPerClient  = 800 << 10
	// resourceRSSFactor scales a heap estimate to what a cgroup ceiling must
	// hold, since RSS also carries stacks and runtime overhead.
	resourceRSSFactor = 1.25
)

// resourceConfigInput is everything the check needs; zero means "not set".
type resourceConfigInput struct {
	Proxies       int   // proxies this provider will launch
	GoMemLimit    int64 // finite GOMEMLIMIT in bytes, 0 = none
	CgroupCeiling int64 // tightest cgroup memory.max/memory.high in bytes, 0 = none
	GOGCEnv       bool  // GOGC is set in the environment
	AutoProfile   bool  // URNETWORK_PROFILE=auto
}

// estimatedPoolMemory is the heap a pool of n proxies is expected to need at
// peak client load.
func estimatedPoolMemory(n int) int64 {
	if n < 0 {
		n = 0
	}
	perProxy := float64(resourceBytesPerProxy) + resourceClientsPerProxy*float64(resourceBytesPerClient)
	return resourceBaseOverhead + int64(float64(n)*perProxy)
}

// resourceConfigWarnings returns one human-readable warning per problem, in a
// stable order: soft memory limit, cgroup ceiling, pinned GOGC.
func resourceConfigWarnings(in resourceConfigInput) []string {
	var out []string
	need := estimatedPoolMemory(in.Proxies)
	if in.GoMemLimit > 0 && in.GoMemLimit < need {
		out = append(out, fmt.Sprintf(
			"GOMEMLIMIT=%d MiB is below the ~%d MiB this pool of %d proxies is expected to need, so the runtime will spend its CPU on GC. Raise it (edit GOMEMLIMIT in the systemd unit, or `urnet-tools set gomemlimit` if the unit does not set it) or lower the pool (`urnet-tools proxy trim`)",
			in.GoMemLimit>>20, need>>20, in.Proxies))
	}
	if in.CgroupCeiling > 0 {
		rss := int64(float64(need) * resourceRSSFactor)
		if in.CgroupCeiling < rss {
			out = append(out, fmt.Sprintf(
				"cgroup memory ceiling %d MiB (MemoryMax/MemoryHigh) is below the ~%d MiB this pool of %d proxies is expected to need, so the kernel will throttle and swap it. Raise the unit's MemoryHigh/MemoryMax or lower the pool (`urnet-tools proxy trim`)",
				in.CgroupCeiling>>20, rss>>20, in.Proxies))
		}
	}
	if in.GOGCEnv && in.AutoProfile {
		out = append(out, "GOGC is pinned in the environment, which disables the adaptive GC governor under URNETWORK_PROFILE=auto; unset GOGC in the unit to let it adapt")
	}
	return out
}
