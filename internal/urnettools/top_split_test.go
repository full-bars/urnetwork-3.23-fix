package urnettools

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect/internal/tui"
	"github.com/urnetwork/connect/internal/tui/tcellui"
)

func TestTopSplitGraphToggleKey(t *testing.T) {
	clock := &fakeClock{t: topBase}
	dir := t.TempDir()
	path := filepath.Join(dir, "top.conf")
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.settingsPath = path

	if !m.graphSplit {
		t.Fatal("graphSplit should default to true")
	}

	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 's'})
	if m.graphSplit {
		t.Fatal("pressing 's' should toggle graphSplit to false")
	}
	s := loadTopSettings(path)
	if s.GraphSplit {
		t.Fatal("saved settings should have graphSplit false")
	}

	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 'S'})
	if !m.graphSplit {
		t.Fatal("pressing 'S' should toggle graphSplit to true")
	}
	s = loadTopSettings(path)
	if !s.GraphSplit {
		t.Fatal("saved settings should have graphSplit true")
	}
}

func TestTopSplitGraphMenuCycle(t *testing.T) {
	clock := &fakeClock{t: topBase}
	dir := t.TempDir()
	path := filepath.Join(dir, "top.conf")
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)
	m.settingsPath = path

	m.handle(tcellui.Event{Kind: tcellui.EventRune, Rune: 'm'})
	if !m.menu {
		t.Fatal("menu should be open")
	}

	m.menuSel = topMenuTrafficView
	m.menuCycle(1)
	if m.graphSplit {
		t.Fatal("cycling traffic view should set graphSplit to false")
	}
	s := loadTopSettings(path)
	if s.GraphSplit {
		t.Fatal("saved settings should reflect false")
	}

	m.menuCycle(1)
	if !m.graphSplit {
		t.Fatal("cycling traffic view should toggle graphSplit back to true")
	}
	s = loadTopSettings(path)
	if !s.GraphSplit {
		t.Fatal("saved settings should reflect true")
	}
}

func TestTopSplitGraphDrawAndFallback(t *testing.T) {
	clock := &fakeClock{t: topBase}
	m := newTestModel(tui.DefaultTheme(), topProviders(1), clock)

	historyLen := 60
	histBps := make([]float64, historyLen)
	histRx := make([]float64, historyLen)
	histTx := make([]float64, historyLen)
	for i := 0; i < historyLen; i++ {
		histRx[i] = float64(1000 + i*100)
		histTx[i] = float64(500 + i*50)
		histBps[i] = histRx[i] + histTx[i]
	}

	snap := &NodeSnapshot{
		Version: "v3.23.0",
		State:   "flowing",
		Now:     clock.t.Format(time.RFC3339),
		Rate: SnapshotRate{
			NowBps:       histBps[historyLen-1],
			HistorySeq:   1,
			HistoryBps:   histBps,
			HistoryRxBps: histRx,
			HistoryTxBps: histTx,
		},
		Traffic: &SnapshotTraffic{
			TotalNowBps:       histBps[historyLen-1],
			TotalHistoryBps:   histBps,
			TotalHistoryRxBps: histRx,
			TotalHistoryTxBps: histTx,
		},
	}
	m.apply(m.gen, snap, nil)

	b := tui.New(100, 30)
	m.render(b)
	splitStr := b.String()
	if !strings.Contains(splitStr, "Billable") || !strings.Contains(splitStr, "Total traffic") {
		t.Fatalf("split view output missing graphs:\n%s", splitStr)
	}

	// Toggle split off -> combined view
	m.graphSplit = false
	bCombined := tui.New(100, 30)
	m.render(bCombined)
	combStr := bCombined.String()
	if !strings.Contains(combStr, "Billable") || !strings.Contains(combStr, "Total traffic") {
		t.Fatalf("combined view output missing graphs:\n%s", combStr)
	}

	// Fallback when directional history is nil
	m.graphSplit = true
	snapNoDir := &NodeSnapshot{
		Version: "v3.23.0",
		State:   "flowing",
		Now:     clock.t.Format(time.RFC3339),
		Rate: SnapshotRate{
			NowBps:     histBps[historyLen-1],
			HistorySeq: 2,
			HistoryBps: histBps,
		},
		Traffic: &SnapshotTraffic{
			TotalNowBps:     histBps[historyLen-1],
			TotalHistoryBps: histBps,
		},
	}
	m.apply(m.gen, snapNoDir, nil)

	bFallback := tui.New(100, 30)
	m.render(bFallback)
	fallStr := bFallback.String()
	if !strings.Contains(fallStr, "Billable") {
		t.Fatalf("fallback view output missing Billable:\n%s", fallStr)
	}
}

func TestTopControlReplyDeserializationDirectional(t *testing.T) {
	snapJSON := `{
		"version": "v3.23.0-fix.36.0",
		"state": "flowing",
		"rate": {
			"now_bps": 1500000,
			"history_bps": [1500000, 1600000],
			"history_rx_bps": [1000000, 1100000],
			"history_tx_bps": [500000, 500000]
		},
		"traffic": {
			"total_now_bps": 3000000,
			"total_history_bps": [3000000, 3200000],
			"total_history_rx_bps": [2000000, 2100000],
			"total_history_tx_bps": [1000000, 1100000]
		}
	}`

	var snap NodeSnapshot
	if err := json.Unmarshal([]byte(snapJSON), &snap); err != nil {
		t.Fatalf("unmarshal snap JSON: %v", err)
	}
	if len(snap.Rate.HistoryRxBps) != 2 || snap.Rate.HistoryRxBps[0] != 1000000 {
		t.Fatalf("unexpected HistoryRxBps: %v", snap.Rate.HistoryRxBps)
	}
	if len(snap.Rate.HistoryTxBps) != 2 || snap.Rate.HistoryTxBps[0] != 500000 {
		t.Fatalf("unexpected HistoryTxBps: %v", snap.Rate.HistoryTxBps)
	}
	if snap.Traffic == nil || len(snap.Traffic.TotalHistoryRxBps) != 2 {
		t.Fatalf("unexpected TotalHistoryRxBps: %+v", snap.Traffic)
	}
	if snap.Traffic.TotalHistoryTxBps[1] != 1100000 {
		t.Fatalf("unexpected TotalHistoryTxBps: %+v", snap.Traffic)
	}

	trafficJSON := `{
		"at_unix_nano": 1700000000000000000,
		"billable_bytes": 10000,
		"total_bytes": 20000,
		"billable_rx_bytes": 6000,
		"billable_tx_bytes": 4000,
		"total_rx_bytes": 12000,
		"total_tx_bytes": 8000
	}`

	var lt LiveTraffic
	if err := json.Unmarshal([]byte(trafficJSON), &lt); err != nil {
		t.Fatalf("unmarshal traffic JSON: %v", err)
	}
	if lt.BillableRxBytes != 6000 || lt.BillableTxBytes != 4000 {
		t.Fatalf("unexpected directional billable bytes: %+v", lt)
	}
	if lt.TotalRxBytes != 12000 || lt.TotalTxBytes != 8000 {
		t.Fatalf("unexpected directional total bytes: %+v", lt)
	}
}
