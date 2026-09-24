package urnettools

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// The runtime views `urnet-tools top` draws in its Internals panel. They mirror
// the provider's replies to the "internals" and "goroutines" commands.

// NodeInternals is the provider's runtime read. Counters are cumulative; the
// pause and latency quantiles and the GC cpu share cover the interval since the
// provider's previous read.
type NodeInternals struct {
	AtUnixNano int64 `json:"at_unix_nano"`

	Goroutines uint64 `json:"goroutines"`

	HeapObjectsBytes uint64 `json:"heap_objects_bytes"`
	HeapStacksBytes  uint64 `json:"heap_stacks_bytes"`
	HeapGoalBytes    uint64 `json:"heap_goal_bytes"`
	GOGC             int64  `json:"gogc"`

	GCCycles   uint64 `json:"gc_cycles"`
	AllocBytes uint64 `json:"alloc_bytes"`

	IntervalSeconds float64 `json:"interval_seconds"`
	GCPauseP99Ms    float64 `json:"gc_pause_p99_ms"`
	SchedLatP99Ms   float64 `json:"sched_latency_p99_ms"`
	GCCPUFraction   float64 `json:"gc_cpu_fraction"`
}

// GoroutineGroup is the goroutines parked in one place.
type GoroutineGroup struct {
	Func  string `json:"func"`
	Count int    `json:"count"`
}

// GoroutineGroups is the provider's goroutine profile grouped by function.
type GoroutineGroups struct {
	AtUnixNano int64            `json:"at_unix_nano"`
	Total      int              `json:"total"`
	Groups     []GoroutineGroup `json:"groups"`
}

// errRuntimeViewUnsupported means the provider predates the runtime commands.
// top then leaves the Internals panel out.
var errRuntimeViewUnsupported = errors.New("provider does not answer the runtime commands")

// runtimeRequest sends one runtime-view command and returns the reply.
func runtimeRequest(p Provider, cmd string) (controlResponse, error) {
	if p.StateDir == "" {
		return controlResponse{}, fmt.Errorf("%w: provider has no state dir", errSnapshotUnavailable)
	}
	resp, err := sendSocketRequest(filepath.Join(p.StateDir, "provider.sock"), controlRequest{Cmd: cmd})
	if err != nil {
		return controlResponse{}, fmt.Errorf("%w: %v", errSnapshotUnavailable, err)
	}
	if !resp.OK {
		if strings.HasPrefix(resp.Error, "unknown command") {
			return controlResponse{}, errRuntimeViewUnsupported
		}
		return controlResponse{}, fmt.Errorf("%w: %s", errSnapshotUnavailable, resp.Error)
	}
	return resp, nil
}

func fetchInternals(p Provider) (*NodeInternals, error) {
	resp, err := runtimeRequest(p, "internals")
	if err != nil {
		return nil, err
	}
	if resp.Internals == nil {
		return nil, fmt.Errorf("%w: provider returned no internals", errSnapshotUnavailable)
	}
	return resp.Internals, nil
}

func fetchGoroutines(p Provider) (*GoroutineGroups, error) {
	resp, err := runtimeRequest(p, "goroutines")
	if err != nil {
		return nil, err
	}
	if resp.Goroutines == nil {
		return nil, fmt.Errorf("%w: provider returned no goroutines", errSnapshotUnavailable)
	}
	return resp.Goroutines, nil
}
