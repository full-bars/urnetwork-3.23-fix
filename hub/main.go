package main

import (
	"bytes"
	"compress/gzip"
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
	"strings"
	"sync"
	"time"
)

var Version string

// Simple per-IP sliding window rate limiter.
type rateLimiter struct {
	mu       sync.Mutex
	clients  map[string]*clientRate
	limit    int           // max requests per window
	window   time.Duration // sliding window size
}

type clientRate struct {
	times []time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	rl := &rateLimiter{
		clients: make(map[string]*clientRate),
		limit:   limit,
		window:  window,
	}
	// Background cleanup of stale entries every 5 minutes
	go func() {
		for {
			time.Sleep(5 * time.Minute)
			rl.mu.Lock()
			now := time.Now()
			for ip, cr := range rl.clients {
				cutoff := now.Add(-rl.window)
				var kept []time.Time
				for _, t := range cr.times {
					if t.After(cutoff) {
						kept = append(kept, t)
					}
				}
				if len(kept) == 0 {
					delete(rl.clients, ip)
				} else {
					cr.times = kept
				}
			}
			rl.mu.Unlock()
		}
	}()
	return rl
}

func (rl *rateLimiter) allow(ip string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	cr, ok := rl.clients[ip]
	if !ok {
		cr = &clientRate{}
		rl.clients[ip] = cr
	}
	cutoff := now.Add(-rl.window)
	var active []time.Time
	for _, t := range cr.times {
		if t.After(cutoff) {
			active = append(active, t)
		}
	}
	if len(active) >= rl.limit {
		cr.times = active
		return false
	}
	cr.times = append(active, now)
	return true
}

// rateLimitMiddleware limits requests to rateLimit per rateWindow per client IP.
func rateLimitMiddleware(next http.Handler) http.Handler {
	rl := newRateLimiter(60, time.Minute) // 60 req/min per IP
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Extract client IP from X-Forwarded-For or remote address
		ip := r.RemoteAddr
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if idx := strings.Index(fwd, ","); idx > 0 {
				ip = strings.TrimSpace(fwd[:idx])
			} else {
				ip = strings.TrimSpace(fwd)
			}
		} else if idx := strings.LastIndex(ip, ":"); idx > 0 {
			ip = ip[:idx]
		}
		// Don't rate limit the report endpoint
		if r.URL.Path == "/api/report" {
			next.ServeHTTP(w, r)
			return
		}
		if !rl.allow(ip) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "rate limit exceeded", 429)
			return
		}
		next.ServeHTTP(w, r)
	})
}

