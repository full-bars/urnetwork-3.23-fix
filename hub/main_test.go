package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --- Helper function tests ---

func TestFmtBytes(t *testing.T) {
	tests := []struct {
		input uint64
		want  string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1048576, "1.0 MB"},
		{1073741824, "1.0 GB"},
		{1099511627776, "1.0 TB"},
	}
	for _, tt := range tests {
		got := fmtBytes(tt.input)
		if got != tt.want {
			t.Errorf("fmtBytes(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFmtMbps(t *testing.T) {
	tests := []struct {
		input float64
		want  string
	}{
		{0, "—"},
		{0.005, "—"},
		{0.5, "500 Kbps"},
		{1.0, "1.0 Mbps"},
		{50.0, "50.0 Mbps"},
		{100, "100 Mbps"},
		{500, "500 Mbps"},
	}
	for _, tt := range tests {
		got := fmtMbps(tt.input)
		if got != tt.want {
			t.Errorf("fmtMbps(%f) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFmtAge(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "—"},
		{-1, "—"},
		{30, "30s"},
		{120, "2m"},
		{3600, "1h"},
		{7200, "2h"},
	}
	for _, tt := range tests {
		got := fmtAge(tt.input)
		if got != tt.want {
			t.Errorf("fmtAge(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestTitle(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"up", "Up"},
		{"connecting", "Connecting"},
		{"degraded", "Degraded"},
		{"", ""},
	}
	for _, tt := range tests {
		got := title(tt.input)
		if got != tt.want {
			t.Errorf("title(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// --- Store tests ---

func TestStoreUpsertAndList(t *testing.T) {
	s := &store{
		Nodes: make(map[string]*nodeState),
		rates: make(map[string]*nodeRate),
	}

	now := time.Now().UTC()

	s.upsert("node1", &nodeState{
		NodeID:    "node1",
		Host:      "host1",
		Version:   "v1.0",
		Timestamp: now,
		Uptime:    3600,
		Proxies: []proxyReport{
			{ID: "p1", Status: "up", TotalRX: 1000, TotalTX: 500, BillRX: 800, BillTX: 400, Clients: 3, MaxAge: 120},
			{ID: "p2", Status: "connecting"},
		},
	})

	list := s.list()
	if len(list) != 1 {
		t.Fatalf("list length = %d, want 1", len(list))
	}
	if list[0].NodeID != "node1" {
		t.Errorf("node ID = %q, want %q", list[0].NodeID, "node1")
	}

	s.upsert("node2", &nodeState{
		NodeID:    "node2",
		Host:      "host2",
		Timestamp: now.Add(time.Second),
		Proxies: []proxyReport{
			{ID: "p3", Status: "up", TotalRX: 2000, TotalTX: 1000},
		},
	})

	list = s.list()
	if len(list) != 2 {
		t.Fatalf("list length = %d, want 2", len(list))
	}
	if list[0].NodeID != "node2" {
		t.Errorf("first (newest) node = %q, want %q", list[0].NodeID, "node2")
	}
}

func TestStoreSummary(t *testing.T) {
	s := &store{
		Nodes: make(map[string]*nodeState),
		rates: make(map[string]*nodeRate),
	}

	now := time.Now().UTC()
	s.Nodes["n1"] = &nodeState{Timestamp: now, Proxies: []proxyReport{
		{Status: "up", TotalRX: 100, TotalTX: 50, BillRX: 80, BillTX: 40, Clients: 2},
		{Status: "up", TotalRX: 200, TotalTX: 100, BillRX: 150, BillTX: 80, Clients: 1},
		{Status: "connecting"},
		{Status: "degraded"},
	}}
	s.Nodes["n2"] = &nodeState{Timestamp: now, Proxies: []proxyReport{
		{Status: "up", TotalRX: 400, TotalTX: 200, BillRX: 300, BillTX: 150, Clients: 5},
		{Status: "dead"},
	}}

	sum := s.summary()
	if sum.Nodes != 2 {
		t.Errorf("nodes = %d, want 2", sum.Nodes)
	}
	if sum.Up != 3 {
		t.Errorf("up = %d, want 3", sum.Up)
	}
	if sum.Connecting != 1 {
		t.Errorf("connecting = %d, want 1", sum.Connecting)
	}
	if sum.Degraded != 1 {
		t.Errorf("degraded = %d, want 1", sum.Degraded)
	}
	if sum.Dead != 1 {
		t.Errorf("dead = %d, want 1", sum.Dead)
	}
	if sum.TotalClients != 8 {
		t.Errorf("clients = %d, want 8", sum.TotalClients)
	}
	if sum.TotalRX != 700 {
		t.Errorf("total_rx = %d, want 700", sum.TotalRX)
	}
	if sum.TotalTX != 350 {
		t.Errorf("total_tx = %d, want 350", sum.TotalTX)
	}
	if sum.BillRX != 530 {
		t.Errorf("bill_rx = %d, want 530", sum.BillRX)
	}
	if sum.BillTX != 270 {
		t.Errorf("bill_tx = %d, want 270", sum.BillTX)
	}
}

func TestStoreRateCalculation(t *testing.T) {
	s := &store{
		Nodes: make(map[string]*nodeState),
		rates: make(map[string]*nodeRate),
	}

	now := time.Now().UTC()

	s.upsert("n1", &nodeState{
		NodeID: "n1", Timestamp: now,
		Proxies: []proxyReport{
			{TotalRX: 0, TotalTX: 0},
		},
	})

	rx, tx := s.getRate("n1")
	if rx != 0 || tx != 0 {
		t.Errorf("first report rate: rx=%f tx=%f, want 0", rx, tx)
	}

	s.upsert("n1", &nodeState{
		NodeID: "n1", Timestamp: now.Add(10 * time.Second),
		Proxies: []proxyReport{
			{TotalRX: 5_000_000, TotalTX: 2_500_000},
			{TotalRX: 5_000_000, TotalTX: 2_500_000},
		},
	})

	rx, tx = s.getRate("n1")
	if rx != 8.0 {
		t.Errorf("rx rate = %f, want 8.0", rx)
	}
	if tx != 4.0 {
		t.Errorf("tx rate = %f, want 4.0", tx)
	}
}

func TestStoreRateZeroTimeDelta(t *testing.T) {
	s := &store{
		Nodes: make(map[string]*nodeState),
		rates: make(map[string]*nodeRate),
	}

	now := time.Now().UTC()

	s.upsert("n1", &nodeState{
		NodeID: "n1", Timestamp: now,
		Proxies: []proxyReport{{TotalRX: 0}},
	})

	s.upsert("n1", &nodeState{
		NodeID: "n1", Timestamp: now,
		Proxies: []proxyReport{{TotalRX: 1_000_000}},
	})

	rx, _ := s.getRate("n1")
	if rx != 0 {
		t.Errorf("zero-delta rate = %f, want 0", rx)
	}
}

func TestStoreRateForUnknownNode(t *testing.T) {
	s := &store{
		Nodes: make(map[string]*nodeState),
		rates: make(map[string]*nodeRate),
	}

	rx, tx := s.getRate("nonexistent")
	if rx != 0 || tx != 0 {
		t.Errorf("unknown node rate = %f,%f, want 0,0", rx, tx)
	}
}

func TestStoreLoadAndSave(t *testing.T) {
	dir := t.TempDir()

	s, err := openStore(dir)
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	if len(s.Nodes) != 0 {
		t.Errorf("nodes = %d, want 0", len(s.Nodes))
	}

	s.upsert("test", &nodeState{
		NodeID:    "test",
		Host:      "h",
		Timestamp: time.Now().UTC(),
		Proxies:   []proxyReport{{ID: "p1", TotalRX: 100, TotalTX: 200, Clients: 1}},
	})
	s.db.Close()

	// reopen: the node and its latest snapshot must come back from hub.db
	s2, err := openStore(dir)
	if err != nil {
		t.Fatalf("reopen openStore: %v", err)
	}
	defer s2.db.Close()
	if s2.Nodes["test"] == nil {
		t.Fatal("test node not loaded from db")
	}
	if s2.Nodes["test"].Host != "h" {
		t.Errorf("host = %q, want %q", s2.Nodes["test"].Host, "h")
	}
	if len(s2.Nodes["test"].Proxies) != 1 || s2.Nodes["test"].Proxies[0].TotalRX != 100 {
		t.Errorf("proxy snapshot not restored: %+v", s2.Nodes["test"].Proxies)
	}
}

func TestStoreLoadNonexistent(t *testing.T) {
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore on empty dir: %v", err)
	}
	defer s.db.Close()
	if len(s.Nodes) != 0 {
		t.Errorf("nodes = %d, want 0", len(s.Nodes))
	}
}

// --- HTTP handler tests ---

func TestReportEndpointValid(t *testing.T) {
	s := &store{
		Nodes: make(map[string]*nodeState),
		rates: make(map[string]*nodeRate),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/report", handleReport(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	report := nodeState{
		NodeID: "test-node",
		Host:   "test-host",
		Proxies: []proxyReport{
			{ID: "p1", Status: "up", TotalRX: 1000, TotalTX: 500, BillRX: 800, BillTX: 400},
		},
	}
	body, _ := json.Marshal(report)
	resp, err := http.Post(ts.URL+"/api/report", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}

	if s.Nodes["test-node"] == nil {
		t.Errorf("node not stored after report")
	}
}

func TestReportEndpointMissingNodeID(t *testing.T) {
	s := &store{
		Nodes: make(map[string]*nodeState),
		rates: make(map[string]*nodeRate),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/report", handleReport(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	report := nodeState{Host: "no-id"}
	body, _ := json.Marshal(report)
	resp, err := http.Post(ts.URL+"/api/report", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestReportEndpointGetRejected(t *testing.T) {
	s := &store{
		Nodes: make(map[string]*nodeState),
		rates: make(map[string]*nodeRate),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/report", handleReport(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/report")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestReportEndpointInvalidBody(t *testing.T) {
	s := &store{
		Nodes: make(map[string]*nodeState),
		rates: make(map[string]*nodeRate),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/report", handleReport(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/report", "application/json", bytes.NewReader([]byte("not json")))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestNodesEndpoint(t *testing.T) {
	s := &store{
		Nodes: make(map[string]*nodeState),
		rates: make(map[string]*nodeRate),
	}
	s.Nodes["n1"] = &nodeState{NodeID: "n1", Host: "h1", Proxies: []proxyReport{{ID: "p1", Status: "up"}}}
	s.Nodes["n2"] = &nodeState{NodeID: "n2", Host: "h2"}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/nodes", handleNodes(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/nodes")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	var nodes []*nodeState
	if err := json.NewDecoder(resp.Body).Decode(&nodes); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(nodes))
	}
	if nodes[0].NodeID != "n1" && nodes[0].NodeID != "n2" {
		t.Errorf("unexpected node ID: %q", nodes[0].NodeID)
	}
}

func TestDashboardEndpoint(t *testing.T) {
	s := &store{
		Nodes: make(map[string]*nodeState),
		rates: make(map[string]*nodeRate),
	}
	s.Nodes["n1"] = &nodeState{
		NodeID: "n1", Host: "h1", Version: "v1",
		Timestamp: time.Now().UTC(), Uptime: 3600,
		Proxies: []proxyReport{
			{ID: "p1", Status: "up", TotalRX: 1000, TotalTX: 500, BillRX: 800, BillTX: 400, Clients: 2},
		},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleDashboard(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("n1")) {
		t.Errorf("dashboard body does not contain node ID")
	}
	if !bytes.Contains(body, []byte("URnetwork Hub")) {
		t.Errorf("dashboard body does not contain title")
	}
	if !bytes.Contains(body, []byte("billable")) {
		t.Errorf("dashboard body does not contain billable labels")
	}
}

func TestDashboardEndpointEmpty(t *testing.T) {
	s := &store{
		Nodes: make(map[string]*nodeState),
		rates: make(map[string]*nodeRate),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", handleDashboard(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("Nodes")) || !bytes.Contains(body, []byte(">0<")) {
		t.Errorf("empty dashboard should show Nodes = 0")
	}
}

func TestRemoveEndpoint(t *testing.T) {
	s := &store{
		Nodes: make(map[string]*nodeState),
		rates: make(map[string]*nodeRate),
	}
	s.Nodes["n1"] = &nodeState{NodeID: "n1"}
	s.Nodes["n2"] = &nodeState{NodeID: "n2"}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/nodes/remove", handleNodeRemove(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Valid remove
	body, _ := json.Marshal(removeRequest{NodeID: "n1"})
	resp, err := http.Post(ts.URL+"/api/nodes/remove", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 204 {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	if s.Nodes["n1"] != nil {
		t.Errorf("n1 not removed")
	}
	if s.Nodes["n2"] == nil {
		t.Errorf("n2 was incorrectly removed")
	}

	// Missing node_id
	body, _ = json.Marshal(removeRequest{})
	resp, err = http.Post(ts.URL+"/api/nodes/remove", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}

	// GET should fail
	resp, err = http.Get(ts.URL + "/api/nodes/remove")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}
