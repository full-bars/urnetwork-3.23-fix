package urnettools

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
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

	// Transport is what each platform transport mode has carried. A provider
	// that never attempted H3 reports none, and top draws no transport rows.
	Transport *NodeTransportStats `json:"transport,omitempty"`
}

// NodeTransportStats is the provider's per-mode transport read: payload frames
// and bytes per mode and direction, the H3 connection counters and H3's share of
// outbound frames. Counters are cumulative since the provider started.
type NodeTransportStats struct {
	H1FramesTx uint64 `json:"h1_frames_tx"`
	H1FramesRx uint64 `json:"h1_frames_rx"`
	H1BytesTx  uint64 `json:"h1_bytes_tx"`
	H1BytesRx  uint64 `json:"h1_bytes_rx"`
	H3FramesTx uint64 `json:"h3_frames_tx"`
	H3FramesRx uint64 `json:"h3_frames_rx"`
	H3BytesTx  uint64 `json:"h3_bytes_tx"`
	H3BytesRx  uint64 `json:"h3_bytes_rx"`

	H3Attempts        uint64 `json:"h3_attempts"`
	H3Connects        uint64 `json:"h3_connects"`
	H3ConnectFailures uint64 `json:"h3_connect_failures"`
	H3Drops           uint64 `json:"h3_drops"`
	H3Up              int64  `json:"h3_up"`

	// H1Direct is H1 for the identities eligible for H3 alone: the denominator
	// the provider measures H3's share against.
	H1DirectFramesTx uint64 `json:"h1_direct_frames_tx"`
	H1DirectFramesRx uint64 `json:"h1_direct_frames_rx"`
	H1DirectBytesTx  uint64 `json:"h1_direct_bytes_tx"`
	H1DirectBytesRx  uint64 `json:"h1_direct_bytes_rx"`

	// Dns and DnsPump are the WhoDis DNS and DNS-pump modes, absent until the
	// provider has attempted one of them.
	Dns     *NodePtModeStats `json:"dns,omitempty"`
	DnsPump *NodePtModeStats `json:"dns_pump,omitempty"`

	// H3TxSharePercent is H3's share of the direct identity's outbound payload
	// frames, or -1 when neither mode has sent one.
	H3TxSharePercent int `json:"h3_tx_share_percent"`
}

// NodePtModeStats is one packet-translation mode's counters, mirroring the
// provider's block of the same name.
type NodePtModeStats struct {
	FramesTx        uint64 `json:"frames_tx"`
	FramesRx        uint64 `json:"frames_rx"`
	BytesTx         uint64 `json:"bytes_tx"`
	BytesRx         uint64 `json:"bytes_rx"`
	Up              int64  `json:"up"`
	Attempts        uint64 `json:"attempts"`
	Connects        uint64 `json:"connects"`
	ConnectFailures uint64 `json:"connect_failures"`
	Drops           uint64 `json:"drops"`
}

// TxBytesTotal is every transport mode's outbound payload: the denominator the
// top panel shows H3's own outbound against. It lives with the struct so adding
// a mode is a change in one place rather than in the view.
func (t *NodeTransportStats) TxBytesTotal() uint64 {
	total := t.H1BytesTx + t.H3BytesTx
	if t.Dns != nil {
		total += t.Dns.BytesTx
	}
	if t.DnsPump != nil {
		total += t.DnsPump.BytesTx
	}
	return total
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

// runtimeRequestTimeout is how long a runtime command may take. internals is a
// cheap read and gets the light deadline; the goroutine profile stops the world
// briefly on the provider and is allowed the full one.
func runtimeRequestTimeout(cmd string) time.Duration {
	if cmd == "internals" {
		return topLightTimeout
	}
	return 5 * time.Second
}

// runtimeRequest sends one runtime-view command and returns the reply.
func runtimeRequest(p Provider, cmd string) (controlResponse, error) {
	if p.StateDir == "" {
		return controlResponse{}, fmt.Errorf("%w: provider has no state dir", errSnapshotUnavailable)
	}
	resp, err := sendSocketRequestTimeout(filepath.Join(p.StateDir, "provider.sock"), controlRequest{Cmd: cmd}, runtimeRequestTimeout(cmd))
	if err != nil {
		return controlResponse{}, fmt.Errorf("%w: %w", errSnapshotUnavailable, err)
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
