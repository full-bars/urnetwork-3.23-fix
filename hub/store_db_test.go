package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *store {
	t.Helper()
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	t.Cleanup(func() { s.db.Close() })
	return s
}

func TestGzipJSONRoundtrip(t *testing.T) {
	in := []proxyReport{
		{ID: "a", Address: "1.2.3.4", Status: "up", TotalRX: 10, TotalTX: 20, Clients: 3},
		{ID: "b", Address: "5.6.7.8", Status: "dead"},
	}
	blob, err := gzipJSON(in)
	if err != nil {
		t.Fatalf("gzipJSON: %v", err)
	}
	var out []proxyReport
	if err := gunzipJSON(blob, &out); err != nil {
		t.Fatalf("gunzipJSON: %v", err)
	}
	if len(out) != 2 || out[0].ID != "a" || out[0].TotalRX != 10 || out[1].Status != "dead" {
		t.Errorf("roundtrip mismatch: %+v", out)
	}
}

func TestPersistRollup(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	// two reports in the same hour for the same node
	s.upsert("n1", &nodeState{
		NodeID: "n1", Host: "h", Timestamp: now,
		Proxies: []proxyReport{{ID: "p1", TotalRX: 100, TotalTX: 50, BillRX: 10, BillTX: 5, Clients: 2}},
	})
	s.upsert("n1", &nodeState{
		NodeID: "n1", Host: "h", Timestamp: now.Add(30 * time.Second),
		Proxies: []proxyReport{{ID: "p1", TotalRX: 300, TotalTX: 150, BillRX: 30, BillTX: 15, Clients: 5}},
	})

	hist, err := s.history("n1", 24)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("hourly rows = %d, want 1", len(hist))
	}
	h := hist[0]
	if h.Samples != 2 {
		t.Errorf("samples = %d, want 2", h.Samples)
	}
	if h.TotalRX != 300 || h.TotalTX != 150 {
		t.Errorf("totals = %d/%d, want 300/150 (latest snapshot)", h.TotalRX, h.TotalTX)
	}
	if h.PeakClients != 5 {
		t.Errorf("peak_clients = %d, want 5 (max across samples)", h.PeakClients)
	}
}

