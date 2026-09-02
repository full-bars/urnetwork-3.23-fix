package urnettools

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// usageSnapshot is one row of the provider's persistent usage history
// (usage_history.jsonl). rx/tx are cumulative TOTAL bytes across all proxies
// since process start; billable_rx/tx are the cumulative billable share.
type usageSnapshot struct {
	TS         time.Time `json:"ts"`
	RX         uint64    `json:"rx"`
	TX         uint64    `json:"tx"`
	BillableRX uint64    `json:"billable_rx"`
	BillableTX uint64    `json:"billable_tx"`
}

// usageAggregates sums snapshots over a window.
type usageAggregates struct {
	BillableRX uint64
	BillableTX uint64
	TotalRX    uint64
	TotalTX    uint64
}

func (a usageAggregates) Billable() uint64 { return a.BillableRX + a.BillableTX }
func (a usageAggregates) Total() uint64    { return a.TotalRX + a.TotalTX }
func (a usageAggregates) Control() uint64 {
	if a.Total() > a.Billable() {
		return a.Total() - a.Billable()
	}
	return 0
}

// readUsageHistory parses the provider's usage_history.jsonl into snapshots,
// oldest-first, skipping malformed lines. A missing file yields an empty slice.
// Streams from the file handle (no os.ReadFile + string copy) so a large
// capped history file does not peak at ~2x its size in memory (CR Major).
func readUsageHistory(stateDir string) []usageSnapshot {
	f, err := os.Open(filepath.Join(stateDir, "usage_history.jsonl"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var snaps []usageSnapshot
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var s usageSnapshot
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			continue // ragged/partial last line — skip
		}
		snaps = append(snaps, s)
	}
	return snaps
}

// usageLifetime returns the lifetime (across-restart) usage. Segment-summed
// instead of a running max: the provider's reported totals are the SUM of
// currently-registered proxies' bandwidth (proxy_health_log.go), so a proxy
// exiting (trim, hot-reload, reaper eviction) makes the cumulative total DROP
// — not just on a process restart. A running max of Total() silently discards
// all post-drop growth that never re-exceeds the pre-drop peak. Segment-sum
// recovers it: the first segment's baseline is ZERO (bytes since the first
// recorded snapshot); whenever any field drops we flush the prior segment's
// contribution and restart the baseline at the post-drop snapshot.
func usageLifetime(snaps []usageSnapshot) usageAggregates {
	var result usageAggregates
	if len(snaps) == 0 {
		return result
	}
	segBase := usageAggregates{} // baseline for the first segment = 0
	for i := 1; i < len(snaps); i++ {
		cur, prev := snaps[i], snaps[i-1]
		if cur.RX < prev.RX || cur.TX < prev.TX ||
			cur.BillableRX < prev.BillableRX || cur.BillableTX < prev.BillableTX {
			// Drop: the previous segment ran segBase -> prev. Flush it.
			result.TotalRX += satSub(prev.RX, segBase.TotalRX)
			result.TotalTX += satSub(prev.TX, segBase.TotalTX)
			result.BillableRX += satSub(prev.BillableRX, segBase.BillableRX)
			result.BillableTX += satSub(prev.BillableTX, segBase.BillableTX)
			// New baseline = the post-drop snapshot.
			segBase = usageAggregates{
				TotalRX: cur.RX, TotalTX: cur.TX,
				BillableRX: cur.BillableRX, BillableTX: cur.BillableTX,
			}
		}
	}
	// Final segment: segBase -> last.
	last := snaps[len(snaps)-1]
	result.TotalRX += satSub(last.RX, segBase.TotalRX)
	result.TotalTX += satSub(last.TX, segBase.TotalTX)
	result.BillableRX += satSub(last.BillableRX, segBase.BillableRX)
	result.BillableTX += satSub(last.BillableTX, segBase.BillableTX)
	return result
}

// Since snapshots are cumulative-per-process, "since X" (e.g. last 24h) is not
// a simple sum — delta between the newest snapshot and the one before the
// window start gives the bytes moved in the window. However, if a provider
// restart occurred between the base and reference snapshots, the cumulative
// counters reset toward zero and a naive two-point delta undercounts (or
// returns 0 via satSub). usageWindow walks all snapshots in the window,
// detects restart boundaries (cumulative drops), and sums each segment
// independently to handle restarts correctly.
func usageWindow(snaps []usageSnapshot, window time.Duration, now time.Time) usageAggregates {
	if len(snaps) == 0 {
		return usageAggregates{}
	}
	// Collect snapshots within the window (including one before the window
	// for the base reference).
	var inWindow []usageSnapshot
	var base *usageSnapshot
	for i := len(snaps) - 1; i >= 0; i-- {
		if snaps[i].TS.Before(now.Add(-window)) {
			base = &snaps[i]
			break
		}
	}
	if base == nil {
		// Window predates all history — treat history start as base (0).
		base = &usageSnapshot{}
	}
	// Collect snapshots from base onward (inclusive).
	for i := 0; i < len(snaps); i++ {
		if !snaps[i].TS.Before(base.TS) {
			inWindow = snaps[i:]
			break
		}
	}
	if len(inWindow) == 0 {
		return usageAggregates{}
	}

	// Walk the snapshots and sum deltas, resetting on restart boundaries.
	// A boundary is detected when ANY cumulative field drops between adjacent
	// snapshots (newer < older means a counter reset, or a proxy's bytes being
	// removed from the aggregate), not just when the combined RX+TX sum drops
	// — an asymmetric dip (e.g. heavy-RX proxy removed while TX keeps growing)
	// can keep the combined sum rising while a real RX field dropped, and a
	// combined-sum-only check would miss it and silently discard those bytes.
	var result usageAggregates
	segBase := usageAggregates{
		TotalRX: base.RX, TotalTX: base.TX,
		BillableRX: base.BillableRX, BillableTX: base.BillableTX,
	}
	for i := 1; i < len(inWindow); i++ {
		cur := inWindow[i]
		prev := inWindow[i-1]
		if cur.RX < prev.RX || cur.TX < prev.TX ||
			cur.BillableRX < prev.BillableRX || cur.BillableTX < prev.BillableTX {
			// Restart detected: add the contribution of the previous segment
			// (its max was prevTotal, since cumulative only grows within a process).
			result.TotalRX += satSub(prev.RX, segBase.TotalRX)
			result.TotalTX += satSub(prev.TX, segBase.TotalTX)
			result.BillableRX += satSub(prev.BillableRX, segBase.BillableRX)
			result.BillableTX += satSub(prev.BillableTX, segBase.BillableTX)
			// Reset segment base to the post-restart snapshot.
			segBase = usageAggregates{
				TotalRX: cur.RX, TotalTX: cur.TX,
				BillableRX: cur.BillableRX, BillableTX: cur.BillableTX,
			}
		}
	}
	// Add the final segment's contribution (from segBase to the last snapshot).
	last := inWindow[len(inWindow)-1]
	result.TotalRX += satSub(last.RX, segBase.TotalRX)
	result.TotalTX += satSub(last.TX, segBase.TotalTX)
	result.BillableRX += satSub(last.BillableRX, segBase.BillableRX)
	result.BillableTX += satSub(last.BillableTX, segBase.BillableTX)
	return result
}

func satSub(a, b uint64) uint64 {
	if a >= b {
		return a - b
	}
	return 0
}
