package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

var Version string

var funcMap = template.FuncMap{
	"fmtBytes": fmtBytes,
	"fmtMbps":  fmtMbps,
	"title":    title,
	"fmtAge":   fmtAge,
	"pct":      func(a, b int) float64 { return float64(a) / float64(b) * 100 },
}

type proxyReport struct {
	ID      string `json:"id"`
	Address string `json:"addr"`
	Status  string `json:"status"`
	TotalRX uint64 `json:"rx"`
	TotalTX uint64 `json:"tx"`
	BillRX  uint64 `json:"bill_rx"`
	BillTX  uint64 `json:"bill_tx"`
	Clients int64  `json:"clients"`
	MaxAge  int64  `json:"max_age_s"`
}

type systemMetrics struct {
	HeapMiB     uint64 `json:"heap_mib"`
	SysMiB      uint64 `json:"sys_mib"`
	Connections int64  `json:"conns"`
}

type nodeState struct {
	NodeID    string         `json:"node_id"`
	Host      string         `json:"host"`
	Version   string         `json:"version"`
	Timestamp time.Time      `json:"ts"`
	Uptime    float64        `json:"uptime"`
	Proxies   []proxyReport  `json:"proxies"`
	System    systemMetrics  `json:"sys"`
}

type store struct {
	mu           sync.RWMutex
	db           *sql.DB
	Nodes        map[string]*nodeState        `json:"nodes"`
	rates        map[string]*nodeRate         `json:"-"`
	prevBillable map[string]map[string]uint64 `json:"-"` // nodeID -> proxyID -> last seen BillRX+BillTX
	earning      map[string]map[string]bool   `json:"-"` // nodeID -> proxyID -> earning=yes/no
}

type nodeRate struct {
	ts  time.Time
	rx  uint64
	tx  uint64
	mbpsRx float64
	mbpsTx float64
}

// openStore opens the SQLite-backed hub store in dataDir. It creates/opens
// hub.db, rebuilds the in-memory cache from the latest stored snapshots, and
// performs a one-time migration of a legacy hub.json if one is present (after
// which the JSON file is retired to hub.json.imported).
func openStore(dataDir string) (*store, error) {
	dbPath := filepath.Join(dataDir, "hub.db")
	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}
	s := &store{
		db:           db,
		Nodes:        make(map[string]*nodeState),
		rates:        make(map[string]*nodeRate),
		prevBillable: make(map[string]map[string]uint64),
		earning:      make(map[string]map[string]bool),
	}

	jsonPath := filepath.Join(dataDir, "hub.json")
	if _, err := os.Stat(jsonPath); err == nil {
		n, err := s.importJSON(jsonPath)
		if err != nil {
			fmt.Printf("hub.json import failed (continuing): %v\n", err)
		} else {
			fmt.Printf("migrated %d nodes from hub.json into hub.db\n", n)
			if err := os.Rename(jsonPath, jsonPath+".imported"); err != nil {
				fmt.Printf("could not retire hub.json: %v\n", err)
			}
		}
	}

	if err := s.loadLatestFromDB(); err != nil {
		fmt.Printf("warning: could not load cached state from hub.db: %v\n", err)
	}
	return s, nil
}

func (s *store) upsert(nodeID string, state *nodeState) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// compute rate from previous snapshot
	var totalRX, totalTX uint64
	for _, p := range state.Proxies {
		totalRX += p.TotalRX
		totalTX += p.TotalTX
	}
	if prev, ok := s.rates[nodeID]; ok {
		dt := state.Timestamp.Sub(prev.ts).Seconds()
		if dt > 1 && totalRX >= prev.rx && totalTX >= prev.tx {
			s.rates[nodeID].mbpsRx = float64(totalRX-prev.rx) / dt * 8 / 1_000_000
			s.rates[nodeID].mbpsTx = float64(totalTX-prev.tx) / dt * 8 / 1_000_000
		}
		s.rates[nodeID].ts = state.Timestamp
		s.rates[nodeID].rx = totalRX
		s.rates[nodeID].tx = totalTX
	} else {
		s.rates[nodeID] = &nodeRate{ts: state.Timestamp, rx: totalRX, tx: totalTX}
	}

	// earning=yes mirrors the provider's own [traffic] log line: billable bytes
	// must have grown since the last report from this proxy (active within the
	// report interval) and it must currently be carrying client sessions. A
	// proxy with no prior snapshot can't have a known delta yet, so it reads no
	// until the next report.
	if s.prevBillable == nil {
		s.prevBillable = make(map[string]map[string]uint64)
	}
	if s.earning == nil {
		s.earning = make(map[string]map[string]bool)
	}
	prevBill := s.prevBillable[nodeID]
	if prevBill == nil {
		prevBill = make(map[string]uint64)
	}
	earning := make(map[string]bool, len(state.Proxies))
	nextBill := make(map[string]uint64, len(state.Proxies))
	for _, p := range state.Proxies {
		billable := p.BillRX + p.BillTX
		_, seen := prevBill[p.ID]
		earning[p.ID] = seen && billable > prevBill[p.ID] && p.Clients > 0
		nextBill[p.ID] = billable
	}
	s.prevBillable[nodeID] = nextBill
	s.earning[nodeID] = earning

	s.Nodes[nodeID] = state
	if err := s.persist(state); err != nil {
		fmt.Printf("persist %s: %v\n", nodeID, err)
	}
}

func (s *store) list() []*nodeState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*nodeState, 0, len(s.Nodes))
	for _, n := range s.Nodes {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Timestamp.After(out[j].Timestamp)
	})
	return out
}

