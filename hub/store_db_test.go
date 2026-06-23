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