var funcMap = template.FuncMap{
	"fmtBytes": fmtBytes,
	"fmtMbps":  fmtMbps,
	"title":    title,
	"fmtAge":   fmtAge,
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

// proxyCounts computes aggregate counts from a proxy report slice.
func proxyCounts(proxies []proxyReport) proxSummary {
	var ps proxSummary
	for _, p := range proxies {
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
	}
	return ps
}

type proxSummary struct {
	Up, Connecting, Degraded, Dead int
	Clients                        int64
	TotalRX, TotalTX               uint64
	BillRX, BillTX                 uint64
	Earning                        int
}

type nodeRow struct {
	NodeID    string
	Host      string
	Version   string
	Heartbeat string
	Color     string
	Uptime    string
	Proxies   proxSummary
	MbpsRX    float64
	MbpsTX    float64
	HeapMiB   uint64
	SysMiB    uint64
	Conns     int64
	Index     int
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

// gzipMiddleware wraps an http.Handler, transparently compressing responses
// when the client sends Accept-Encoding: gzip.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gz, err := gzip.NewWriterLevel(w, gzip.DefaultCompression)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		defer gz.Close()
		w.Header().Set("Content-Encoding", "gzip")
		next.ServeHTTP(&gzipResponseWriter{ResponseWriter: w, Writer: gz}, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	Writer io.Writer
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	return g.Writer.Write(b)
}

func handleNodes(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Support /api/nodes/<id>/proxies for lazy-loaded proxy detail rows
		path := strings.TrimPrefix(r.URL.Path, "/api/nodes/")
		if path != "" && strings.HasSuffix(path, "/proxies") {
			nodeID := strings.TrimSuffix(path, "/proxies")
			s.mu.RLock()
			n, ok := s.Nodes[nodeID]
			s.mu.RUnlock()
			if !ok {
				http.Error(w, "node not found", 404)
				return
			}
			json.NewEncoder(w).Encode(n.Proxies)
			return
		}
		// Return node list without proxy details for lightweight polling
		type nodeSummary struct {
			NodeID      string         `json:"node_id"`
			Host        string         `json:"host"`
			Version     string         `json:"version"`
			Timestamp   time.Time      `json:"ts"`
			Uptime      float64        `json:"uptime"`
			Proxies     int            `json:"proxies"`
			Up          int            `json:"up"`
			Connecting  int            `json:"connecting"`
			Degraded    int            `json:"degraded"`
			Dead        int            `json:"dead"`
			MbpsRX      float64        `json:"mbps_rx"`
			MbpsTX      float64        `json:"mbps_tx"`
			Earning     int            `json:"earning"`
			System      systemMetrics  `json:"sys"`
		}
		nodes := s.list()
		out := make([]nodeSummary, 0, len(nodes))
		for _, n := range nodes {
			ps := proxyCounts(n.Proxies)
			nodeEarning := s.getEarning(n.NodeID)
			earning := 0
			for _, p := range n.Proxies {
				if nodeEarning[p.ID] {
					earning++
				}
			}
			mbpsRX, mbpsTX := s.getRate(n.NodeID)
			out = append(out, nodeSummary{
				NodeID:    n.NodeID,
				Host:      n.Host,
				Version:   n.Version,
				Timestamp: n.Timestamp,
				Uptime:    n.Uptime,
				Proxies:   len(n.Proxies),
				Up:        ps.Up,
				Connecting: ps.Connecting,
				Degraded:  ps.Degraded,
				Dead:      ps.Dead,
				MbpsRX:    mbpsRX,
				MbpsTX:    mbpsTX,
				Earning:   earning,
				System:    n.System,
			})
		}
		json.NewEncoder(w).Encode(out)
	}
}

const maxHistoryHours = 168

// handleHistory serves the hourly rollups stored in SQLite as JSON. Query
// params: node (optional node_id filter) and hours (lookback window, default
// 24, max 168). Example: /api/history?node=la6&hours=168
func handleHistory(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nodeID := r.URL.Query().Get("node")
		hours := 24
		if h := r.URL.Query().Get("hours"); h != "" {
			if v, err := strconv.Atoi(h); err == nil && v > 0 {
				hours = v
			}
		}
		if hours > maxHistoryHours {
			hours = maxHistoryHours
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

		var sm summaryRow
		rows := make([]nodeRow, 0, len(nodes))
		for i, n := range nodes {
			nodeEarning := s.getEarning(n.NodeID)
			ps := proxyCounts(n.Proxies)

			// Accumulate fleet totals in one pass
			sm.Nodes++
			sm.Up += ps.Up
			sm.Connecting += ps.Connecting
			sm.Degraded += ps.Degraded
			sm.Dead += ps.Dead
			sm.TotalClients += ps.Clients
			sm.TotalRX += ps.TotalRX
			sm.TotalTX += ps.TotalTX
			sm.BillRX += ps.BillRX
			sm.BillTX += ps.BillTX
			sm.TotalProxies += len(n.Proxies)
			for _, p := range n.Proxies {
				if nodeEarning[p.ID] {
					sm.Earning++
				}
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
				Index:     i,
			})
		}

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
	if err := http.ListenAndServe(*addr, gzipMiddleware(rateLimitMiddleware(mux))); err != nil {
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
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; background: #0b1120; color: #e2e8f0; padding: 0; }
.header { background: linear-gradient(135deg, #0f172a 0%, #1a2332 100%); border-bottom: 1px solid #1e293b; padding: 20px 24px 16px; }
.header h1 { font-size: 20px; font-weight: 600; color: #f1f5f9; }
.header h1 small { color: #64748b; font-weight: 400; font-size: 13px; margin-left: 8px; }
.summary { display: flex; gap: 4px 20px; flex-wrap: wrap; margin-top: 12px; font-size: 13px; }
.summary-item { display: flex; align-items: center; gap: 6px; color: #94a3b8; }
.summary-item strong { font-weight: 600; }
.summary-item .val { color: #e2e8f0; font-variant-numeric: tabular-nums; }
.summary-item .up { color: #4ade80; }
.summary-item .connecting { color: #60a5fa; }
.summary-item .degraded { color: #facc15; }
.summary-item .dead { color: #f87171; }
.table-wrap { padding: 0 24px; }
table { width: 100%; border-collapse: collapse; font-size: 13px; }
th, td { text-align: left; padding: 10px 12px; border-bottom: 1px solid #1e293b; white-space: nowrap; }
th { color: #64748b; font-weight: 600; text-transform: uppercase; font-size: 11px; letter-spacing: 0.06em; background: #0b1120; position: sticky; top: 0; cursor: pointer; user-select: none; }
th:hover { color: #94a3b8; }
th.sorted { color: #60a5fa; }
th .sort-arrow { display: inline-block; width: 10px; margin-left: 2px; color: #475569; }
th.sorted .sort-arrow { color: #60a5fa; }
.remove-btn { color: #475569; cursor: pointer; font-size: 13px; padding: 2px 6px; border-radius: 3px; }
.remove-btn:hover { color: #f87171; background: rgba(248,113,113,0.1); }
td { font-variant-numeric: tabular-nums; }
tr { transition: background 0.1s; }
tr:hover td { background: #1a2332; }
tr.expandable { cursor: pointer; }
tr.detail-row { display: none; }
tr.detail-row.open { display: table-row; }
tr.detail-row td { padding: 0; background: #0f172a; }
.proxy-list .loading { padding: 16px; color: #64748b; text-align: center; }
.proxy-list table { width: 100%; border-collapse: collapse; font-size: 12px; }
.proxy-list th { text-align: left; padding: 8px 10px; border-bottom: 1px solid #1e293b; color: #64748b; font-weight: 600; white-space: nowrap; cursor: pointer; user-select: none; }
.proxy-list th:hover { color: #94a3b8; }
.proxy-list td { padding: 6px 10px; border-bottom: 1px solid #1e293b; font-size: 12px; }
.detail-inner { padding: 8px 12px 12px 60px; }
.detail-table { width: 100%; border-collapse: collapse; font-size: 12px; }
.detail-table th { background: transparent; color: #64748b; padding: 6px 8px; font-size: 10px; border-bottom: 1px solid #1e293b; }
.detail-table td { padding: 4px 8px; border-bottom: 1px solid #1e293b; background: transparent; white-space: nowrap; }
.detail-table tr:last-child td { border-bottom: none; }
.detail-table .proxy-status { display: inline-block; width: 6px; height: 6px; border-radius: 50%; margin-right: 6px; vertical-align: middle; }
.dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; margin-right: 6px; vertical-align: middle; }
.up { color: #4ade80; }
.connecting { color: #60a5fa; }
.degraded { color: #facc15; }
.dead { color: #f87171; }
.idle { color: #64748b; }
.num { text-align: right; font-family: "JetBrains Mono", "Fira Code", monospace; }
.num-mono { font-family: "JetBrains Mono", "Fira Code", monospace; }
.node-id { font-weight: 500; color: #f1f5f9; }
.version { color: #475569; font-size: 11px; }
.status-badge { display: inline-block; padding: 1px 6px; border-radius: 3px; font-size: 11px; font-weight: 500; }
.status-badge.up { background: rgba(74,222,128,0.12); color: #4ade80; }
.status-badge.connecting { background: rgba(96,165,250,0.12); color: #60a5fa; }
.status-badge.degraded { background: rgba(250,204,21,0.12); color: #facc15; }
.status-badge.dead { background: rgba(248,113,113,0.12); color: #f87171; }
.footer { padding: 16px 24px; color: #475569; font-size: 12px; display: flex; justify-content: space-between; align-items: center; }
.footer a { color: #60a5fa; text-decoration: none; }
.footer a:hover { text-decoration: underline; }
#countdown { font-variant-numeric: tabular-nums; }
@media (max-width: 900px) {
  .header { padding: 16px; }
  .table-wrap { padding: 0; overflow-x: auto; }
  .footer { padding: 12px 16px; flex-direction: column; gap: 8px; text-align: center; }
}
</style>
</head>
<body>
<div class="header">
<h1>URnetwork Hub <small>&mdash; fleet bandwidth dashboard</small></h1>
<div class="summary">
<span class="summary-item"><strong>Nodes</strong> <span class="val">{{.Sum.Nodes}}</span></span>
<span class="summary-item"><strong>Proxies</strong>
  {{if .Sum.Up}}<span class="val up">{{.Sum.Up}} up</span>{{end}}
  {{if .Sum.Connecting}}<span class="val connecting">{{.Sum.Connecting}} conn</span>{{end}}
  {{if .Sum.Degraded}}<span class="val degraded">{{.Sum.Degraded}} deg</span>{{end}}
  {{if .Sum.Dead}}<span class="val dead">{{.Sum.Dead}} dead</span>{{end}}
</span>
<span class="summary-item"><strong>Earning</strong> <span class="val">{{.Sum.Earning}}/{{.Sum.TotalProxies}}</span></span>
<span class="summary-item"><strong>Clients</strong> <span class="val">{{.Sum.TotalClients}}</span></span>
<span class="summary-item"><strong>RX</strong> <span class="val">{{fmtBytes .Sum.TotalRX}}</span> <span style="color:#ef4444;font-weight:bold">· {{fmtBytes .Sum.BillRX}} billable</span></span>
<span class="summary-item"><strong>TX</strong> <span class="val">{{fmtBytes .Sum.TotalTX}}</span> <span style="color:#ef4444;font-weight:bold">· {{fmtBytes .Sum.BillTX}} billable</span></span>
</div>
</div>
<div class="table-wrap">
<table id="node-table">
<thead>
<tr>
<th data-col="node">Node<span class="sort-arrow"></span></th>
<th data-col="heartbeat">Heartbeat<span class="sort-arrow"></span></th>
<th data-col="uptime">Uptime<span class="sort-arrow"></span></th>
<th data-col="proxies" class="sorted">Proxies<span class="sort-arrow">▼</span></th>
<th data-col="clients" class="num">Clients<span class="sort-arrow"></span></th>
<th data-col="rx" class="num">RX<span class="sort-arrow"></span></th>
<th data-col="billrx" class="num">Bill RX<span class="sort-arrow"></span></th>
<th data-col="tx" class="num">TX<span class="sort-arrow"></span></th>
<th data-col="billtx" class="num">Bill TX<span class="sort-arrow"></span></th>
<th data-col="rate-rx" class="num">In Mbps<span class="sort-arrow"></span></th>
<th data-col="rate-tx" class="num">Out Mbps<span class="sort-arrow"></span></th>
<th data-col="heap" class="num">Heap<span class="sort-arrow"></span></th>
<th data-col="conns" class="num">Conns<span class="sort-arrow"></span></th>
<th class="num">Earning</th>
<th></th>
</tr>
</thead>
<tbody>
{{range .Rows}}
<tr class="expandable" data-id="{{.NodeID}}" onclick="toggleDetail('{{.NodeID}}')">
<td class="node-id">{{.NodeID}} <span class="version">{{.Version}}</span></td>
<td><span class="dot" style="background:{{.Color}}"></span>{{.Heartbeat}}</td>
<td>{{.Uptime}}</td>
<td>
{{if .Proxies.Up}}<span class="status-badge up">{{.Proxies.Up}}</span>{{end}}
{{if .Proxies.Connecting}}<span class="status-badge connecting">{{.Proxies.Connecting}}</span>{{end}}
{{if .Proxies.Degraded}}<span class="status-badge degraded">{{.Proxies.Degraded}}</span>{{end}}
{{if .Proxies.Dead}}<span class="status-badge dead">{{.Proxies.Dead}}</span>{{end}}
</td>
<td class="num">{{.Proxies.Clients}}</td>
<td class="num">{{fmtBytes .Proxies.TotalRX}}</td>
<td class="num" style="color:#ef4444;font-weight:bold">{{fmtBytes .Proxies.BillRX}}</td>
<td class="num">{{fmtBytes .Proxies.TotalTX}}</td>
<td class="num" style="color:#ef4444;font-weight:bold">{{fmtBytes .Proxies.BillTX}}</td>
<td class="num">{{fmtMbps .MbpsRX}}</td>
<td class="num">{{fmtMbps .MbpsTX}}</td>
<td class="num">{{.HeapMiB}} MiB</td>
<td class="num">{{.Conns}}</td>
<td class="num">{{.Proxies.Earning}}/{{.Proxies.Up}}</td>
<td><span class="remove-btn" onclick="event.stopPropagation();removeNode('{{.NodeID}}')" title="Remove node">✕</span></td>
</tr>
<tr class="detail-row" id="detail-{{.NodeID}}">
<td colspan="15">
<div class="detail-inner">
<div id="proxies-{{.NodeID}}" class="proxy-list"><div class="loading">Loading proxies...</div></div>
</div>
</td>
</tr>
{{end}}
</tbody>
</table>
</div>
<div class="footer">
<div>
<label style="display:inline-flex;align-items:center;gap:4px">
<input type="checkbox" id="auto-refresh" checked onchange="toggleRefresh()">
auto-refresh
</label>
<span id="countdown" style="margin-left:12px"></span>
</div>
<div>
<a href="/api/nodes">/api/nodes (JSON)</a>
</div>
</div>
<script>
var proxyCache = {};

function toggleDetail(id) {
  var detail = document.getElementById('detail-' + id);
  detail.classList.toggle('open');
  if (detail.classList.contains('open') && !proxyCache[id]) {
    proxyCache[id] = true;
    var container = document.getElementById('proxies-' + id);
    fetch('/api/nodes/' + id + '/proxies').then(function(r) { return r.json(); }).then(function(proxies) {
      if (!proxies || proxies.length === 0) {
        container.innerHTML = '<div class="loading">No proxy data</div>';
        return;
      }
      var html = '<table><thead><tr><th>ID</th><th>Address</th><th>Status</th><th class="num">Clients</th><th class="num">Max Age</th><th class="num">RX</th><th class="num">TX</th><th class="num">Bill RX</th><th class="num">Bill TX</th></tr></thead><tbody>';
      proxies.forEach(function(p) {
        var earning = p.bill_rx > 0 || p.bill_tx > 0;
        html += '<tr><td class="num-mono">' + p.id + '</td><td>' + p.addr + '</td><td><span class="proxy-status ' + p.status + '"></span>' + p.status + '</td><td class="num">' + p.clients + '</td><td class="num">' + fmtAge(p.max_age_s) + '</td><td class="num">' + fmtBytes(p.rx) + '</td><td class="num">' + fmtBytes(p.tx) + '</td><td class="num">' + fmtBytes(p.bill_rx) + '</td><td class="num">' + fmtBytes(p.bill_tx) + '</td></tr>';
      });
      html += '</tbody></table>';
      container.innerHTML = html;
    }).catch(function() {
      container.innerHTML = '<div class="loading">Failed to load proxies</div>';
    });
  }
}

function removeNode(nodeId) {
  if (!confirm('Remove ' + nodeId + ' from dashboard?')) return;
  fetch('/api/nodes/remove', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({node_id: nodeId})
  }).then(function(r) {
    if (r.ok) refreshDashboard();
    else alert('Failed to remove node');
  });
}

// auto-refresh countdown
var secondsLeft = 30;
var countdownEl = document.getElementById('countdown');
var autoRefresh = document.getElementById('auto-refresh');

function tick() {
  if (!autoRefresh.checked) {
    countdownEl.textContent = '⏸ paused';
    return;
  }
  secondsLeft--;
  if (secondsLeft <= 0) {
    secondsLeft = 30;
    refreshDashboard();
    return;
  }
  countdownEl.textContent = 'refreshing in ' + secondsLeft + 's';
}
setInterval(tick, 1000);
countdownEl.textContent = 'refreshing in 30s';

function toggleRefresh() {
  if (autoRefresh.checked) {
    secondsLeft = 30;
  }
}

function closeAllDetails() {
  document.querySelectorAll('.detail-row.open').forEach(function(r) { r.classList.remove('open'); });
}

var refreshing = false;

function refreshDashboard() {
  if (refreshing) return;
  refreshing = true;
  var openIds = [];
  document.querySelectorAll('.detail-row.open').forEach(function(r) { openIds.push(r.id); });
  
  fetch('/api/nodes').then(function(r) { return r.json(); }).then(function(nodes) {
    var tbody = document.querySelector('#node-table tbody');
    var rows = {};
    tbody.querySelectorAll('tr.expandable').forEach(function(r) {
      rows[r.getAttribute('data-id')] = r;
    });
    
    var total = { up: 0, connecting: 0, degraded: 0, dead: 0, clients: 0, earning: 0, proxies: 0, nodes: 0 };
    var frag = document.createDocumentFragment();
    
    nodes.forEach(function(n) {
      total.nodes++;
      total.up += n.up;
      total.connecting += n.connecting;
      total.degraded += n.degraded;
      total.dead += n.dead;
      total.clients += n.clients;
      total.proxies += n.proxies;
      
      var ago = fmtAgo(n.ts);
      var uptime = fmtUptime(n.uptime);
      var color = n.ts ? nodeColor(n.ts) : '#ef4444';
      var existing = rows[n.node_id];
      if (existing) {
        // Update existing row in place
        existing.innerHTML = '<td><span class="dot" style="background:' + color + '"></span>' + n.node_id + '</td><td>' + (n.host||'') + '</td><td>' + ago + '</td><td>' + uptime + '</td><td class="num">' + n.proxies + '</td><td class="num">' + n.clients + '</td><td class="num">' + fmtBytes(n.rx) + '</td><td class="num">' + fmtBytes(n.tx) + '</td><td class="num">' + fmtBytes(n.bill_rx) + '</td><td class="num">' + fmtBytes(n.bill_tx) + '</td><td class="num">' + fmtMbps(n.mbps_rx) + '</td><td class="num">' + fmtMbps(n.mbps_tx) + '</td><td class="num">' + n.earning + '/' + n.up + '</td><td><span class="remove-btn" onclick="event.stopPropagation();removeNode(\'' + n.node_id + '\')" title="Remove node">&#10005;</span></td>';
      } else {
        // New node, add a row
        var tr = document.createElement('tr');
        tr.className = 'expandable';
        tr.setAttribute('data-id', n.node_id);
        tr.onclick = function() { toggleDetail(n.node_id); };
        tr.innerHTML = '<td><span class="dot" style="background:' + color + '"></span>' + n.node_id + '</td><td>' + (n.host||'') + '</td><td>' + ago + '</td><td>' + uptime + '</td><td class="num">' + n.proxies + '</td><td class="num">' + n.clients + '</td><td class="num">' + fmtBytes(n.rx) + '</td><td class="num">' + fmtBytes(n.tx) + '</td><td class="num">' + fmtBytes(n.bill_rx) + '</td><td class="num">' + fmtBytes(n.bill_tx) + '</td><td class="num">' + fmtMbps(n.mbps_rx) + '</td><td class="num">' + fmtMbps(n.mbps_tx) + '</td><td class="num">' + n.earning + '/' + n.up + '</td><td><span class="remove-btn" onclick="event.stopPropagation();removeNode(\'' + n.node_id + '\')" title="Remove node">&#10005;</span></td>';
        frag.appendChild(tr);
        // Add detail row
        var detail = document.createElement('tr');
        detail.className = 'detail-row';
        detail.id = 'detail-' + n.node_id;
        detail.innerHTML = '<td colspan="15"><div class="detail-inner"><div id="proxies-' + n.node_id + '" class="proxy-list"><div class="loading">Loading proxies...</div></div></div></td>';
        frag.appendChild(detail);
      }
    });
    
    tbody.innerHTML = '';
    tbody.appendChild(frag);
    
    openIds.forEach(function(id) {
      var r = document.getElementById(id);
      if (r) r.classList.add('open');
    });
    
    // Update summary
    var sumHtml = '<span>' + total.nodes + ' nodes</span><span>' + total.proxies + ' proxies</span><span class="up">' + total.up + ' up</span><span class="warn">' + total.degraded + ' degraded</span><span class="dead">' + total.dead + ' dead</span><span>' + total.clients + ' clients</span>';
    document.querySelector('.summary').innerHTML = sumHtml;
    
    if (currentCol) {
      // Re-sort
      var rowsArr = Array.from(tbody.querySelectorAll('tr.expandable'));
      rowsArr.sort(function(a, b) {
        var va = a.cells[getColIndex(currentCol)].textContent.trim();
        var vb = b.cells[getColIndex(currentCol)].textContent.trim();
        return cmpNode(va, vb, currentDir);
      });
      var sortedFrag = document.createDocumentFragment();
      rowsArr.forEach(function(r) {
        sortedFrag.appendChild(r);
        var detail = document.getElementById('detail-' + r.getAttribute('data-id'));
        if (detail) sortedFrag.appendChild(detail);
      });
      tbody.innerHTML = '';
      tbody.appendChild(sortedFrag);
    }
  }).catch(function() {}).then(function() {
    refreshing = false;
  });
}

// column sorting
var sortDir = {};
var currentCol = 'proxies';
var currentDir = -1;

document.querySelectorAll('th[data-col]').forEach(function(th) {
  th.addEventListener('click', function() {
    var col = th.getAttribute('data-col');
    var dir = sortDir[col] === undefined ? -1 : -sortDir[col];
    sortDir[col] = dir;
    currentCol = col;
    currentDir = dir;
    document.querySelectorAll('th[data-col]').forEach(function(h) {
      h.classList.remove('sorted');
      h.querySelector('.sort-arrow').textContent = '';
    });
    th.classList.add('sorted');
    th.querySelector('.sort-arrow').textContent = dir === -1 ? '▼' : '▲';

    var tbody = document.querySelector('#node-table tbody');
    var rows = Array.from(tbody.querySelectorAll('tr.expandable'));
    rows.sort(function(a, b) {
      var va = a.cells[getColIndex(col)].textContent.trim();
      var vb = b.cells[getColIndex(col)].textContent.trim();
      return cmpNode(va, vb, dir);
    });
    // reorder rows + their detail rows
    rows.forEach(function(r) {
      var detail = r.nextElementSibling;
      tbody.appendChild(r);
      if (detail && detail.classList.contains('detail-row')) {
        tbody.appendChild(detail);
      }
    });
  });
});

function getColIndex(col) {
  var map = {node:0, heartbeat:1, uptime:2, proxies:3, clients:4, rx:5, billrx:6, tx:7, billtx:8, 'rate-rx':9, 'rate-tx':10, heap:11, conns:12};
  return map[col] || 0;
}

// helper matching Go fmtBytes
function fmtAgo(ts) {
  if (!ts) return 'never';
  var d = (Date.now() - new Date(ts).getTime()) / 1000;
  if (d < 10) return 'just now';
  if (d < 60) return Math.round(d) + 's ago';
  if (d < 3600) return Math.round(d/60) + 'm ago';
  return Math.round(d/3600) + 'h ago';
}

function fmtUptime(s) {
  if (!s) return '0s';
  var h = Math.floor(s / 3600);
  var d = Math.floor(h / 24);
  if (d > 0) return d + 'd ' + (h % 24) + 'h';
  if (h > 0) return h + 'h ' + Math.floor((s % 3600) / 60) + 'm';
  return Math.floor(s / 60) + 'm';
}

function fmtMbps(v) {
  if (!v || v === 0) return '—';
  return v.toFixed(1) + ' Mbps';
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

function parseSortValue(s) {
  s = s.trim();
  if (s === '—') return -1;
  var m = s.match(/^([\d.]+)\s*([KMGTPE]?B)$/i);
  if (m) return parseFloat(m[1]) * ({'B':1,'KB':1024,'MB':1024*1024,'GB':1024*1024*1024,'TB':1024*1024*1024*1024}[m[2].toUpperCase()]||1);
  m = s.match(/^([\d.]+)\s*([KMG]?bps)$/i);
  if (m) return parseFloat(m[1]) * ({'BPS':1,'KBPS':1000,'MBPS':1000*1000,'GBPS':1000*1000*1000}[m[2].toUpperCase()]||1);
  var ts = s.replace(/ ago$/, '');
  if (/^(?:\d+d\s*)?(?:\d+h\s*)?(?:\d+m\s*)?(?:\d+s)?$/.test(ts) && ts.length > 0) {
    var tot = 0;
    ts.split(' ').forEach(function(p) {
      var n = parseInt(p);
      if (p.endsWith('d')) tot += n * 86400;
      else if (p.endsWith('h')) tot += n * 3600;
      else if (p.endsWith('m')) tot += n * 60;
      else if (p.endsWith('s')) tot += n;
    });
    if (tot > 0 || ts === '0s') return tot;
  }
  if (/^\d+(\s+\d+)*$/.test(s)) return parseFloat(s.split(/\s+/)[0]);
  m = s.match(/^([\d.]+)\s*MiB$/);
  if (m) return parseFloat(m[1]);
  if (/^-?[\d.]+$/.test(s)) return parseFloat(s);
  return s;
}

function cmpNode(a, b, dir) {
  var pa = parseSortValue(a);
  var pb = parseSortValue(b);
  if (typeof pa === 'number' && typeof pb === 'number') return dir * (pa - pb);
  return dir * String(a).localeCompare(String(b), undefined, {numeric: true});
}

function attachInnerSort() {
  document.querySelectorAll('.detail-table').forEach(function(tbl) {
    if (tbl.dataset.sorted) return;
    tbl.dataset.sorted = "true";
    var ths = tbl.querySelectorAll('th');
    ths.forEach(function(th, idx) {
      th.style.cursor = 'pointer';
      th.addEventListener('click', function() {
        var dir = th.dataset.dir === '1' ? -1 : 1;
        ths.forEach(function(h) { h.dataset.dir = ''; h.textContent = h.textContent.replace(/ [▲▼]/g, '').trim(); });
        th.dataset.dir = dir;
        th.textContent = th.textContent + (dir === 1 ? ' ▲' : ' ▼');
        
        var tbody = tbl.querySelector('tbody');
        var rows = Array.from(tbody.querySelectorAll('tr'));
        rows.sort(function(a, b) {
          var va = a.cells[idx].textContent.trim();
          var vb = b.cells[idx].textContent.trim();
          return cmpNode(va, vb, dir);
        });
        rows.forEach(function(r) { tbody.appendChild(r); });
      });
    });
  });
}

// Initial setup
attachInnerSort();
</script>
</body>
</html>`))