func (s *store) getRate(nodeID string) (float64, float64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if r, ok := s.rates[nodeID]; ok {
		return r.mbpsRx, r.mbpsTx
	}
	return 0, 0
}

// getEarning returns the per-proxy earning=yes/no map for a node, computed by
// upsert from the billable-bytes delta against the previous report.
func (s *store) getEarning(nodeID string) map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.earning[nodeID]
}

func (s *store) summary() summaryRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sr summaryRow
	now := time.Now()
	for _, n := range s.Nodes {
		if now.Sub(n.Timestamp) > staleCutoff {
			continue
		}
		sr.Nodes++
		nodeEarning := s.earning[n.NodeID]
		for _, p := range n.Proxies {
			switch p.Status {
			case "up":
				sr.Up++
			case "connecting":
				sr.Connecting++
			case "degraded":
				sr.Degraded++
			default:
				sr.Dead++
			}
			sr.TotalClients += p.Clients
			sr.TotalRX += p.TotalRX
			sr.TotalTX += p.TotalTX
			sr.BillRX += p.BillRX
			sr.BillTX += p.BillTX
			sr.TotalProxies++
			if nodeEarning[p.ID] {
				sr.Earning++
			}
		}
	}
	return sr
}

type summaryRow struct {
	Nodes, Up, Connecting, Degraded, Dead int
	TotalClients                           int64
	TotalRX, TotalTX                       uint64
	BillRX, BillTX                         uint64
	Earning, TotalProxies                  int
}

type proxSummary struct {
	Up, Connecting, Degraded, Dead int
	Clients                        int64
	TotalRX, TotalTX               uint64
	BillRX, BillTX                 uint64
	Earning                        int
}

// proxyRow pairs a reported proxy with its hub-computed earning state for
// template rendering. proxyReport itself stays a plain decode/persist target.
type proxyRow struct {
	proxyReport
	Earning bool
}

type nodeRow struct {
	NodeID        string
	Host          string
	Version       string
	Heartbeat     string
	Color         string
	Uptime        string
	Proxies       proxSummary
	MbpsRX        float64
	MbpsTX        float64
	HeapMiB       uint64
	SysMiB        uint64
	Conns         int64
	ProxyList     []proxyRow
	Index         int
}

func fmtBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := float64(unit), 0
	for n := float64(b) / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(b)/div, "KMGTPE"[exp])
}

func fmtMbps(v float64) string {
	if v < 0.01 {
		return "—"
	}
	if v < 1 {
		return fmt.Sprintf("%.0f Kbps", v*1000)
	}
	if v < 100 {
		return fmt.Sprintf("%.1f Mbps", v)
	}
	return fmt.Sprintf("%.0f Mbps", v)
}

func title(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-32) + s[1:]
}

func fmtAge(seconds int64) string {
	if seconds <= 0 {
		return "—"
	}
	if seconds < 60 {
		return fmt.Sprintf("%ds", seconds)
	}
	if seconds < 3600 {
		return fmt.Sprintf("%dm", seconds/60)
	}
	return fmt.Sprintf("%dh", seconds/3600)
}

// Freshness thresholds, sized for the provider's 5m default report interval
// (one missed report still reads green; sustained silence goes yellow then
// red). staleCutoff also gates which nodes count toward the fleet summary.
const (
	freshWindow = 7 * time.Minute
	staleWindow = 15 * time.Minute
	staleCutoff = staleWindow
)

func nodeColor(ts time.Time) string {
	d := time.Since(ts)
	if d < freshWindow {
		return "#22c55e"
	}
	if d < staleWindow {
		return "#eab308"
	}
	return "#ef4444"
}

func handleReport(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		var ns nodeState
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := json.Unmarshal(body, &ns); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if ns.NodeID == "" {
			http.Error(w, "missing node_id", 400)
			return
		}
		ns.Timestamp = time.Now().UTC()
		fmt.Printf("report from %s: %d proxies\n", ns.NodeID, len(ns.Proxies))
		s.upsert(ns.NodeID, &ns)
		w.WriteHeader(204)
	}
}

func handleNodes(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.list())
	}
}

// handleHistory serves the hourly rollups stored in SQLite as JSON. Query
// params: node (optional node_id filter) and hours (lookback window, default
// 24). Example: /api/history?node=la6&hours=168
func handleHistory(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.URL.Query().Get("node")
		hours := 24
		if h := r.URL.Query().Get("hours"); h != "" {
			if v, err := strconv.Atoi(h); err == nil && v > 0 {
				hours = v
			}
		}
		rows, err := s.history(nodeID, hours)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rows)
	}
}

type removeRequest struct {
	NodeID string `json:"node_id"`
}

func handleNodeRemove(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		var req removeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if req.NodeID == "" {
			http.Error(w, "missing node_id", 400)
			return
		}
		s.mu.Lock()
		delete(s.Nodes, req.NodeID)
		delete(s.rates, req.NodeID)
		delete(s.prevBillable, req.NodeID)
		delete(s.earning, req.NodeID)
		s.mu.Unlock()
		if err := s.deleteFromDB(req.NodeID); err != nil {
			fmt.Printf("delete %s from db: %v\n", req.NodeID, err)
		}
		fmt.Printf("removed node %s\n", req.NodeID)
		w.WriteHeader(204)
	}
}

