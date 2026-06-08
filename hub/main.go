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
	mu    sync.RWMutex
	path  string
	Nodes map[string]*nodeState `json:"nodes"`
}

func loadStore(path string) *store {
	s := &store{path: path, Nodes: make(map[string]*nodeState)}
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

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dataDir := flag.String("data", ".", "data directory for hub.json")
	flag.Parse()

	storePath := filepath.Join(*dataDir, "hub.json")
	s := loadStore(storePath)

	mux := http.NewServeMux()

	mux.HandleFunc("/api/report", func(w http.ResponseWriter, r *http.Request) {
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
		s.upsert(ns.NodeID, &ns)
		w.WriteHeader(204)
	})

	mux.HandleFunc("/api/nodes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.list())
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		nodes := s.list()

		type proxySummary struct {
			Up, Degraded, Dead int
		}
		type row struct {
			NodeID    string
			Host      string
			Version   string
			Heartbeat string
			Color     string
			Uptime    string
			Proxies   proxySummary
			Bandwidth string
			HeapMiB   uint64
			SysMiB    uint64
			Conns     int64
		}

		rows := make([]row, 0, len(nodes))
		for _, n := range nodes {
			var ps proxySummary
			var totalBillable uint64
			for _, p := range n.Proxies {
				totalBillable += p.BillTX + p.BillRX
				switch p.Status {
				case "up":
					ps.Up++
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

			rows = append(rows, row{
				NodeID:    n.NodeID,
				Host:      n.Host,
				Version:   n.Version,
				Heartbeat: agoStr,
				Color:     nodeColor(n.Timestamp),
				Uptime:    uptimeStr,
				Proxies:   ps,
				Bandwidth: fmtBytes(totalBillable),
				HeapMiB:   n.System.HeapMiB,
				SysMiB:    n.System.SysMiB,
				Conns:     n.System.Connections,
			})
		}

		var buf bytes.Buffer
		tmpl.Execute(&buf, rows)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		buf.WriteTo(w)
	})

	fmt.Printf("hub listening on %s (data: %s)\n", *addr, storePath)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		fmt.Fprintf(os.Stderr, "hub: %v\n", err)
		os.Exit(1)
	}
}

var tmpl = template.Must(template.New("dashboard").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>URnetwork Hub</title>
<style>
* { margin: 0; padding: 0; box-sizing: border-box; }
body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif; background: #0f172a; color: #e2e8f0; padding: 24px; }
h1 { font-size: 20px; font-weight: 600; margin-bottom: 16px; color: #f1f5f9; }
h1 span { color: #64748b; font-weight: 400; }
table { width: 100%; border-collapse: collapse; font-size: 13px; }
th, td { text-align: left; padding: 8px 12px; border-bottom: 1px solid #1e293b; white-space: nowrap; }
th { color: #64748b; font-weight: 600; text-transform: uppercase; font-size: 11px; letter-spacing: 0.05em; background: #0f172a; position: sticky; top: 0; }
td { font-variant-numeric: tabular-nums; }
tr:hover td { background: #1e293b; }
.dot { display: inline-block; width: 8px; height: 8px; border-radius: 50%; margin-right: 6px; vertical-align: middle; }
.up { color: #4ade80; }
.degraded { color: #facc15; }
.dead { color: #f87171; }
.num { text-align: right; font-family: "JetBrains Mono", "Fira Code", monospace; }
.node-id { font-weight: 500; color: #f1f5f9; }
.version { color: #64748b; }
a { color: #60a5fa; }
</style>
</head>
<body>
<h1>URnetwork Hub <span>&mdash; fleet bandwidth dashboard</span></h1>
<table>
<thead>
<tr>
<th>Node</th>
<th>Host</th>
<th>Heartbeat</th>
<th>Uptime</th>
<th>Proxies</th>
<th>Bandwidth</th>
<th>Heap</th>
<th>Sys</th>
<th>Conns</th>
</tr>
</thead>
<tbody>
{{range .}}
<tr>
<td class="node-id">{{.NodeID}}</td>
<td>{{.Host}} <span class="version">{{.Version}}</span></td>
<td><span class="dot" style="background:{{.Color}}"></span>{{.Heartbeat}}</td>
<td>{{.Uptime}}</td>
<td>
{{if .Proxies.Up}}<span class="up">{{.Proxies.Up}} up</span>{{end}}
{{if .Proxies.Degraded}} <span class="degraded">{{.Proxies.Degraded}} deg</span>{{end}}
{{if .Proxies.Dead}} <span class="dead">{{.Proxies.Dead}} dead</span>{{end}}
</td>
<td class="num">{{.Bandwidth}}</td>
<td class="num">{{.HeapMiB}} MiB</td>
<td class="num">{{.SysMiB}} MiB</td>
<td class="num">{{.Conns}}</td>
</tr>
{{end}}
</tbody>
</table>
<p style="margin-top:16px;color:#475569;font-size:12px">
auto-refreshes every 30s &middot;
<a href="/api/nodes">/api/nodes (JSON)</a>
</p>
<script>setTimeout(function(){location.reload()},30000)</script>
</body>
</html>`))
