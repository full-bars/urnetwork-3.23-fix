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
// window start gives the bytes moved in the window. usageWindow returns that
// delta-based window aggregate.
func usageWindow(snaps []usageSnapshot, window time.Duration, now time.Time) usageAggregates {
	if len(snaps) == 0 {
		return usageAggregates{}
	}
	// Reference snapshot: the newest one (live values).
	ref := snaps[len(snaps)-1]
	// Find the newest snapshot older than the window start.
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
	return usageAggregates{
		BillableRX: satSub(ref.BillableRX, base.BillableRX),
		BillableTX: satSub(ref.BillableTX, base.BillableTX),
		TotalRX:    satSub(ref.RX, base.RX),
		TotalTX:    satSub(ref.TX, base.TX),
	}
}

func satSub(a, b uint64) uint64 {
	if a >= b {
		return a - b
	}
	return 0
}
