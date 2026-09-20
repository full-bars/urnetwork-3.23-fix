package urnettools

import (
	"errors"
	"fmt"
	"path/filepath"
)

// NodeSnapshot is the client-side mirror of the provider's live status
// snapshot (schema v1, see testdata/livestatus). Fields are only ever added
// by the provider, never renamed or removed, and readers ignore fields they
// do not know. Optional fields are pointers: the provider omits what the
// platform cannot supply, and nil must stay distinguishable from zero.
type NodeSnapshot struct {
	V               int               `json:"v"`
	Version         string            `json:"version"`
	PreviousVersion *string           `json:"previous_version,omitempty"`
	StartedAt       string            `json:"started_at,omitempty"`
	Now             string            `json:"now,omitempty"`
	UptimeSeconds   float64           `json:"uptime_seconds"`
	State           string            `json:"state"` // starting, degraded, idle, flowing
	Busy            bool              `json:"busy"`
	RestartPending  bool              `json:"restart_pending"`
	Rate            SnapshotRate      `json:"rate"`
	Clients         int               `json:"clients"`
	Sessions        SnapshotSessions  `json:"sessions"`
	Proxies         SnapshotProxies   `json:"proxies"`
	Pressure        float64           `json:"pressure"`
	Restart         SnapshotRestart   `json:"restart"`
	Resources       SnapshotResources `json:"resources"`
	IdleHint        *string           `json:"idle_hint,omitempty"`
}

// SnapshotRate carries bytes per second. Floats decode integers too, so a
// provider that later reports fractional rates does not break the reader.
type SnapshotRate struct {
	NowBps                 float64   `json:"now_bps"`
	Avg1mBps               float64   `json:"avg_1m_bps"`
	Avg5mBps               float64   `json:"avg_5m_bps"`
	HistoryIntervalSeconds float64   `json:"history_interval_seconds"`
	HistoryBps             []float64 `json:"history_bps"` // oldest first, newest last
}

type SnapshotSessions struct {
	PQE       int `json:"pqe"`
	Classical int `json:"classical"`
}

type SnapshotProxies struct {
	Up         int `json:"up"`
	Degraded   int `json:"degraded"`
	Connecting int `json:"connecting"`
	Dead       int `json:"dead"`
}

type SnapshotRestart struct {
	Reason        string `json:"reason"` // update, hotswap, manual, clean, unclean, first-start
	CleanShutdown bool   `json:"clean_shutdown"`
}

type SnapshotResources struct {
	HeapInuseBytes uint64  `json:"heap_inuse_bytes"`
	MemLimitBytes  *uint64 `json:"mem_limit_bytes,omitempty"`
	RSSBytes       *uint64 `json:"rss_bytes,omitempty"`
	Goroutines     int     `json:"goroutines"`
	OpenFDs        *int    `json:"open_fds,omitempty"`
	FDLimit        *int    `json:"fd_limit,omitempty"`
}

// errSnapshotUnavailable means no snapshot could be had: the provider is not
// running, its socket is unreachable, or it predates the "snapshot" command.
// Callers skip the live block on it; only --json turns it into an error.
var errSnapshotUnavailable = errors.New("live status unavailable")

// fetchSnapshot asks the provider for its live snapshot over the control
// socket. Every failure to obtain one wraps errSnapshotUnavailable so a
// caller can skip the live block with a single errors.Is check.
func fetchSnapshot(p Provider) (*NodeSnapshot, error) {
	if p.StateDir == "" {
		return nil, fmt.Errorf("%w: provider has no state dir", errSnapshotUnavailable)
	}
	sockPath := filepath.Join(p.StateDir, "provider.sock")
	resp, err := sendSocketRequest(sockPath, controlRequest{Cmd: "snapshot"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSnapshotUnavailable, err)
	}
	if !resp.OK || resp.Snapshot == nil {
		reason := resp.Error
		if reason == "" {
			reason = "provider returned no snapshot"
		}
		return nil, fmt.Errorf("%w: %s", errSnapshotUnavailable, reason)
	}
	return resp.Snapshot, nil
}
