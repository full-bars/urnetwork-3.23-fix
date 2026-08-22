package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func seedProxyTopData(t *testing.T, s *store) (now int64) {
	t.Helper()
	now = time.Now().UTC().Unix() / 3600

	// Intern proxies.
	p1, _ := s.internProxy("10.0.0.1:1080")
	p2, _ := s.internProxy("10.0.0.2:1080")
	p3, _ := s.internProxy("10.0.0.3:1080")

	// Insert proxy_node_hourly rows for 2 hours, 2 nodes.
	for _, h := range []int64{now - 2, now - 1} {
		// n1, p1: rx=100, tx=50, acq=2, den=1
		s.db.Exec(`INSERT INTO proxy_node_hourly (node_id, proxy_id, hour, rx, tx, bill_rx, bill_tx, acq, denied, clients_peak)
			VALUES ('n1', ?, ?, 100, 50, 10, 5, 2, 1, 3)`, p1, h)
		// n1, p2: rx=200, tx=100, acq=5, den=0
		s.db.Exec(`INSERT INTO proxy_node_hourly (node_id, proxy_id, hour, rx, tx, bill_rx, bill_tx, acq, denied, clients_peak)
			VALUES ('n1', ?, ?, 200, 100, 20, 10, 5, 0, 7)`, p2, h)
		// n2, p1: rx=300, tx=150, acq=1, den=4
		s.db.Exec(`INSERT INTO proxy_node_hourly (node_id, proxy_id, hour, rx, tx, bill_rx, bill_tx, acq, denied, clients_peak)
			VALUES ('n2', ?, ?, 300, 150, 30, 15, 1, 4, 2)`, p1, h)
		// n2, p3: rx=50, tx=25, acq=0, den=10
		s.db.Exec(`INSERT INTO proxy_node_hourly (node_id, proxy_id, hour, rx, tx, bill_rx, bill_tx, acq, denied, clients_peak)
			VALUES ('n2', ?, ?, 50, 25, 5, 2, 0, 10, 1)`, p3, h)
	}
	return now
}

func TestProxiesTopSortedByTraffic(t *testing.T) {
	s := newTestStore(t)
	seedProxyTopData(t, s)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/proxies/top", handleProxiesTop(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/proxies/top?window=24h&sort=traffic&order=desc")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Traffic = rx+tx. Per proxy across 2 hours:
	// p1 (n1+n2): rx=100+200+300+300=900, tx=50+100+150+150=450 → total=1350
	// p2 (n1): rx=200+200=400, tx=100+100=200 → total=600
	// p3 (n2): rx=50+50=100, tx=25+25=50 → total=150
	// Descending: p1, p2, p3.
	if len(rows) < 3 {
		t.Fatalf("rows = %d, want >= 3", len(rows))
	}
	if rows[0]["addr"] != "10.0.0.1:1080" {
		t.Errorf("top (desc traffic) = %q, want 10.0.0.1:1080", rows[0]["addr"])
	}
	if rows[2]["addr"] != "10.0.0.3:1080" {
		t.Errorf("last (desc traffic) = %q, want 10.0.0.3:1080", rows[2]["addr"])
	}

	// p1 appeared on 2 nodes (n1, n2).
	if n, ok := rows[0]["nodes"].(float64); !ok || n != 2 {
		t.Errorf("p1 nodes = %v, want 2", rows[0]["nodes"])
	}
}

func TestProxiesTopFilteredByNode(t *testing.T) {
	s := newTestStore(t)
	seedProxyTopData(t, s)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/proxies/top", handleProxiesTop(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/proxies/top?window=24h&sort=traffic&node=n2")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// Node n2 only: p1 (300+300=600 rx, 150+150=300 tx → 900) and p3 (50+50=100, 25+25=50 → 150).
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		addr := r["addr"].(string)
		if addr != "10.0.0.1:1080" && addr != "10.0.0.3:1080" {
			t.Errorf("unexpected addr %q on n2", addr)
		}
		if n, ok := r["nodes"].(float64); !ok || n != 1 {
			t.Errorf("%s nodes = %v, want 1 (filtered to n2)", addr, r["nodes"])
		}
	}
}

func TestProxiesTopSortedByContracts(t *testing.T) {
	s := newTestStore(t)
	seedProxyTopData(t, s)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/proxies/top", handleProxiesTop(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/proxies/top?window=24h&sort=contracts&order=desc")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var rows []map[string]any
	json.NewDecoder(resp.Body).Decode(&rows)

	// acq totals: p2=10, p1=6, p3=0. Descending: p2, p1, p3.
	if rows[0]["addr"] != "10.0.0.2:1080" {
		t.Errorf("top by contracts = %q, want 10.0.0.2:1080", rows[0]["addr"])
	}
}

