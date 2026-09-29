package main

import (
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/urnetwork/connect"
)

// resource_config_warn.go compares the memory limits a provider was started with
// against an estimate of what its proxy pool needs, and reports a mismatch once
// at startup. Two 1 GB boxes ran 1,300 proxies under a hand-pinned
// GOMEMLIMIT=400MiB and a 450M cgroup MemoryHigh: the live heap sat above its own
// soft limit and nothing said so. The provider never changes these values (they
// are the operator's, or systemd's); it only makes the mismatch visible.
//
// Two separate checks, because the two numbers answer different questions:
//
//   - GOMEMLIMIT is a HEAP limit. It is compared against the Go heap the pool
//     needs, never against the whole-process footprint.
//   - The box's RAM is compared against the whole-process footprint, because
//     that is what the kernel has to hold: a long-running proxy also carries its
//     connections, buffers, goroutine stacks and the clients routed through it.
//
// Measuring one number and holding it against both limits was the bug this
// file previously had: it warned on the fleet's top earner, which was healthy,
// and stayed silent on a box thrashing its memory at pressure 1.0.

const (
	// resourceBaseOverhead is the fixed cost of a provider with no proxies
	// (control socket, metrics, stores, pools).
	resourceBaseOverhead = 100 << 20
	// resourceHeapBytesPerProxy is the Go heap one long-running proxy carries.
	// Sampled from the running fleet after days of uptime, not fresh restarts:
	// heap per proxy ran from about 0.4 to 1.2 MiB with a median near 0.65 and
	// the upper quartile near 1. It already includes the average share of the
	// connected clients, so no separate client allowance is added on top.
	resourceHeapBytesPerProxy = 665600
	// resourceFootprintBytesPerProxy is the whole-process footprint (RSS plus
	// swap) one long-running proxy carries: a median near 1.03 MiB across the
	// main fleet and 1.15 on the Taco fleet. 0.9 MiB keeps the estimate under
	// the measured median while still clearing the reserve on healthy boxes.
	resourceFootprintBytesPerProxy = 943718
	// resourceRAMReserveFloor is the smallest slice of RAM held back for the OS
	// and co-tenants; resourceRAMReservePercent is the share of RAM held back
	// on a box large enough for the percentage to matter.
	resourceRAMReserveFloor     = 300 << 20
	resourceRAMReservePercent   = 10
	resourceRAMReservePercentOf = 100
)

// resourceConfigInput is everything the check needs; zero means "not set".
type resourceConfigInput struct {
	Proxies int // proxies this provider will launch
	// GoMemLimit is a finite GOMEMLIMIT in bytes, 0 = none.
	GoMemLimit int64
	// EffectiveRAMBytes is the RAM ceiling the provider tunes itself against
	// (connect.EffectiveRAMLimit): the tightest cgroup memory.max/memory.high
	// on the unit or an ancestor, or the host's total RAM when nothing limits
	// it. 0 = unknown.
	EffectiveRAMBytes int64
	GOGCEnv           bool // GOGC is set in the environment
	AutoProfile       bool // URNETWORK_PROFILE=auto
}

// estimatedPoolHeap is the Go heap a pool of n proxies is expected to need. It
// is what GOMEMLIMIT is compared against, and only that.
func estimatedPoolHeap(n int) int64 {
	if n < 0 {
		n = 0
	}
	return resourceBaseOverhead + int64(n)*resourceHeapBytesPerProxy
}

// estimatedPoolFootprint is the whole-process footprint a pool of n proxies is
// expected to need. It is what the box's RAM is compared against, and only
// that. Note it includes swap while a cgroup memory.max does not, so a
// cgroup-limited box is the least validated case of this check: no sampled
// node had a MemoryMax, so no sample pins that path.
func estimatedPoolFootprint(n int) int64 {
	if n < 0 {
		n = 0
	}
	return resourceBaseOverhead + int64(n)*resourceFootprintBytesPerProxy
}

// ramReserve is what the provider leaves for the OS and every other tenant on
// the box: a floor on small boxes, a share of RAM on large ones. Never more
// than the box actually has, so a provider in a container capped below the
// floor reserves what it has rather than reporting a negative remainder.
func ramReserve(ram int64) int64 {
	if ram <= 0 {
		return 0
	}
	byPercent := ram * resourceRAMReservePercent / resourceRAMReservePercentOf
	if byPercent > resourceRAMReserveFloor {
		return byPercent
	}
	if resourceRAMReserveFloor > ram {
		return ram
	}
	return resourceRAMReserveFloor
}

