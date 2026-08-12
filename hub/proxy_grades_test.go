package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Hub-side grade surfacing (roadmap step 3): the provider's report now
// carries per-proxy A-F grades (tier/score/graded/failed/last_graded), the
// hub ingests them into a separate proxy_grades table (latest-wins per
// node+proxy per hour, with history), and the best-proxies endpoint joins
// the latest grade so the dashboard can show an A-F badge per proxy.

func TestPersistWritesProxyGrades(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)

	s.upsert("n1", &nodeState{
		NodeID: "n1", Timestamp: now,
		Proxies: []proxyReport{
			{
				ID: "p1", Address: "1.2.3.4:1080",
				TotalRX: 100, TotalTX: 50,
				Tier: "A", Score: 0.95, Graded: true, LastGraded: now.Add(-5 * time.Minute).Unix(),
			},
			{
				ID: "p2", Address: "5.6.7.8:1080",
				TotalRX: 200, TotalTX: 100,
				Tier: "C", Score: 0.71, Graded: true, LastGraded: now.Add(-10 * time.Minute).Unix(),
			},
		},
	})

	var cnt int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM proxy_grades`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 2 {
		t.Fatalf("proxy_grades rows = %d, want 2 (both graded proxies ingested)", cnt)
	}

	var tier string
	var score float64
	var graded int
	var lastGraded int64
	if err := s.db.QueryRow(`
		SELECT tier, score, graded, last_graded
		FROM proxy_grades JOIN proxies p ON p.id = proxy_grades.proxy_id
		WHERE p.addr = '1.2.3.4:1080'`).Scan(&tier, &score, &graded, &lastGraded); err != nil {
		t.Fatal(err)
	}
	if tier != "A" || score != 0.95 || graded != 1 || lastGraded != now.Add(-5*time.Minute).Unix() {
		t.Errorf("grade row = tier=%q score=%f graded=%d last_graded=%d, want A/0.95/1/%d",
			tier, score, graded, lastGraded, now.Add(-5*time.Minute).Unix())
	}
}

func TestPersistSkipsUngradedProxies(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	s.upsert("n1", &nodeState{
		NodeID: "n1", Timestamp: now,
		Proxies: []proxyReport{
			{ID: "p1", Address: "1.2.3.4:1080", TotalRX: 100, TotalTX: 50, Graded: false},
		},
	})

	var cnt int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM proxy_grades`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 0 {
		t.Fatalf("proxy_grades rows = %d, want 0 for an ungraded proxy", cnt)
	}
}

func TestPersistProxyGradesLatestWinsPerHour(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Hour).Add(30 * time.Minute)
	hour := now.Unix() / 3600

	// First report: grade A.
	s.upsert("n1", &nodeState{
		NodeID: "n1", Timestamp: now,
		Proxies: []proxyReport{
			{ID: "p1", Address: "1.2.3.4:1080", TotalRX: 100, TotalTX: 50, Tier: "A", Score: 0.95, Graded: true, LastGraded: now.Unix()},
		},
	})
	// Same hour, second report: grade downgraded to B — latest wins, no new row.
	s.upsert("n1", &nodeState{
		NodeID: "n1", Timestamp: now.Add(5 * time.Minute),
		Proxies: []proxyReport{
			{ID: "p1", Address: "1.2.3.4:1080", TotalRX: 200, TotalTX: 100, Tier: "B", Score: 0.88, Graded: true, LastGraded: now.Add(5 * time.Minute).Unix()},
		},
	})

	var cnt int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM proxy_grades WHERE hour = ?`, hour).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 1 {
		t.Fatalf("rows for hour %d = %d, want 1 (latest-wins, no duplicate per hour)", hour, cnt)
	}

	var tier string
	var lastGraded int64
	if err := s.db.QueryRow(`SELECT tier, last_graded FROM proxy_grades WHERE hour = ?`, hour).Scan(&tier, &lastGraded); err != nil {
		t.Fatal(err)
	}
	if tier != "B" {
		t.Errorf("tier = %q, want B (latest report wins)", tier)
	}
	if lastGraded != now.Add(5*time.Minute).Unix() {
		t.Errorf("last_graded = %d, want the second report's %d", lastGraded, now.Add(5*time.Minute).Unix())
	}
}

func TestProxiesBestIncludesGrade(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Unix() / 3600

	p1, _ := s.internProxy("10.0.0.1:1080")
	p2, _ := s.internProxy("10.0.0.2:1080")

	// Fleet daily rows so both proxies clear the (acq+denied) >= 20 bar.
	for _, pid := range []int64{p1, p2} {
		for _, d := range []int64{now/24 - 1, now / 24} {
			s.db.Exec(`INSERT INTO proxy_fleet_daily (proxy_id, day, rx, tx, acq, denied)
				VALUES (?, ?, 1000, 500, 15, 2)`, pid, d)
		}
	}

	// Grades: p1 = A, p2 = ungraded (no row). hourStart converts the
	// epoch-hour back to hour-aligned unix seconds for the last_graded column.
	hourStart := now * 3600
	s.db.Exec(`INSERT INTO proxy_grades (node_id, proxy_id, hour, tier, score, graded, last_graded)
		VALUES ('n1', ?, ?, 'A', 0.95, 1, ?)`, p1, now, hourStart)

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
		Addr    string  `json:"addr"`
		Score   float64 `json:"score"` // composite, unchanged
		Tier    string  `json:"tier"`
		Graded  bool    `json:"graded"`
		LastDay int64   `json:"last_day"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}

	var p1Row, p2Row *struct {
		Addr    string  `json:"addr"`
		Score   float64 `json:"score"`
		Tier    string  `json:"tier"`
		Graded  bool    `json:"graded"`
		LastDay int64   `json:"last_day"`
	}
	for i := range out {
		switch out[i].Addr {
		case "10.0.0.1:1080":
			p1Row = &out[i]
		case "10.0.0.2:1080":
			p2Row = &out[i]
		}
	}
	if p1Row == nil || p2Row == nil {
		t.Fatalf("expected both proxies in best list, got %d rows", len(out))
	}
	if p1Row.Tier != "A" || !p1Row.Graded {
		t.Errorf("p1 grade = tier=%q graded=%v, want A/true", p1Row.Tier, p1Row.Graded)
	}
	if p2Row.Tier != "" || p2Row.Graded {
		t.Errorf("p2 grade = tier=%q graded=%v, want empty/false (ungraded)", p2Row.Tier, p2Row.Graded)
	}
	if p1Row.Score <= 0 {
		t.Errorf("p1 composite score = %f, want > 0 (composite Score must be preserved)", p1Row.Score)
	}
}