func TestProxiesTopLimitsResults(t *testing.T) {
	s := newTestStore(t)
	seedProxyTopData(t, s)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/proxies/top", handleProxiesTop(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/proxies/top?window=24h&limit=2")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var rows []map[string]any
	json.NewDecoder(resp.Body).Decode(&rows)
	if len(rows) != 2 {
		t.Errorf("limit=2 returned %d rows", len(rows))
	}
}

func TestProxiesHistoryWithSplit(t *testing.T) {
	s := newTestStore(t)
	now := seedProxyTopData(t, s)
	_ = now

	mux := http.NewServeMux()
	mux.HandleFunc("/api/proxies/history", handleProxiesHistory(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/proxies/history?addr=10.0.0.1:1080&window=24h&split=node")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var body struct {
		Series []struct {
			Node   string `json:"node"`
			Points []struct {
				TS  int64  `json:"ts"`
				RX  uint64 `json:"rx"`
				TX  uint64 `json:"tx"`
				Acq int64  `json:"acq"`
				Den int64  `json:"den"`
			} `json:"points"`
		} `json:"series"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(body.Series) != 2 {
		t.Fatalf("series count = %d, want 2 (n1, n2)", len(body.Series))
	}
	// Each series should have 2 points (hours now-2 and now-1).
	for _, s := range body.Series {
		if len(s.Points) != 2 {
			t.Errorf("node %s: points = %d, want 2", s.Node, len(s.Points))
		}
	}
}

func TestProxiesHistoryMissingAddr(t *testing.T) {
	s := newTestStore(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/proxies/history", handleProxiesHistory(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/proxies/history?window=24h")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 (missing addr)", resp.StatusCode)
	}
}

func TestProxiesHistoryUnknownAddr(t *testing.T) {
	s := newTestStore(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/proxies/history", handleProxiesHistory(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/proxies/history?addr=unknown:1080&window=24h")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 404 {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
}

func TestNodeContractsSeries(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()

	// Seed node_hourly with contracts data via persist.
	s.upsert("n1", &nodeState{
		NodeID: "n1", Timestamp: now,
		Proxies: []proxyReport{
			{ID: "p", Address: "1.2.3.4:1080", ContractsAcquired: 10, ContractsDenied: 2},
		},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/api/nodes/contracts", handleNodeContracts(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/nodes/contracts?node=n1&window=24h")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var points []struct {
		TS  int64 `json:"ts"`
		Acq int64 `json:"acq"`
		Den int64 `json:"denied"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&points); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(points) == 0 {
		t.Fatal("expected at least 1 contract data point")
	}
	p := points[0]
	if p.Acq != 10 || p.Den != 2 {
		t.Errorf("contracts = acq=%d den=%d, want acq=10 den=2", p.Acq, p.Den)
	}
}

func TestNodeContractsMissingNode(t *testing.T) {
	s := newTestStore(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/nodes/contracts", handleNodeContracts(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/nodes/contracts?window=24h")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 (missing node)", resp.StatusCode)
	}
}

func TestProxiesBestCompositeScore(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Unix() / 3600 / 24

	p1, _ := s.internProxy("10.0.0.1:1080")
	p2, _ := s.internProxy("10.0.0.2:1080")

	// p1: 90% win rate (90/10 = 100 total, above min sample)
	s.db.Exec(`INSERT INTO proxy_fleet_daily (proxy_id, day, rx, tx, acq, denied) VALUES (?, ?, 100, 0, 90, 10)`, p1, now)

	// p2: 10% win rate (10/90 = 100 total, above min sample)
	s.db.Exec(`INSERT INTO proxy_fleet_daily (proxy_id, day, rx, tx, acq, denied) VALUES (?, ?, 1000000, 0, 10, 90)`, p2, now)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/proxies/best", handleProxiesBest(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/proxies/best?limit=100")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(rows) < 2 {
		t.Fatalf("rows = %d, want >= 2", len(rows))
	}
	if rows[0]["addr"] != "10.0.0.1:1080" {
		t.Errorf("top by score = %q, want 10.0.0.1:1080", rows[0]["addr"])
	}
}

