package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

var Version string

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
	mu       sync.RWMutex
	path     string
	Nodes    map[string]*nodeState   `json:"nodes"`
	rates    map[string]*nodeRate    `json:"-"`
}

type nodeRate struct {
	ts  time.Time
	rx  uint64
	tx  uint64
	mbpsRx float64
	mbpsTx float64
}

func loadStore(path string) *store {
	s := &store{path: path, Nodes: make(map[string]*nodeState), rates: make(map[string]*nodeRate)}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	json.Unmarshal(data, s)
	if s.Nodes == nil {
		s.Nodes = make(map[string]*nodeState)
	}
	return s
}

func (s *store) save() error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
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

	s.Nodes[nodeID] = state
	s.save()
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

func (s *store) summary() summaryRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var sr summaryRow
	for _, n := range s.Nodes {
		sr.Nodes++
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
		}
	}
	return sr
}

type summaryRow struct {
	Nodes, Up, Connecting, Degraded, Dead int
	TotalClients                           int64
	TotalRX, TotalTX                       uint64
	BillRX, BillTX                         uint64
}

type proxSummary struct {
	Up, Connecting, Degraded, Dead int
	Clients                        int64
	TotalRX, TotalTX               uint64
	BillRX, BillTX                 uint64
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
	ProxyList     []proxyReport
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

func nodeColor(ts time.Time) string {
	d := time.Since(ts)
	if d < 2*time.Minute {
		return "#22c55e"
	}
	if d < 5*time.Minute {
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
		s.mu.Unlock()
		s.save()
		fmt.Printf("removed node %s\n", req.NodeID)
		w.WriteHeader(204)
	}
}

func handleDashboard(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		nodes := s.list()

		rows := make([]nodeRow, 0, len(nodes))
		for i, n := range nodes {
			var ps proxSummary
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
				ProxyList: n.Proxies,
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

	storePath := filepath.Join(*dataDir, "hub.json")
	s := loadStore(storePath)

	mux := http.NewServeMux()

	mux.HandleFunc("/api/report", handleReport(s))
	mux.HandleFunc("/api/nodes", handleNodes(s))
	mux.HandleFunc("/api/nodes/remove", handleNodeRemove(s))
	mux.HandleFunc("/", handleDashboard(s))

	fmt.Printf("hub listening on %s (data: %s)\n", *addr, storePath)
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
<span class="summary-item"><strong>Clients</strong> <span class="val">{{.Sum.TotalClients}}</span></span>
<span class="summary-item"><strong>RX</strong> <span class="val">{{fmtBytes .Sum.TotalRX}}</span> <span style="color:#64748b">· {{fmtBytes .Sum.BillRX}} billable</span></span>
<span class="summary-item"><strong>TX</strong> <span class="val">{{fmtBytes .Sum.TotalTX}}</span> <span style="color:#64748b">· {{fmtBytes .Sum.BillTX}} billable</span></span>
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
<th data-col="tx" class="num">TX<span class="sort-arrow"></span></th>
<th data-col="rate-rx" class="num">In Mbps<span class="sort-arrow"></span></th>
<th data-col="rate-tx" class="num">Out Mbps<span class="sort-arrow"></span></th>
<th data-col="heap" class="num">Heap<span class="sort-arrow"></span></th>
<th data-col="conns" class="num">Conns<span class="sort-arrow"></span></th>
<th></th>
</tr>
</thead>
<tbody>
{{range .Rows}}
<tr class="expandable" onclick="toggleDetail({{.Index}})">
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
<td class="num">{{fmtBytes .Proxies.TotalRX}}<br><span style="font-size:11px;color:#64748b">{{fmtBytes .Proxies.BillRX}} b</span></td>
<td class="num">{{fmtBytes .Proxies.TotalTX}}<br><span style="font-size:11px;color:#64748b">{{fmtBytes .Proxies.BillTX}} b</span></td>
<td class="num">{{fmtMbps .MbpsRX}}</td>
<td class="num">{{fmtMbps .MbpsTX}}</td>
<td class="num">{{.HeapMiB}} MiB</td>
<td class="num">{{.Conns}}</td>
<td><span class="remove-btn" onclick="event.stopPropagation();removeNode('{{.NodeID}}')" title="Remove node">✕</span></td>
</tr>
<tr class="detail-row" id="detail-{{.Index}}">
<td colspan="12">
<div class="detail-inner">
<table class="detail-table">
<thead>
<tr>
<th>ID</th>
<th>Address</th>
<th>Status</th>
<th class="num">Clients</th>
<th class="num">Max Age</th>
<th class="num">RX</th>
<th class="num">TX</th>
<th class="num">Bill RX</th>
<th class="num">Bill TX</th>
</tr>
</thead>
<tbody>
{{range .ProxyList}}
<tr>
<td class="num-mono">{{.ID}}</td>
<td>{{.Address}}</td>
<td><span class="proxy-status {{.Status}}"></span>{{title .Status}}</td>
<td class="num">{{.Clients}}</td>
<td class="num">{{fmtAge .MaxAge}}</td>
<td class="num">{{fmtBytes .TotalRX}}</td>
<td class="num">{{fmtBytes .TotalTX}}</td>
<td class="num">{{fmtBytes .BillRX}}</td>
<td class="num">{{fmtBytes .BillTX}}</td>
</tr>
{{end}}
</tbody>
</table>
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
function toggleDetail(idx) {
  document.getElementById('detail-' + idx).classList.toggle('open');
}

function removeNode(nodeId) {
  if (!confirm('Remove ' + nodeId + ' from dashboard?')) return;
  fetch('/api/nodes/remove', {
    method: 'POST',
    headers: {'Content-Type': 'application/json'},
    body: JSON.stringify({node_id: nodeId})
  }).then(function(r) {
    if (r.ok) location.reload();
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
    location.reload();
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
      // close any open details first
      document.querySelectorAll('.detail-row.open').forEach(function(r) { r.classList.remove('open'); });
      var va = a.cells[getColIndex(col)].textContent.trim();
      var vb = b.cells[getColIndex(col)].textContent.trim();
      // numeric-aware sort
      var na = parseFloat(va.replace(/[^0-9.]/g, ''));
      var nb = parseFloat(vb.replace(/[^0-9.]/g, ''));
      if (!isNaN(na) && !isNaN(nb)) {
        return dir * (na - nb);
      }
      return dir * va.localeCompare(vb);
    });
    // reorder rows + their detail rows
    rows.forEach(function(r) {
      var idx = parseInt(r.cells[0].textContent.match(/\d+/) || '0');
      // Actually we need to identify the detail row. We'll look for the next sibling tr with class detail-row
      // But the detail row is always the next sibling
      var detail = r.nextElementSibling;
      tbody.appendChild(r);
      if (detail && detail.classList.contains('detail-row')) {
        tbody.appendChild(detail);
      }
    });
  });
});

function getColIndex(col) {
  var map = {node:0, heartbeat:1, uptime:2, proxies:3, clients:4, rx:5, tx:6, 'rate-rx':7, 'rate-tx':8, heap:9, conns:10};
  return map[col] || 0;
}

// helper matching Go fmtBytes
function fmtBytes(b) {
  if (b < 1024) return b + ' B';
  var units = 'KMGTPE';
  var i = -1;
  var n = b;
  while (n >= 1024 && i < units.length-1) { n /= 1024; i++; }
  return n.toFixed(1) + ' ' + units[i] + 'B';
}
</script>
</body>
</html>`))
