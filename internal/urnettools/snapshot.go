package urnettools

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/term"
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
	StateReason     *string           `json:"state_reason,omitempty"`
	// Traffic is billable versus total bytes. Nil from a provider that
	// predates it.
	Traffic *SnapshotTraffic `json:"traffic,omitempty"`
}

// SnapshotTraffic mirrors the provider's traffic block: session byte totals
// and the total-traffic rate that sits beside the billable Rate.
type SnapshotTraffic struct {
	BillableBytes         uint64    `json:"billable_bytes"`
	TotalBytes            uint64    `json:"total_bytes"`
	LifetimeBillableBytes *uint64   `json:"lifetime_billable_bytes,omitempty"`
	TotalNowBps           float64   `json:"total_now_bps"`
	TotalAvg1mBps         float64   `json:"total_avg_1m_bps"`
	TotalAvg5mBps         float64   `json:"total_avg_5m_bps"`
	TotalHistoryBps       []float64 `json:"total_history_bps"` // oldest first, newest last
}

// LiveTraffic is the light reply to the "traffic" command: live counter sums
// and the provider's clock when they were read. top polls it at up to 100ms and
// derives rates from the deltas. The sums drop when a proxy is removed or
// respawns, so a decrease is skipped, not read as negative traffic.
type LiveTraffic struct {
	AtUnixNano    int64  `json:"at_unix_nano"`
	BillableBytes uint64 `json:"billable_bytes"`
	TotalBytes    uint64 `json:"total_bytes"`
}

// errTrafficUnsupported means the provider predates the "traffic" command. top
// then falls back to the snapshot's own once-a-second rates.
var errTrafficUnsupported = errors.New("provider does not answer the traffic command")

// fetchLiveTraffic asks the provider for its live counters over the control
// socket.
func fetchLiveTraffic(p Provider) (*LiveTraffic, error) {
	if p.StateDir == "" {
		return nil, fmt.Errorf("%w: provider has no state dir", errSnapshotUnavailable)
	}
	resp, err := sendSocketRequest(filepath.Join(p.StateDir, "provider.sock"), controlRequest{Cmd: "traffic"})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errSnapshotUnavailable, err)
	}
	if !resp.OK {
		if strings.HasPrefix(resp.Error, "unknown command") {
			return nil, errTrafficUnsupported
		}
		return nil, fmt.Errorf("%w: %s", errSnapshotUnavailable, resp.Error)
	}
	if resp.Traffic == nil {
		return nil, fmt.Errorf("%w: provider returned no traffic", errSnapshotUnavailable)
	}
	return resp.Traffic, nil
}

// whyRow returns the label and text of the "why" line for the node's current
// state: the idle hint when idle, the state reason when starting or degraded.
// ok is false when there is nothing to say (flowing, or no text).
func (s *NodeSnapshot) whyRow() (label, text string, ok bool) {
	switch s.State {
	case "idle":
		if s.IdleHint != nil && *s.IdleHint != "" {
			return "why idle", *s.IdleHint, true
		}
	case "starting", "degraded":
		if s.StateReason != nil && *s.StateReason != "" {
			return "why " + s.State, *s.StateReason, true
		}
	}
	return "", "", false
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
	snap, _, err := fetchSnapshotRaw(p)
	return snap, err
}

// fetchSnapshotRaw is fetchSnapshot that also returns the snapshot object
// exactly as the provider sent it, so --json can print fields this build of
// the tool does not know about.
func fetchSnapshotRaw(p Provider) (*NodeSnapshot, json.RawMessage, error) {
	if p.StateDir == "" {
		return nil, nil, fmt.Errorf("%w: provider has no state dir", errSnapshotUnavailable)
	}
	sockPath := filepath.Join(p.StateDir, "provider.sock")
	resp, err := sendSocketRequest(sockPath, controlRequest{Cmd: "snapshot"})
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", errSnapshotUnavailable, err)
	}
	if !resp.OK || resp.Snapshot == nil {
		reason := resp.Error
		if reason == "" {
			reason = "provider returned no snapshot"
		}
		return nil, nil, fmt.Errorf("%w: %s", errSnapshotUnavailable, reason)
	}
	var raw struct {
		Snapshot json.RawMessage `json:"snapshot"`
	}
	if err := json.Unmarshal(resp.Raw, &raw); err != nil {
		return nil, nil, fmt.Errorf("%w: %v", errSnapshotUnavailable, err)
	}
	return resp.Snapshot, raw.Snapshot, nil
}

// printSnapshotJSON prints the provider's raw snapshot for scripts. When no
// snapshot is available it says why and returns an error (non-zero exit).
func printSnapshotJSON(p Provider) error {
	_, raw, err := fetchSnapshotFn(p)
	if err != nil {
		return fmt.Errorf("no live status from %s: %w (is the provider running and new enough to answer \"snapshot\"?)", providerLabel(p), err)
	}
	var out bytes.Buffer
	if err := json.Indent(&out, raw, "", "  "); err != nil {
		return fmt.Errorf("live status from %s is not valid JSON: %w", providerLabel(p), err)
	}
	out.WriteByte('\n')
	_, err = os.Stdout.Write(out.Bytes())
	return err
}

// statusLiveOpts reads the drawing options for stdout.
func statusLiveOpts() liveOpts {
	fd := int(os.Stdout.Fd())
	isTTY := term.IsTerminal(fd)
	width := 0
	if isTTY {
		if w, _, err := term.GetSize(fd); err == nil {
			width = w
		}
	}
	return liveOptsFromEnv(isTTY, width)
}

// printLiveBlock appends the live section under the classic status output
// when the provider answers, and prints nothing at all when it does not.
func printLiveBlock(p Provider) {
	snap, _, err := fetchSnapshotFn(p)
	if err != nil {
		return
	}
	fmt.Println()
	fmt.Print(renderLiveBlock(snap, statusLiveOpts()))
}

// printProviderSummary prints one compact row per provider, fetching the
// snapshots concurrently so one slow socket does not stall the rest.
func printProviderSummary(providers []Provider) {
	rows := make([]providerRow, len(providers))
	var wg sync.WaitGroup
	for i, p := range providers {
		rows[i] = providerRow{Name: providerLabel(p), Running: p.Running, Version: p.Version}
		wg.Add(1)
		go func(i int, p Provider) {
			defer wg.Done()
			if snap, _, err := fetchSnapshotFn(p); err == nil {
				rows[i].Snap = snap
			}
		}(i, p)
	}
	wg.Wait()
	fmt.Printf("%d providers found; use a target (--unit / --user / --network / --network-id / --state-dir) for detail:\n\n", len(providers))
	fmt.Print(renderProviderTable(rows, statusLiveOpts()))
}