func TestProxiesBestMinSampleFilter(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Unix() / 3600 / 24

	pSmall, _ := s.internProxy("10.0.0.3:1080")
	pBoundary, _ := s.internProxy("10.0.0.4:1080")

	s.db.Exec(`INSERT INTO proxy_fleet_daily (proxy_id, day, rx, tx, acq, denied) VALUES (?, ?, 100, 0, 1, 0)`, pSmall, now)
	s.db.Exec(`INSERT INTO proxy_fleet_daily (proxy_id, day, rx, tx, acq, denied) VALUES (?, ?, 100, 0, 18, 2)`, pBoundary, now)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/proxies/best", handleProxiesBest(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/proxies/best?limit=100")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, r := range rows {
		addr := r["addr"].(string)
		if addr == "10.0.0.3:1080" {
			t.Errorf("proxy %s should be excluded (sample < 20)", addr)
		}
	}
	if len(rows) != 1 || rows[0]["addr"] != "10.0.0.4:1080" {
		t.Errorf("expected only boundary proxy, got %d rows", len(rows))
	}
}

func TestProxiesBestDeadStatus(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Unix() / 3600 / 24

	active, _ := s.internProxy("10.0.0.5:1080")
	dead10, _ := s.internProxy("10.0.0.6:1080")

	s.db.Exec(`INSERT INTO proxy_fleet_daily (proxy_id, day, rx, tx, acq, denied) VALUES (?, ?, 100, 0, 20, 5)`, active, now)
	s.db.Exec(`INSERT INTO proxy_fleet_daily (proxy_id, day, rx, tx, acq, denied) VALUES (?, ?, 100, 0, 18, 2)`, dead10, now-10)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/proxies/best", handleProxiesBest(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/proxies/best?limit=100&hide_dead=false")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}

	for _, r := range rows {
		addr := r["addr"].(string)
		status := r["status"].(string)
		if addr == "10.0.0.5:1080" && status != "active" {
			t.Errorf("active proxy status = %q, want active", status)
		}
		if addr == "10.0.0.6:1080" && status == "active" {
			t.Errorf("dead proxy status = %q, should be 'dead Nd ago'", status)
		}
	}
}

func TestProxiesBestHideDeadParam(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Unix() / 3600 / 24

	dead, _ := s.internProxy("10.0.0.7:1080")

	s.db.Exec(`INSERT INTO proxy_fleet_daily (proxy_id, day, rx, tx, acq, denied) VALUES (?, ?, 100, 0, 18, 2)`, dead, now-10)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/proxies/best", handleProxiesBest(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/proxies/best?limit=100&hide_dead=true")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(rows) != 0 {
		t.Errorf("hide_dead=true returned %d rows, want 0", len(rows))
	}
}

// win_pct must be on a 0-100 scale (matching the pct() convention used
// elsewhere in the hub, e.g. loadProxies()'s win% column), not the raw
// 0-1 fraction computed by the SQL query. The frontend's color thresholds
// (>=80, >=60) and "%" display assume 0-100.
func TestProxiesBestWinPctIsPercentScale(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC().Unix() / 3600 / 24

	p, _ := s.internProxy("10.0.0.9:1080")
	s.db.Exec(`INSERT INTO proxy_fleet_daily (proxy_id, day, rx, tx, acq, denied) VALUES (?, ?, 100, 0, 17, 3)`, p, now)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/proxies/best", handleProxiesBest(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/proxies/best?limit=100")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var rows []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	winPct := rows[0]["win_pct"].(float64)
	if winPct < 50 || winPct > 100 {
		t.Errorf("win_pct = %v, want ~85 (0-100 scale); got a value that looks like a 0-1 fraction instead", winPct)
	}
	if want := 85.0; winPct < want-0.01 || winPct > want+0.01 {
		t.Errorf("win_pct = %v, want %v", winPct, want)
	}
}

// Regression test: each nav item's markup must render exactly once. A prior
// version of the "Best Proxies" addition accidentally duplicated its
// nav-item <div>, rendering the button twice in the sidebar.
func TestDashboardNavItemsNotDuplicated(t *testing.T) {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, map[string]interface{}{
		"Rows": []nodeRow{},
		"Sum":  summaryRow{},
	}); err != nil {
		t.Fatalf("template execute: %v", err)
	}
	html := buf.String()

	for _, page := range []string{"overview", "servers", "proxies", "contracts", "best"} {
		want := `data-page="` + page + `"`
		if got := strings.Count(html, want); got != 1 {
			t.Errorf("nav-item for %q rendered %d times in dashboard HTML, want 1", page, got)
		}
	}
}