func handleDashboard(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nodes := s.list()

		rows := make([]nodeRow, 0, len(nodes))
		for i, n := range nodes {
			nodeEarning := s.getEarning(n.NodeID)
			var ps proxSummary
			proxyList := make([]proxyRow, 0, len(n.Proxies))
			for _, p := range n.Proxies {
				ps.TotalRX += p.TotalRX
				ps.TotalTX += p.TotalTX
				ps.BillRX += p.BillRX
				ps.BillTX += p.BillTX
				ps.Clients += p.Clients
				switch p.Status {
				case "up":
					ps.Up++
				case "connecting":
					ps.Connecting++
				case "degraded":
					ps.Degraded++
				default:
					ps.Dead++
				}
				isEarning := nodeEarning[p.ID]
				if isEarning {
					ps.Earning++
				}
				proxyList = append(proxyList, proxyRow{proxyReport: p, Earning: isEarning})
			}
			uptime := time.Duration(n.Uptime * float64(time.Second)).Round(time.Second)
			uptimeStr := uptime.String()
			if uptime.Hours() > 24 {
				uptimeStr = fmt.Sprintf("%dd %dh", int(uptime.Hours()/24), int(uptime.Hours())%24)
			}

			ago := time.Since(n.Timestamp).Round(time.Second)
			agoStr := "just now"
			if ago > 10*time.Second {
				if ago.Minutes() < 1 {
					agoStr = fmt.Sprintf("%ds ago", int(ago.Seconds()))
				} else {
					agoStr = fmt.Sprintf("%dm ago", int(ago.Minutes()))
				}
			}

			mbpsRX, mbpsTX := s.getRate(n.NodeID)

			rows = append(rows, nodeRow{
				NodeID:    n.NodeID,
				Host:      n.Host,
				Version:   n.Version,
				Heartbeat: agoStr,
				Color:     nodeColor(n.Timestamp),
				Uptime:    uptimeStr,
				Proxies:   ps,
				MbpsRX:    mbpsRX,
				MbpsTX:    mbpsTX,
				HeapMiB:   n.System.HeapMiB,
				SysMiB:    n.System.SysMiB,
				Conns:     n.System.Connections,
				ProxyList: proxyList,
				Index:     i,
			})
		}

		sm := s.summary()

		var buf bytes.Buffer
		tmpl.Execute(&buf, map[string]interface{}{
			"Rows": rows,
			"Sum":  sm,
		})
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		buf.WriteTo(w)
	}
}

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dataDir := flag.String("data", ".", "data directory for hub.json")
	flag.Parse()

	s, err := openStore(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hub: open store: %v\n", err)
		os.Exit(1)
	}
	defer s.db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.startRetention(ctx)

	mux := http.NewServeMux()

	mux.HandleFunc("/api/report", handleReport(s))
	mux.HandleFunc("/api/nodes", handleNodes(s))
	mux.HandleFunc("/api/nodes/remove", handleNodeRemove(s))
	mux.HandleFunc("/api/history", handleHistory(s))
	mux.HandleFunc("/", handleDashboard(s))

	fmt.Printf("hub listening on %s (data: %s)\n", *addr, filepath.Join(*dataDir, "hub.db"))
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "hub: %v\n", err)
		os.Exit(1)
	}
}

