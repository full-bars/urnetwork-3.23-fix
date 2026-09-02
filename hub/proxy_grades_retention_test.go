package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Regression-test additions:
// 1. pruneProxyGrades retention — the proxy_grades table was never pruned
//    (free-review MEDIUM: the ROW_NUMBER subquery in handleProxiesBest scans
//    the whole table on every call, so unbounded growth is a perf regression
//    on long-lived hubs).
// 2. deleteFromDB must clear a node's grades.
// 3. Multi-node same-hour grade dedup — ROW_NUMBER must pick the freshest
//    verdict when two nodes grade the same proxy in the same hour.

func TestPruneProxyGradesRetainsRecentDeletesOld(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Hour)
	nowHour := now.Unix() / 3600

	p1, _ := s.internProxy("1.2.3.4:1080")
	// Old grade row: 10 days ago (past retainGradeDays=7).
	s.db.Exec(`INSERT INTO proxy_grades (node_id, proxy_id, hour, tier, score, graded, last_graded)
		VALUES ('n1', ?, ?, 'F', 0.4, 1, ?)`, p1, nowHour-240, (nowHour-240)*3600)
	// Recent grade row: 2 hours ago (within retention).
	s.db.Exec(`INSERT INTO proxy_grades (node_id, proxy_id, hour, tier, score, graded, last_graded)
		VALUES ('n1', ?, ?, 'A', 0.95, 1, ?)`, p1, nowHour-2, (nowHour-2)*3600)

	n, err := s.pruneProxyGrades()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("pruned %d rows, want 1 (only the 10-day-old row)", n)
	}

	var cnt int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM proxy_grades`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Fatalf("remaining rows = %d, want 1 (recent grade survives)", cnt)
	}

	var tier string
	if err := s.db.QueryRow(`SELECT tier FROM proxy_grades`).Scan(&tier); err != nil {
		t.Fatal(err)
	}
	if tier != "A" {
		t.Errorf("remaining tier = %q, want A (recent row must survive)", tier)
	}
}

func TestDeleteFromDBClearedGrades(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)

	s.upsert("n1", &nodeState{
		NodeID: "n1", Timestamp: now,
		Proxies: []proxyReport{
			{ID: "p1", Address: "1.2.3.4:1080", TotalRX: 100, TotalTX: 50,
				Tier: "A", Score: 0.95, Graded: true, LastGraded: now.Unix()},
		},
	})
	// Verify grade exists before deletion.
	var cnt int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM proxy_grades`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Fatalf("pre-delete count = %d, want 1", cnt)
	}

	if err := s.deleteFromDB("n1"); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM proxy_grades`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 {
		t.Fatalf("post-delete count = %d, want 0 (deleteFromDB must clear grades)", cnt)
	}
}

func TestProxiesBestMultiNodeGradePicksLatest(t *testing.T) {
	s := newTestStore(t)
	hour := time.Now().UTC().Unix() / 3600
	p1, _ := s.internProxy("10.0.0.1:1080")
	// Fleet daily so p1 clears the >=20 bar.
	for _, d := range []int64{hour/24 - 1, hour / 24} {
		s.db.Exec(`INSERT INTO proxy_fleet_daily (proxy_id, day, rx, tx, acq, denied)
			VALUES (?, ?, 1000, 500, 15, 2)`, p1, d)
	}
	// Node n1 grades p1 as B (last_graded=100).
	s.db.Exec(`INSERT INTO proxy_grades (node_id, proxy_id, hour, tier, score, graded, last_graded)
		VALUES ('n1', ?, ?, 'B', 0.85, 1, 100)`, p1, hour)
	// Node n2 grades p1 as A (last_graded=200, same hour) — should win.
	s.db.Exec(`INSERT INTO proxy_grades (node_id, proxy_id, hour, tier, score, graded, last_graded)
		VALUES ('n2', ?, ?, 'A', 0.95, 1, 200)`, p1, hour)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/proxies/best", handleProxiesBest(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/proxies/best?limit=50")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	var out []struct {
		Addr   string  `json:"addr"`
		Tier   string  `json:"tier"`
		Graded bool    `json:"graded"`
		Score  float64 `json:"score"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 {
		t.Fatalf("expected 1 row, got %d", len(out))
	}
	if out[0].Tier != "A" {
		t.Errorf("tier = %q, want A (n2's grade with higher last_graded should win)", out[0].Tier)
	}
	if out[0].Score <= 0 {
		t.Errorf("composite score = %f, want > 0", out[0].Score)
	}
}