// resourceConfigWarnings returns one human-readable warning per problem, in a
// stable order: heap against the soft memory limit, footprint against the box's
// RAM, pinned GOGC.
func resourceConfigWarnings(in resourceConfigInput) []string {
	var out []string
	if in.GoMemLimit > 0 {
		need := estimatedPoolHeap(in.Proxies)
		if in.GoMemLimit < need {
			// Never advise raising the limit above what the box has. When the
			// box RAM is unknown, name no number rather than say "0 MiB".
			raise := fmt.Sprintf("Raise it, but never above what this box actually has, or lower the pool (`urnet-tools proxy trim <count>`)")
			if ram := ramCeilingMiB(in); ram > 0 {
				raise = fmt.Sprintf("Raise it towards the %d MiB this box actually has (edit GOMEMLIMIT in the systemd unit, or `urnet-tools set gomemlimit` if the unit does not set it), or lower the pool (`urnet-tools proxy trim <count>`)", ram)
			}
			out = append(out, fmt.Sprintf(
				"GOMEMLIMIT=%d MiB is below the ~%d MiB of heap this pool of %d proxies is expected to need, so the runtime will spend its CPU on GC. %s",
				in.GoMemLimit>>20, need>>20, in.Proxies, raise))
		}
	}
	if in.EffectiveRAMBytes > 0 {
		need := estimatedPoolFootprint(in.Proxies)
		avail := in.EffectiveRAMBytes - ramReserve(in.EffectiveRAMBytes)
		if need > avail {
			out = append(out, fmt.Sprintf(
				"this pool of %d proxies is expected to need about %d MiB of memory but this box gives the provider about %d MiB (%d MiB left after holding back %d MiB for the OS and other tenants), so expect swapping and stalls. Lower the pool (`urnet-tools proxy trim <count>`) or move the provider to a larger box",
				in.Proxies, need>>20, in.EffectiveRAMBytes>>20, avail>>20, ramReserve(in.EffectiveRAMBytes)>>20))
		}
	}
	if in.GOGCEnv && in.AutoProfile {
		out = append(out, "GOGC is pinned in the environment, which disables the adaptive GC governor under URNETWORK_PROFILE=auto; unset GOGC in the unit to let it adapt")
	}
	return out
}

// ramCeilingMiB is the box's own RAM ceiling, or 0 when unknown, for wording.
func ramCeilingMiB(in resourceConfigInput) int64 {
	return in.EffectiveRAMBytes >> 20
}

// resourceConfigInputFrom builds the warning input from values the caller
// already holds, so the check can be exercised without a real cgroup, a real
// process limit, or a start.
func resourceConfigInputFrom(proxies int, goMemLimit, effectiveRAM int64) resourceConfigInput {
	return resourceConfigInput{
		Proxies:           proxies,
		GoMemLimit:        goMemLimit,
		EffectiveRAMBytes: effectiveRAM,
		GOGCEnv:           os.Getenv("GOGC") != "",
		AutoProfile:       os.Getenv("URNETWORK_PROFILE") == "auto",
	}
}

// resourceConfigLaunchCount is the pool size this start is about to launch.
// It is stored before the first launch goroutine starts because the warning
// itself runs from inside the first of them (see resourceConfigWarningsOnce).
var resourceConfigLaunchCount atomic.Int64

// resourceConfigWarnOnce makes the startup warning fire exactly once, from the
// first proxy goroutine, AFTER the tier/profile memory limits are in place.
// Read at the old call site it saw no limit at all on every auto, eco, turbo
// and default node, because the tier code sets GOMEMLIMIT after that point.
var resourceConfigWarnOnce sync.Once

// resourceConfigGather reads the live values the startup warning reports. It is
// a variable so a test can pin them: the real one reads the process's own
// limits, which no test can set without changing the whole test binary's
// runtime.
var resourceConfigGather = resourceConfigInputCurrent

// resourceConfigInputCurrent is the real gatherer: the pool size this start is
// about to launch, the soft memory limit in force right now (only meaningful
// once the tier and profile limits have been applied), and the box's own RAM
// ceiling.
func resourceConfigInputCurrent() resourceConfigInput {
	var limit int64
	if l := debug.SetMemoryLimit(-1); l > 0 && l != math.MaxInt64 {
		limit = l
	}
	ram, _ := connect.EffectiveRAMLimit()
	return resourceConfigInputFrom(int(resourceConfigLaunchCount.Load()), limit, ram)
}

// resourceConfigWarningsOnce says once, at startup, when the limits this
// process runs under are short for the pool it is about to launch. The soft
// memory limit is read here, after ensureMemoryLimit has run, so what it
// reports is the limit actually in force.
func resourceConfigWarningsOnce() {
	resourceConfigWarnOnce.Do(func() {
		for _, w := range resourceConfigWarnings(resourceConfigGather()) {
			importantLogf("[proxy][resources] warning: %s\n", w)
		}
	})
}

// ramCeilingLogLine is the one line, logged once at startup, that says what RAM
// ceiling this process tunes itself against (tier selection, GOMEMLIMIT, the GC
// governor, the headroom threshold, the hot-swap gate) and which limit set it.
// The number is derived from the process's own cgroup and can differ from the
// host's RAM by a lot, so without this line a box that suddenly tunes for a
// smaller ceiling gives the operator nothing to explain why.
func ramCeilingLogLine(ceilingBytes int64, source string) string {
	return fmt.Sprintf("[proxy][resources] effective RAM ceiling %d MiB (%s)", ceilingBytes>>20, source)
}