var tmpl = template.Must(template.New("dashboard").Funcs(funcMap).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>URnetwork Hub</title>
<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/uplot@1.6.31/dist/uPlot.min.css">
<script src="https://cdn.jsdelivr.net/npm/uplot@1.6.31/dist/uPlot.iife.min.js"></script>
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; background: #0b1120; color: #e2e8f0; padding: 0; }
.header { background: linear-gradient(135deg, #0f172a 0%, #1a2332 100%); border-bottom: 1px solid #1e293b; padding: 16px 24px; }
.header h1 { font-size: 18px; font-weight: 600; color: #f1f5f9; }
.header h1 small { color: #64748b; font-weight: 400; font-size: 12px; margin-left: 8px; }
.cards { display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: 12px; margin-top: 12px; }
.card { background: #0f172a; border-radius: 8px; padding: 12px 16px; border: 1px solid #1e293b; }
.card .label { font-size: 11px; text-transform: uppercase; letter-spacing: 0.05em; color: #64748b; margin-bottom: 4px; }
.card .value { font-size: 22px; font-weight: 700; font-variant-numeric: tabular-nums; }
.card-up .value { color: #4ade80; }
.card-degraded .value { color: #facc15; }
.card-dead .value { color: #ef4444; }
.card-earn .value { color: #60a5fa; }
.card-clients .value { color: #a78bfa; }
.card-proxies .value { color: #e2e8f0; }
.card .sub { font-size: 12px; color: #64748b; margin-top: 2px; }
.tabs { display: flex; gap: 0; border-bottom: 1px solid #1e293b; background: #0f172a; padding: 0 24px; position: sticky; top: 0; z-index: 10; }
.tab { padding: 10px 20px; cursor: pointer; color: #64748b; font-size: 13px; font-weight: 500; border-bottom: 2px solid transparent; user-select: none; }
.tab:hover { color: #94a3b8; }
.tab.active { color: #60a5fa; border-bottom-color: #60a5fa; }
.tab-content { display: none; }
.tab-content.active { display: block; }
.filter-bar { display: flex; gap: 12px; align-items: center; padding: 12px 24px; background: #0f172a; border-bottom: 1px solid #1e293b; flex-wrap: wrap; }
.filter-bar input, .filter-bar select { background: #1a2332; border: 1px solid #1e293b; color: #e2e8f0; padding: 6px 12px; border-radius: 6px; font-size: 13px; outline: none; }
.filter-bar input:focus, .filter-bar select:focus { border-color: #60a5fa; }
.filter-bar input { flex: 1; min-width: 200px; }
.filter-bar .info { font-size: 12px; color: #64748b; margin-left: auto; }
.table-wrap { padding: 0; overflow-x: auto; }
table { width: 100%; border-collapse: collapse; font-size: 13px; }
th { text-align: left; padding: 10px 8px; border-bottom: 1px solid #1e293b; color: #64748b; font-weight: 600; white-space: nowrap; cursor: pointer; user-select: none; background: #0b1120; }
th:hover { color: #94a3b8; }
th.sorted { color: #60a5fa; }
td { padding: 10px 8px; border-bottom: 1px solid #1e293b; }
tr.expandable { cursor: pointer; }
tr.expandable:hover { background: #1a2332; }
.num { text-align: right; font-variant-numeric: tabular-nums; }
.num-mono { text-align: right; font-family: "SF Mono", Monaco, Consolas, monospace; font-variant-numeric: tabular-nums; }
.node-id { font-weight: 600; color: #e2e8f0; }
.version { font-size: 11px; color: #64748b; font-weight: 400; margin-left: 6px; }
.dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; margin-right: 8px; transition: opacity 0.3s; }
.dot.alive { animation: pulse 2s infinite; }
@keyframes pulse { 0%, 100% { opacity: 1; } 50% { opacity: 0.4; } }
.status-badge { display: inline-block; padding: 1px 6px; border-radius: 4px; font-size: 11px; font-weight: 600; }
.status-badge.up { background: rgba(74,222,128,0.15); color: #4ade80; }
.status-badge.connecting { background: rgba(96,165,250,0.15); color: #60a5fa; }
.status-badge.degraded { background: rgba(250,204,21,0.15); color: #facc15; }
.status-badge.dead { background: rgba(239,68,68,0.15); color: #ef4444; }
.sort-arrow { margin-left: 4px; font-size: 10px; }
.remove-btn { cursor: pointer; padding: 2px 6px; border-radius: 4px; font-size: 16px; line-height: 1; color: #64748b; }
.remove-btn:hover { background: rgba(239,68,68,0.2); color: #ef4444; }
.hidden { display: none !important; }
.drawer-overlay { position: fixed; top: 0; left: 0; right: 0; bottom: 0; background: rgba(0,0,0,0.5); z-index: 100; display: none; }
.drawer-overlay.open { display: block; }
.drawer { position: fixed; top: 0; right: -600px; width: 600px; max-width: 100vw; height: 100%; background: #0f172a; border-left: 1px solid #1e293b; z-index: 101; transition: right 0.25s ease; overflow-y: auto; display: flex; flex-direction: column; }
.drawer.open { right: 0; }
.drawer-header { display: flex; justify-content: space-between; align-items: center; padding: 16px 20px; border-bottom: 1px solid #1e293b; }
.drawer-header h2 { font-size: 15px; font-weight: 600; }
.drawer-close { cursor: pointer; font-size: 20px; color: #64748b; padding: 4px 8px; border-radius: 4px; }
.drawer-close:hover { background: #1e293b; color: #e2e8f0; }
.drawer-body { flex: 1; padding: 16px 20px; overflow-y: auto; }
.drawer-body .loading { text-align: center; padding: 40px; color: #64748b; }
.drawer-body table { font-size: 12px; }
.drawer-body th { padding: 6px 8px; background: #0f172a; }
.drawer-body td { padding: 4px 8px; }
.proxy-status { display: inline-block; width: 6px; height: 6px; border-radius: 50%; margin-right: 6px; }
.proxy-status.up { background: #4ade80; }
.proxy-status.degraded { background: #facc15; }
.proxy-status.dead { background: #ef4444; }
.proxy-status.connecting { background: #60a5fa; }
.chart-wrap { padding: 20px 24px; }
.chart-controls { display: flex; gap: 8px; margin-bottom: 12px; flex-wrap: wrap; align-items: center; }
.chart-controls button { background: #1a2332; border: 1px solid #1e293b; color: #94a3b8; padding: 4px 12px; border-radius: 6px; cursor: pointer; font-size: 12px; }
.chart-controls button:hover { background: #1e293b; color: #e2e8f0; }
.chart-controls button.active { background: #60a5fa; color: #fff; border-color: #60a5fa; }
.chart-box { background: #0f172a; border-radius: 8px; border: 1px solid #1e293b; padding: 16px; }
.chart-box.compact { padding: 8px; }
.chart-box.compact .u-title { font-size: 12px !important; }
.footer { display: flex; justify-content: space-between; align-items: center; padding: 12px 24px; border-top: 1px solid #1e293b; font-size: 13px; color: #64748b; background: #0f172a; }
.footer a { color: #60a5fa; text-decoration: none; }
.footer a:hover { text-decoration: underline; }
.auto-refresh { display: inline-flex; align-items: center; gap: 6px; }
.auto-refresh input { accent-color: #60a5fa; }
.countdown { margin-left: 8px; font-variant-numeric: tabular-nums; }
@media (max-width: 900px) {
  .header { padding: 12px 16px; }
  .cards { grid-template-columns: repeat(2, 1fr); gap: 8px; }
  .tabs { padding: 0 16px; }
  .filter-bar { padding: 8px 16px; }
  .chart-wrap { padding: 12px 16px; }
  .footer { padding: 10px 16px; flex-direction: column; gap: 6px; text-align: center; }
  .drawer { width: 100vw; right: -100vw; }
}
</style>
</head>
<body>
<div class="header">
<h1>URnetwork Hub <small>fleet bandwidth dashboard</small></h1>
<div class="cards">
<div class="card card-proxies"><div class="label">Total Proxies</div><div class="value">{{.Sum.TotalProxies}}</div><div class="sub">across {{.Sum.Nodes}} nodes</div></div>
<div class="card card-up"><div class="label">Healthy</div><div class="value">{{.Sum.Up}}</div><div class="sub">{{printf "%.1f" (pct .Sum.Up .Sum.TotalProxies)}}% of fleet</div></div>
<div class="card card-degraded"><div class="label">Degraded</div><div class="value">{{.Sum.Degraded}}</div><div class="sub">{{.Sum.Dead}} dead</div></div>
<div class="card card-earn"><div class="label">Earning</div><div class="value">{{printf "%.1f" (pct .Sum.Earning .Sum.TotalProxies)}}%</div><div class="sub">{{.Sum.Earning}} / {{.Sum.Up}} up proxies</div></div>
<div class="card card-clients"><div class="label">Active Clients</div><div class="value">{{.Sum.TotalClients}}</div><div class="sub">{{printf "%s" (fmtBytes .Sum.TotalRX)}} RX / {{printf "%s" (fmtBytes .Sum.TotalTX)}} TX</div></div>
</div>
</div>
<div class="tabs">
<div class="tab active" onclick="switchTab('nodes')">Nodes</div>
<div class="tab" onclick="switchTab('history')">History</div>
</div>
<div id="tab-nodes" class="tab-content active">
<div class="chart-wrap" style="padding-bottom:8px">
<div class="chart-box compact"><div id="fleet-chart" style="height:120px"></div></div>
</div>
<div class="filter-bar">
<input type="text" id="filter-input" placeholder="Filter nodes..." oninput="applyFilter()">
<select id="filter-status" onchange="applyFilter()">
<option value="all">All statuses</option>
<option value="up">Healthy</option>
<option value="degraded">Degraded</option>
<option value="dead">Dead</option>
</select>
<span class="info" id="filter-count">All {{.Sum.Nodes}} nodes</span>
</div>
<div class="table-wrap">
<table id="node-table">
<thead>
<tr>
<th data-col="node" onclick="sortBy('node')">Node <span class="sort-arrow"></span></th>
<th data-col="heartbeat" onclick="sortBy('heartbeat')">Heartbeat <span class="sort-arrow"></span></th>
<th data-col="uptime" onclick="sortBy('uptime')">Uptime <span class="sort-arrow"></span></th>
<th data-col="proxies" class="num" onclick="sortBy('proxies')">Proxies <span class="sort-arrow"></span></th>
<th data-col="clients" class="num" onclick="sortBy('clients')">Clients <span class="sort-arrow"></span></th>
<th data-col="rx" class="num" onclick="sortBy('rx')">RX <span class="sort-arrow"></span></th>
<th data-col="tx" class="num" onclick="sortBy('tx')">TX <span class="sort-arrow"></span></th>
<th data-col="billrx" class="num" onclick="sortBy('billrx')">Bill RX <span class="sort-arrow"></span></th>
<th data-col="billtx" class="num" onclick="sortBy('billtx')">Bill TX <span class="sort-arrow"></span></th>
<th data-col="rate-rx" class="num" onclick="sortBy('rate-rx')">In Mbps <span class="sort-arrow"></span></th>
<th data-col="rate-tx" class="num" onclick="sortBy('rate-tx')">Out Mbps <span class="sort-arrow"></span></th>
<th data-col="earning" class="num" onclick="sortBy('earning')">Earning <span class="sort-arrow"></span></th>
<th></th>
</tr>
</thead>
<tbody>
{{range .Rows}}
<tr class="expandable" data-id="{{.NodeID}}" data-status="{{if .Proxies.Dead}}dead{{else if .Proxies.Degraded}}degraded{{else}}up{{end}}" onclick="openDrawer('{{.NodeID}}')">
<td class="node-id"><span class="dot{{if .Proxies.Up}} alive{{end}}" style="background:{{.Color}}"></span>{{.NodeID}} <span class="version">{{.Version}}</span></td>
<td>{{.Heartbeat}}</td>
<td>{{.Uptime}}</td>
<td class="num">{{.Proxies.Up}}{{if .Proxies.Degraded}} <span class="status-badge degraded">{{.Proxies.Degraded}}</span>{{end}}{{if .Proxies.Dead}} <span class="status-badge dead">{{.Proxies.Dead}}</span>{{end}}</td>
<td class="num">{{.Proxies.Clients}}</td>
<td class="num">{{fmtBytes .Proxies.TotalRX}}</td>
<td class="num">{{fmtBytes .Proxies.TotalTX}}</td>
<td class="num">{{fmtBytes .Proxies.BillRX}}</td>
<td class="num">{{fmtBytes .Proxies.BillTX}}</td>
<td class="num">{{printf "%.1f" .MbpsRX}}</td>
<td class="num">{{printf "%.1f" .MbpsTX}}</td>
<td class="num">{{.Proxies.Earning}}/{{.Proxies.Up}}</td>
<td><span class="remove-btn" onclick="event.stopPropagation();removeNode('{{.NodeID}}')" title="Remove node">&#10005;</span></td>
</tr>
{{end}}
</tbody>
</table>
</div>
</div>
<div id="tab-history" class="tab-content">
<div class="chart-wrap">
<div class="chart-controls">
<button class="active" onclick="setHistoryRange(24,this)">24h</button>
<button onclick="setHistoryRange(72,this)">3d</button>
<button onclick="setHistoryRange(168,this)">7d</button>
<button onclick="resetHistoryZoom()" style="margin-left:4px;color:#64748b;font-size:11px">Reset zoom</button>
<select id="history-node" onchange="loadHistory()" style="background:#1a2332;border:1px solid #1e293b;color:#e2e8f0;padding:4px 8px;border-radius:6px;font-size:12px;margin-left:auto">
<option value="">All nodes</option>
{{range .Rows}}
<option value="{{.NodeID}}">{{.NodeID}}</option>
{{end}}
</select>
</div>
<div class="chart-box"><div id="history-chart"></div></div>
</div>
</div>
<div class="drawer-overlay" id="drawer-overlay" onclick="closeDrawer()"></div>
<div class="drawer" id="drawer">
<div class="drawer-header">
<h2 id="drawer-title">Node</h2>
<span class="drawer-close" onclick="closeDrawer()">&#10005;</span>
</div>
<div class="drawer-body" id="drawer-body">
<div class="loading">Select a node to view proxy details</div>
</div>
</div>
<div class="footer">
<div class="auto-refresh">
<input type="checkbox" id="auto-refresh" checked onchange="toggleRefresh()">
<label for="auto-refresh">auto-refresh</label>
<span class="countdown" id="countdown">30s</span>
</div>
<div>
<a href="/api/nodes">/api/nodes (JSON)</a>
</div>
</div>
<script>
var fleetChart = null;

function switchTab(name) {
  document.querySelectorAll('.tab').forEach(function(t) { t.classList.remove('active'); });
  document.querySelectorAll('.tab-content').forEach(function(t) { t.classList.remove('active'); });
  document.querySelectorAll('.tab')[name === 'nodes' ? 0 : 1].classList.add('active');
  document.getElementById('tab-' + name).classList.add('active');
  if (name === 'history') loadHistory();
  if (name === 'nodes') loadFleetChart();
}

function loadFleetChart() {
  if (fleetChart) return; // already loaded
  fetch('/api/history?hours=24').then(function(r) { return r.json(); }).then(function(data) {
    if (!data || data.length === 0) return;
    var byHour = {};
    for (var i = 0; i < data.length; i++) {
      var h = data[i];
      if (!byHour[h.hour]) byHour[h.hour] = { rx: 0, tx: 0 };
      byHour[h.hour].rx += h.total_rx;
      byHour[h.hour].tx += h.total_tx;
    }
    var hours = Object.keys(byHour).sort();
    var labels = [], rx = [], tx = [];
    hours.forEach(function(h) {
      labels.push(new Date(parseInt(h) * 1000));
      rx.push(byHour[h].rx);
      tx.push(byHour[h].tx);
    });
    var opts = {
      width: document.getElementById('fleet-chart').clientWidth || 800,
      height: 120,
      cursor: { show: false },
      legend: { show: true },
      axes: [{ show: false }, { show: false }],
      series: [
        {},
        { label: 'RX', stroke: '#60a5fa', fill: 'rgba(96,165,250,0.1)', width: 1.5, points: { show: false } },
        { label: 'TX', stroke: '#4ade80', fill: 'rgba(74,222,128,0.1)', width: 1.5, points: { show: false } }
      ]
    };
    fleetChart = new uPlot(opts, [labels, rx, tx], document.getElementById('fleet-chart'));
  }).catch(function() {});
}
function applyFilter() {
  var q = document.getElementById('filter-input').value.toLowerCase();
  var status = document.getElementById('filter-status').value;
  var visible = 0;
  document.querySelectorAll('#node-table tbody tr.expandable').forEach(function(r) {
    var match = r.getAttribute('data-id').toLowerCase().indexOf(q) >= 0;
    if (status !== 'all' && r.getAttribute('data-status') !== status) match = false;
    r.classList.toggle('hidden', !match);
    if (match) visible++;
  });
  document.getElementById('filter-count').textContent = visible + ' / ' + document.querySelectorAll('#node-table tbody tr.expandable').length + ' nodes';
}
var drawerNodeId = null;
function openDrawer(id) {
  drawerNodeId = id;
  document.getElementById('drawer-overlay').classList.add('open');
  document.getElementById('drawer').classList.add('open');
  document.getElementById('drawer-title').textContent = 'Node: ' + id;
  document.getElementById('drawer-body').innerHTML = '<div class="loading">Loading proxies...</div>';
  fetch('/api/nodes/' + id + '/proxies').then(function(r) { return r.json(); }).then(function(proxies) {
    if (!proxies || proxies.length === 0) {
      document.getElementById('drawer-body').innerHTML = '<div class="loading">No proxy data</div>';
      return;
    }
    var html = '<table><thead><tr><th>ID</th><th>Address</th><th>Status</th><th class="num">Clients</th><th class="num">Age</th><th class="num">RX</th><th class="num">TX</th><th class="num">Bill RX</th><th class="num">Bill TX</th></tr></thead><tbody>';
    proxies.forEach(function(p) {
      html += '<tr><td class="num-mono">' + p.id + '</td><td class="truncate">' + p.addr + '</td><td><span class="proxy-status ' + p.status + '"></span>' + p.status + '</td><td class="num">' + p.clients + '</td><td class="num">' + fmtAge(p.max_age_s) + '</td><td class="num">' + fmtBytes(p.rx) + '</td><td class="num">' + fmtBytes(p.tx) + '</td><td class="num">' + fmtBytes(p.bill_rx) + '</td><td class="num">' + fmtBytes(p.bill_tx) + '</td></tr>';
    });
    html += '</tbody></table>';
    document.getElementById('drawer-body').innerHTML = html;
  }).catch(function() {
    document.getElementById('drawer-body').innerHTML = '<div class="loading">Failed to load proxies</div>';
  });
}
function closeDrawer() {
  document.getElementById('drawer-overlay').classList.remove('open');
  document.getElementById('drawer').classList.remove('open');
}
var secondsLeft = 30;
var countdownEl = document.getElementById('countdown');
var autoRefresh = document.getElementById('auto-refresh');
function tick() {
  if (!autoRefresh.checked) { countdownEl.textContent = 'paused'; return; }
  secondsLeft--;
  if (secondsLeft <= 0) { secondsLeft = 30; refreshDashboard(); return; }
  countdownEl.textContent = secondsLeft + 's';
}
setInterval(tick, 1000);
countdownEl.textContent = '30s';
function toggleRefresh() { if (autoRefresh.checked) secondsLeft = 30; }
var refreshing = false;
function refreshDashboard() {
  if (refreshing) return;
  refreshing = true;
  fetch('/api/nodes').then(function(r) { return r.json(); }).then(function(nodes) {
    var tbody = document.querySelector('#node-table tbody');
    var existing = {};
    tbody.querySelectorAll('tr.expandable').forEach(function(r) { existing[r.getAttribute('data-id')] = r; });
    var frag = document.createDocumentFragment();
    var totalProxies = 0, totalUp = 0, totalDeg = 0, totalDead = 0, totalClients = 0, totalEarning = 0, nodeCount = 0, totalRX = 0, totalTX = 0;
    nodes.forEach(function(n) {
      nodeCount++; totalProxies += n.proxies; totalUp += n.up; totalDeg += n.degraded; totalDead += n.dead; totalClients += n.clients; totalEarning += n.earning; totalRX += n.rx; totalTX += n.tx;
      var ago = fmtAgo(n.ts), uptime = fmtUptime(n.uptime), color = n.ts ? nodeColor(n.ts) : '#ef4444';
      var sc = n.dead > 0 ? 'dead' : (n.degraded > 0 ? 'degraded' : 'up');
      var existingRow = existing[n.node_id];
      if (existingRow) {
        var c = existingRow.cells;
        c[0].innerHTML = '<span class="dot' + (n.up > 0 ? ' alive' : '') + '" style="background:' + color + '"></span>' + n.node_id + ' <span class="version">' + (n.sys.host||'') + '</span>';
        c[1].textContent = ago; c[2].textContent = uptime;
        c[3].innerHTML = n.up + (n.degraded > 0 ? ' <span class="status-badge degraded">' + n.degraded + '</span>' : '') + (n.dead > 0 ? ' <span class="status-badge dead">' + n.dead + '</span>' : '');
        c[4].textContent = n.clients; c[5].textContent = fmtBytes(n.rx); c[6].textContent = fmtBytes(n.tx);
        c[7].textContent = fmtBytes(n.bill_rx); c[8].textContent = fmtBytes(n.bill_tx);
        c[9].textContent = n.mbps_rx ? n.mbps_rx.toFixed(1) : ''; c[10].textContent = n.mbps_tx ? n.mbps_tx.toFixed(1) : '';
        c[11].textContent = n.earning + '/' + n.up;
        existingRow.setAttribute('data-status', sc);
      } else {
        var tr = document.createElement('tr'); tr.className = 'expandable'; tr.setAttribute('data-id', n.node_id); tr.setAttribute('data-status', sc);
        tr.onclick = function() { openDrawer(n.node_id); };
        tr.innerHTML = '<td class="node-id"><span class="dot' + (n.up > 0 ? ' alive' : '') + '" style="background:' + color + '"></span>' + n.node_id + ' <span class="version">' + (n.sys.host||'') + '</span></td><td>' + ago + '</td><td>' + uptime + '</td><td class="num">' + n.up + (n.degraded > 0 ? ' <span class="status-badge degraded">' + n.degraded + '</span>' : '') + (n.dead > 0 ? ' <span class="status-badge dead">' + n.dead + '</span>' : '') + '</td><td class="num">' + n.clients + '</td><td class="num">' + fmtBytes(n.rx) + '</td><td class="num">' + fmtBytes(n.tx) + '</td><td class="num">' + fmtBytes(n.bill_rx) + '</td><td class="num">' + fmtBytes(n.bill_tx) + '</td><td class="num">' + (n.mbps_rx ? n.mbps_rx.toFixed(1) : '') + '</td><td class="num">' + (n.mbps_tx ? n.mbps_tx.toFixed(1) : '') + '</td><td class="num">' + n.earning + '/' + n.up + '</td><td><span class="remove-btn" onclick="event.stopPropagation();removeNode(\'' + n.node_id + '\')">&#10005;</span></td>';
        frag.appendChild(tr);
      }
    });
    tbody.appendChild(frag);
    document.querySelectorAll('.card')[0].innerHTML = '<div class="label">Total Proxies</div><div class="value">' + totalProxies + '</div><div class="sub">across ' + nodeCount + ' nodes</div>';
    document.querySelectorAll('.card')[1].innerHTML = '<div class="label">Healthy</div><div class="value">' + totalUp + '</div><div class="sub">' + (totalProxies > 0 ? (totalUp/totalProxies*100).toFixed(1) : '0') + '% of fleet</div>';
    document.querySelectorAll('.card')[2].innerHTML = '<div class="label">Degraded</div><div class="value">' + totalDeg + '</div><div class="sub">' + totalDead + ' dead</div>';
    document.querySelectorAll('.card')[3].innerHTML = '<div class="label">Earning</div><div class="value">' + (totalUp > 0 ? (totalEarning/totalUp*100).toFixed(1) : '0') + '%</div><div class="sub">' + totalEarning + ' / ' + totalUp + ' up</div>';
    document.querySelectorAll('.card')[4].innerHTML = '<div class="label">Active Clients</div><div class="value">' + totalClients + '</div><div class="sub">' + fmtBytes(totalRX) + ' RX / ' + fmtBytes(totalTX) + ' TX</div>';
    applyFilter();
  }).catch(function() {}).then(function() { refreshing = false; });
}
var historyChart = null;
var historyHours = 24;
function setHistoryRange(hours, btn) {
  historyHours = hours;
  document.querySelectorAll('.chart-controls button').forEach(function(b) { b.classList.remove('active'); });
  if (btn) btn.classList.add('active');
  loadHistory();
}
function resetHistoryZoom() {
  if (historyChart) historyChart.setScale('x', { min: null, max: null });
}
function loadHistory() {
  var nodeId = document.getElementById('history-node').value;
  fetch('/api/history?hours=' + historyHours + (nodeId ? '&node=' + nodeId : '')).then(function(r) { return r.json(); }).then(function(data) {
    if (!data || data.length === 0) {
      document.getElementById('history-chart').innerHTML = '<div style="text-align:center;padding:40px;color:#64748b">No history data</div>';
      return;
    }
    var rx = [], tx = [], labels = [];
    for (var i = data.length - 1; i >= 0; i--) {
      labels.push(new Date(data[i].hour * 1000));
      rx.push(data[i].total_rx);
      tx.push(data[i].total_tx);
    }
    var opts = {
      width: Math.min(document.getElementById('history-chart').clientWidth || 800, 1200),
      height: 300,
      cursor: { show: true },
      legend: { show: true },
      axes: [
        { stroke: '#64748b', grid: { stroke: '#1e293b', width: 1 } },
        { stroke: '#64748b', grid: { stroke: '#1e293b', width: 1 }, label: 'Bytes' }
      ],
      series: [
        { label: 'Time', value: '{HH}:{mm}' },
        { label: 'RX', stroke: '#60a5fa', fill: 'rgba(96,165,250,0.1)', width: 2 },
        { label: 'TX', stroke: '#4ade80', fill: 'rgba(74,222,128,0.1)', width: 2 }
      ]
    };
    if (historyChart) { historyChart.destroy(); historyChart = null; }
    historyChart = new uPlot(opts, [labels, rx, tx], document.getElementById('history-chart'));
  }).catch(function() {});
}
function fmtAgo(ts) {
  if (!ts) return 'never';
  var d = (Date.now() - new Date(ts).getTime()) / 1000;
  if (d < 10) return 'now';
  if (d < 60) return Math.round(d) + 's';
  if (d < 3600) return Math.round(d/60) + 'm';
  return Math.round(d/3600) + 'h';
}
function fmtUptime(s) {
  if (!s) return '0s';
  var h = Math.floor(s / 3600);
  var d = Math.floor(h / 24);
  if (d > 0) return d + 'd ' + (h % 24) + 'h';
  if (h > 0) return h + 'h';
  return Math.floor(s / 60) + 'm';
}
function nodeColor(ts) {
  var age = (Date.now() - new Date(ts).getTime()) / 1000;
  if (age < 420) return '#22c55e';
  if (age < 900) return '#eab308';
  return '#ef4444';
}
function fmtBytes(b) {
  if (b < 1024) return b + ' B';
  var units = 'KMGTPE';
  var i = -1;
  var n = b;
  while (n >= 1024 && i < units.length-1) { n /= 1024; i++; }
  return n.toFixed(1) + ' ' + units[i] + 'B';
}
function fmtAge(s) {
  if (!s || s === 0) return '&mdash;';
  if (s < 60) return s + 's';
  if (s < 3600) return Math.round(s/60) + 'm';
  return Math.round(s/3600) + 'h';
}
var sortState = {};
function sortBy(col) {
  var dir = sortState[col] === -1 ? 1 : -1;
  sortState[col] = dir;
  document.querySelectorAll('th[data-col]').forEach(function(th) {
    th.classList.remove('sorted');
    th.querySelector('.sort-arrow').textContent = '';
  });
  var th = document.querySelector('th[data-col="' + col + '"]');
  th.classList.add('sorted');
  th.querySelector('.sort-arrow').textContent = dir === -1 ? '\u25BC' : '\u25B2';
  var tbody = document.querySelector('#node-table tbody');
  var rows = Array.from(tbody.querySelectorAll('tr.expandable'));
  rows.sort(function(a, b) {
    return cmpNode(a.cells[getColIndex(col)].textContent.trim(), b.cells[getColIndex(col)].textContent.trim(), dir);
  });
  rows.forEach(function(r) { tbody.appendChild(r); });
}
function getColIndex(col) {
  return {node:0,heartbeat:1,uptime:2,proxies:3,clients:4,rx:5,billrx:6,tx:7,billtx:8,'rate-rx':9,'rate-tx':10,earning:11}[col]||0;
}
function parseSortValue(s) {
  s = s.trim();
  if (s === '') return -1;
  var m = s.match(/^([\d.]+)\s*([KMGTPE]?B)$/i);
  if (m) return parseFloat(m[1]) * ({'B':1,'KB':1024,'MB':1048576,'GB':1073741824,'TB':1099511627776}[m[2].toUpperCase()]||1);
  if (/^-?[\d.]+$/.test(s)) return parseFloat(s);
  return s;
}
function cmpNode(a, b, dir) {
  var pa = parseSortValue(a), pb = parseSortValue(b);
  if (typeof pa === 'number' && typeof pb === 'number') return dir * (pa - pb);
  return dir * String(a).localeCompare(String(b), undefined, {numeric:true});
}
function removeNode(nodeId) {
  if (!confirm('Remove ' + nodeId + ' from dashboard?')) return;
  fetch('/api/nodes/remove', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({node_id:nodeId})}).then(function(r){if(r.ok)refreshDashboard();});
}
loadFleetChart();
</script>
</body>
</html>
`))
