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
func readUsageHistory(stateDir string) []usageSnapshot {
	b, err := os.ReadFile(filepath.Join(stateDir, "usage_history.jsonl"))
	if err != nil {
		return nil
	}
	var snaps []usageSnapshot
	sc := bufio.NewScanner(strings.NewReader(string(b)))
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

// usageLifetime returns the lifetime (across-restart) usage: because rx/tx are
// cumulative per process and reset on restart, the lifetime total is the
// running maximum of Total()/Billable() over the file. A single-snapshot file
// (or no file) still reports the latest/live sums.
func usageLifetime(snaps []usageSnapshot) usageAggregates {
	var a usageAggregates
	for _, s := range snaps {
		if s.RX+s.TX > a.TotalRX+a.TotalTX {
			a.TotalRX, a.TotalTX = s.RX, s.TX
		}
		if s.BillableRX+s.BillableTX > a.BillableRX+a.BillableTX {
			a.BillableRX, a.BillableTX = s.BillableRX, s.BillableTX
		}
	}
	return a
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
	// A restart is detected when the total cumulative drops between
	// adjacent snapshots (newer < older means counters reset).
	var result usageAggregates
	segBase := usageAggregates{
		TotalRX: base.RX, TotalTX: base.TX,
		BillableRX: base.BillableRX, BillableTX: base.BillableTX,
	}
	for i := 1; i < len(inWindow); i++ {
		cur := inWindow[i]
		prev := inWindow[i-1]
		curTotal := cur.RX + cur.TX
		prevTotal := prev.RX + prev.TX
		if curTotal < prevTotal {
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