func TestPruneSnapshots(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	// one fresh, one beyond the retention window
	s.upsert("n1", &nodeState{NodeID: "n1", Timestamp: now, Proxies: []proxyReport{{ID: "p"}}})
	old := now.Add(-(proxySnapshotRetention + time.Hour))
	s.upsert("n1", &nodeState{NodeID: "n1", Timestamp: old, Proxies: []proxyReport{{ID: "p"}}})

	n, err := s.pruneSnapshots()
	if err != nil {
		t.Fatalf("pruneSnapshots: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d snapshots, want 1", n)
	}

	var cnt int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM proxy_snapshots WHERE node_id='n1'`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Errorf("remaining snapshots = %d, want 1", cnt)
	}
}

func TestImportJSONMigration(t *testing.T) {
	dir := t.TempDir()
	jsonPath := filepath.Join(dir, "hub.json")
	legacy := `{"nodes":{"old1":{"node_id":"old1","host":"legacy","version":"v1","proxies":[{"id":"p1","rx":42}]}}}`
	if err := os.WriteFile(jsonPath, []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer s.db.Close()

	if s.Nodes["old1"] == nil {
		t.Fatal("legacy node not migrated into cache")
	}
	if s.Nodes["old1"].Host != "legacy" {
		t.Errorf("host = %q, want legacy", s.Nodes["old1"].Host)
	}
	// hub.json must be retired so we don't re-import on the next boot
	if _, err := os.Stat(jsonPath); !os.IsNotExist(err) {
		t.Errorf("hub.json should be retired after import, stat err = %v", err)
	}
	if _, err := os.Stat(jsonPath + ".imported"); err != nil {
		t.Errorf("hub.json.imported should exist: %v", err)
	}
}

// --- B: proxy analytics tests ---

func TestInternProxy(t *testing.T) {
	s := newTestStore(t)

	id1, err := s.internProxy("1.2.3.4:1080")
	if err != nil {
		t.Fatalf("internProxy: %v", err)
	}
	if id1 <= 0 {
		t.Errorf("id = %d, want >0", id1)
	}

	// Same addr should return same id.
	id2, err := s.internProxy("1.2.3.4:1080")
	if err != nil {
		t.Fatalf("second internProxy: %v", err)
	}
	if id2 != id1 {
		t.Errorf("same addr: got %d, want %d", id2, id1)
	}

	// Different addr should return different id.
	id3, err := s.internProxy("5.6.7.8:1080")
	if err != nil {
		t.Fatalf("third internProxy: %v", err)
	}
	if id3 == id1 {
		t.Errorf("different addr: got %d, want !=%d", id3, id1)
	}
}

func TestPersistWritesProxyNodeHourly(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	addr := "1.2.3.4:1080"

	// First report: delta returns ok=false (no baseline), so NO proxy_node_hourly row.
	s.upsert("n1", &nodeState{
		NodeID: "n1", Timestamp: now,
		Proxies: []proxyReport{
			{ID: "p1", Address: addr, TotalRX: 100, TotalTX: 50, BillRX: 10, BillTX: 5, ContractsAcquired: 2, ContractsDenied: 1, Clients: 3},
			{ID: "p2", Address: addr + "x", TotalRX: 0, TotalTX: 0, BillRX: 0, BillTX: 0, ContractsAcquired: 0, ContractsDenied: 0, Clients: 0},
		},
	})

	var cnt1 int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM proxy_node_hourly`).Scan(&cnt1); err != nil {
		t.Fatal(err)
	}
	if cnt1 != 0 {
		t.Errorf("first report should write 0 proxy_node_hourly rows (no baseline), got %d", cnt1)
	}

	// Second report: monotonic increase → deltas written.
	s.upsert("n1", &nodeState{
		NodeID: "n1", Timestamp: now.Add(30 * time.Second),
		Proxies: []proxyReport{
			{ID: "p1", Address: addr, TotalRX: 300, TotalTX: 150, BillRX: 30, BillTX: 15, ContractsAcquired: 5, ContractsDenied: 3, Clients: 5},
			{ID: "p2", Address: addr + "x", TotalRX: 0, TotalTX: 0, BillRX: 0, BillTX: 0, ContractsAcquired: 0, ContractsDenied: 0, Clients: 0},
		},
	})

	// Active proxy should have one row with correct deltas (200/100/20/10/3/2).
	var rx, tx, billRX, billTX uint64
	var acq, denied, cp int64
	if err := s.db.QueryRow(`
		SELECT rx, tx, bill_rx, bill_tx, acq, denied, clients_peak
		FROM proxy_node_hourly JOIN proxies p ON p.id = proxy_node_hourly.proxy_id
		WHERE p.addr = ?`, addr,
	).Scan(&rx, &tx, &billRX, &billTX, &acq, &denied, &cp); err != nil {
		t.Fatalf("query active proxy: %v", err)
	}
	if rx != 200 || tx != 100 || billRX != 20 || billTX != 10 || acq != 3 || denied != 2 || cp != 5 {
		t.Errorf("active proxy row = rx=%d tx=%d brx=%d btx=%d acq=%d den=%d cp=%d, want 200/100/20/10/3/2/5",
			rx, tx, billRX, billTX, acq, denied, cp)
	}

	// Idle proxy (zero counters both times) should have NO row.
	var cnt2 int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM proxy_node_hourly`).Scan(&cnt2); err != nil {
		t.Fatal(err)
	}
	if cnt2 != 1 {
		t.Errorf("should have exactly 1 proxy_node_hourly row (active only), got %d", cnt2)
	}

	// node_hourly should reflect the latest snapshot's contracts_acquired/denied.
	hist, err := s.history("n1", 24)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(hist) != 1 {
		t.Fatalf("history rows = %d, want 1", len(hist))
	}
	if hist[0].ContractsAcquired != 5 || hist[0].ContractsDenied != 3 {
		t.Errorf("node_hourly contracts = %d/%d, want 5/3",
			hist[0].ContractsAcquired, hist[0].ContractsDenied)
	}
}

func TestRollupAndPrune(t *testing.T) {
	s := newTestStore(t)

	// Use a fixed epoch-hour far in the past so all test hours fall in the
	// same day and are well past the 26h guard. Epoch-hour 1000 = day 41.
	baseHour := int64(1000)
	hours := []int64{baseHour, baseHour + 1, baseHour + 2}
	nodes := []string{"n1", "n2"}
	proxyAddr := "10.0.0.1:1080"
	proxyAddr2 := "10.0.0.2:1080"

	// Intern proxies.
	pid1, err := s.internProxy(proxyAddr)
	if err != nil {
		t.Fatalf("intern: %v", err)
	}
	pid2, err := s.internProxy(proxyAddr2)
	if err != nil {
		t.Fatalf("intern2: %v", err)
	}

	// Insert known values for each hour.
	for i, h := range hours {
		// n1, proxy1: rx=100,110,120
		if _, err := s.db.Exec(`
			INSERT INTO proxy_node_hourly (node_id, proxy_id, hour, rx, tx, bill_rx, bill_tx, acq, denied, clients_peak)
			VALUES (?, ?, ?, ?, 0, 0, 0, 0, 0, 0)`,
			nodes[0], pid1, h, uint64(100+i*10),
		); err != nil {
			t.Fatalf("seed n1/p1 h=%d: %v", h, err)
		}
		// n1, proxy2: rx=200,210,220
		if _, err := s.db.Exec(`
			INSERT INTO proxy_node_hourly (node_id, proxy_id, hour, rx, tx, bill_rx, bill_tx, acq, denied, clients_peak)
			VALUES (?, ?, ?, ?, 0, 0, 0, 0, 0, 0)`,
			nodes[0], pid2, h, uint64(200+i*10),
		); err != nil {
			t.Fatalf("seed n1/p2 h=%d: %v", h, err)
		}
		// n2, proxy1: rx=300,310,320
		if _, err := s.db.Exec(`
			INSERT INTO proxy_node_hourly (node_id, proxy_id, hour, rx, tx, bill_rx, bill_tx, acq, denied, clients_peak)
			VALUES (?, ?, ?, ?, 0, 0, 0, 0, 0, 0)`,
			nodes[1], pid1, h, uint64(300+i*10),
		); err != nil {
			t.Fatalf("seed n2/p1 h=%d: %v", h, err)
		}
	}

	// Mock nowFunc so maxHour (nowFunc-26h) sits just past our test data.
	// This makes rollup only process hours 1000..1002 instead of scanning
	// from epoch 0 to today.
	origNow := nowFunc
	nowFunc = func() time.Time {
		return time.Unix((baseHour+2+27)*3600, 0).UTC() // nowHour = baseHour+2+27, maxHour = baseHour+2+1
	}
	defer func() { nowFunc = origNow }()

	// Set high-water mark so rollup only processes our 3 test hours instead
	// of scanning from epoch=0.
	if _, err := s.db.Exec(`INSERT INTO rollup_state (id, last_hour) VALUES (1, ?)`, baseHour-1); err != nil {
		t.Fatalf("set watermark: %v", err)
	}

	// Run rollup.
	n, err := s.rollupProxyDaily()
	if err != nil {
		t.Fatalf("rollupProxyDaily: %v", err)
	}
	if n != 9 { // 3 hours × 3 (node, proxy) combinations
		t.Errorf("rows affected = %d, want 9", n)
	}

	// Check proxy_node_daily: day = hours[0] / 24.
	day := hours[0] / 24
	var dailyN1P1RX uint64
	if err := s.db.QueryRow(`
		SELECT rx FROM proxy_node_daily
		WHERE node_id = ? AND proxy_id = ? AND day = ?`, nodes[0], pid1, day,
	).Scan(&dailyN1P1RX); err != nil {
		t.Fatalf("query daily n1/p1: %v", err)
	}
	// 100 + 110 + 120 = 330
	if dailyN1P1RX != 330 {
		t.Errorf("daily n1/p1 rx = %d, want 330", dailyN1P1RX)
	}

	// Check proxy_fleet_daily: proxy1 should sum n1 + n2 across all 3 hours.
	// n1 rx for pid1: 100+110+120=330, n2: 300+310+320=930. Total = 1260.
	// node_count should be 2 (n1 and n2).
	var fleetP1RX uint64
	var fleetP1NodeCount int64
	if err := s.db.QueryRow(`
		SELECT rx, node_count FROM proxy_fleet_daily
		WHERE proxy_id = ? AND day = ?`, pid1, day,
	).Scan(&fleetP1RX, &fleetP1NodeCount); err != nil {
		t.Fatalf("query fleet pid1: %v", err)
	}
	if fleetP1RX != 1260 {
		t.Errorf("fleet pid1 rx = %d, want 1260", fleetP1RX)
	}
	if fleetP1NodeCount != 2 {
		t.Errorf("fleet pid1 node_count = %d, want 2", fleetP1NodeCount)
	}

	// Run rollup again — must not double-count.
	n2, err := s.rollupProxyDaily()
	if err != nil {
		t.Fatalf("second rollupProxyDaily: %v", err)
	}
	if n2 != 0 {
		t.Errorf("second rollup should affect 0 rows, got %d", n2)
	}

	// Prune hourly: force retention to 1 day.
	origRetention := retainHourlyDays
	retainHourlyDays = 1
	defer func() { retainHourlyDays = origRetention }()

	pruned, err := s.pruneProxyHourly()
	if err != nil {
		t.Fatalf("pruneProxyHourly: %v", err)
	}
	if pruned == 0 {
		t.Error("pruneProxyHourly should have pruned rows")
	}

	// Hourly rows should be gone.
	var hourlyCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM proxy_node_hourly`).Scan(&hourlyCount); err != nil {
		t.Fatal(err)
	}
	if hourlyCount != 0 {
		t.Errorf("hourly rows remaining = %d, want 0", hourlyCount)
	}

	// Daily rows should still be intact.
	var dailyCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM proxy_node_daily`).Scan(&dailyCount); err != nil {
		t.Fatal(err)
	}
	if dailyCount != 3 {
		t.Errorf("daily rows after prune = %d, want 3", dailyCount)
	}
}
