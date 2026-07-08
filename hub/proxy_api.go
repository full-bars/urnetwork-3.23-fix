package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"time"
)

var windowSizes = map[string]int{
	"1h": 1, "24h": 24, "7d": 168, "30d": 720, "1y": 8760,
}

var sortCols = map[string]string{
	"traffic":   "rx+tx",
	"contracts": "acq",
	"denied":    "denied",
}

func parseWindow(s string) (hours int, ok bool) {
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return n, true
	}
	hours, ok = windowSizes[s]
	return
}

// Proxy top leaderboard.
// GET /api/proxies/top?window=24h&sort=traffic&order=desc&node=ID&limit=50
func handleProxiesTop(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		hours, ok := parseWindow(q.Get("window"))
		if !ok {
			hours = 24
		}
		node := q.Get("node")
		limit := 50
		if l := q.Get("limit"); l != "" {
			if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 500 {
				limit = v
			}
		}
		sortCol, ok := sortCols[q.Get("sort")]
		if !ok {
			sortCol = "rx+tx"
		}
		order := "DESC"
		if o := q.Get("order"); o == "asc" {
			order = "ASC"
		}

		var rows []struct {
			Addr   string `json:"addr"`
			RX     uint64 `json:"rx"`
			TX     uint64 `json:"tx"`
			BillRX uint64 `json:"bill_rx"`
			BillTX uint64 `json:"bill_tx"`
			Acq    int64  `json:"acq"`
			Denied int64  `json:"denied"`
			Nodes  int64  `json:"nodes"`
		}
		var err error

		if hours <= 90*24 {
			since := timeNowHour() - int64(hours)
			if node != "" {
				rows, err = queryProxiesTop(s, `
					SELECT p.addr, SUM(h.rx), SUM(h.tx), SUM(h.bill_rx), SUM(h.bill_tx),
					       SUM(h.acq), SUM(h.denied), COUNT(DISTINCT h.node_id)
					FROM proxy_node_hourly h JOIN proxies p ON p.id = h.proxy_id
					WHERE h.hour >= ? AND h.node_id = ?
					GROUP BY h.proxy_id ORDER BY `+safeOrder(sortCol, order)+` LIMIT ?`,
					since, node, limit)
			} else {
				// The fleet-wide (no node filter) leaderboard reads the
				// pre-aggregated proxy_fleet_hourly rollup instead of
				// GROUP-BY-ing the full proxy_node_hourly tier-1 table
				// (proxy x node x hour — millions of rows fleet-wide) on
				// every page load. The rollup only covers hours older than
				// 26h (see rollupProxyDaily), so the most recent tail is
				// live-aggregated from proxy_node_hourly and unioned in —
				// a small scan bounded to ~26h regardless of the requested
				// window.
				//
				// The live branch groups by proxy_id only (not proxy_id+hour)
				// so its node_count is an exact distinct-node count across
				// the live tail, matching what this endpoint always returned
				// for windows entirely inside that tail. Only the historical
				// fleet_hourly portion is hour-bucketed, so summing it with
				// the live count can over-count nodes that persisted across
				// many historical hours — the same approximation the
				// existing proxy_fleet_daily (>90 day) path already makes.
				recentCutoff := timeNowHour() - 26
				liveSince := since
				if liveSince < recentCutoff {
					liveSince = recentCutoff
				}
				rows, err = queryProxiesTop(s, `
					SELECT p.addr, SUM(rx), SUM(tx), SUM(bill_rx), SUM(bill_tx),
					       SUM(acq), SUM(denied), SUM(node_count)
					FROM (
						SELECT proxy_id, rx, tx, bill_rx, bill_tx, acq, denied, node_count
						FROM proxy_fleet_hourly WHERE hour >= ?
						UNION ALL
						SELECT proxy_id, SUM(rx) AS rx, SUM(tx) AS tx, SUM(bill_rx) AS bill_rx, SUM(bill_tx) AS bill_tx,
						       SUM(acq) AS acq, SUM(denied) AS denied, COUNT(DISTINCT node_id) AS node_count
						FROM proxy_node_hourly WHERE hour >= ?
						GROUP BY proxy_id
					) t JOIN proxies p ON p.id = t.proxy_id
					GROUP BY t.proxy_id ORDER BY `+safeOrder(sortCol, order)+` LIMIT ?`,
					since, liveSince, limit)
			}
		} else {
			since := timeNowHour()/24 - int64(hours/24)
			if node != "" {
				rows, err = queryProxiesTop(s, `
					SELECT p.addr, SUM(d.rx), SUM(d.tx), SUM(d.bill_rx), SUM(d.bill_tx),
					       SUM(d.acq), SUM(d.denied), COUNT(DISTINCT d.node_id)
					FROM proxy_node_daily d JOIN proxies p ON p.id = d.proxy_id
					WHERE d.day >= ? AND d.node_id = ?
					GROUP BY d.proxy_id ORDER BY `+safeOrder(sortCol, order)+` LIMIT ?`,
					since, node, limit)
			} else {
				rows, err = queryProxiesTop(s, `
					SELECT p.addr, SUM(f.rx), SUM(f.tx), SUM(f.bill_rx), SUM(f.bill_tx),
					       SUM(f.acq), SUM(f.denied), SUM(f.node_count)
					FROM proxy_fleet_daily f JOIN proxies p ON p.id = f.proxy_id
					WHERE f.day >= ?
					GROUP BY f.proxy_id ORDER BY `+safeOrder(sortCol, order)+` LIMIT ?`,
					since, limit)
			}
		}

		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(rows)
	}
}

func queryProxiesTop(s *store, query string, args ...any) ([]struct {
	Addr   string `json:"addr"`
	RX     uint64 `json:"rx"`
	TX     uint64 `json:"tx"`
	BillRX uint64 `json:"bill_rx"`
	BillTX uint64 `json:"bill_tx"`
	Acq    int64  `json:"acq"`
	Denied int64  `json:"denied"`
	Nodes  int64  `json:"nodes"`
}, error) {
	if s.db == nil {
		return nil, nil
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		Addr   string `json:"addr"`
		RX     uint64 `json:"rx"`
		TX     uint64 `json:"tx"`
		BillRX uint64 `json:"bill_rx"`
		BillTX uint64 `json:"bill_tx"`
		Acq    int64  `json:"acq"`
		Denied int64  `json:"denied"`
		Nodes  int64  `json:"nodes"`
	}
	for rows.Next() {
		var r struct {
			Addr   string `json:"addr"`
			RX     uint64 `json:"rx"`
			TX     uint64 `json:"tx"`
			BillRX uint64 `json:"bill_rx"`
			BillTX uint64 `json:"bill_tx"`
			Acq    int64  `json:"acq"`
			Denied int64  `json:"denied"`
			Nodes  int64  `json:"nodes"`
		}
		if err := rows.Scan(&r.Addr, &r.RX, &r.TX, &r.BillRX, &r.BillTX, &r.Acq, &r.Denied, &r.Nodes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func safeOrder(col, order string) string {
	valid := map[string]bool{
		"rx+tx": true, "rx": true, "tx": true,
		"bill_rx": true, "bill_tx": true,
		"acq": true, "denied": true,
	}
	if col == "" || !valid[col] {
		col = "rx+tx"
	}
	o := "DESC"
	if order == "ASC" {
		o = "ASC"
	}
	if col == "rx+tx" {
		return "SUM(rx)+SUM(tx) " + o
	}
	return "SUM(" + col + ") " + o
}

// Proxy history: per-proxy time series, optionally split by node.
// GET /api/proxies/history?addr=host:port&window=24h&split=node
func handleProxiesHistory(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		addr := r.URL.Query().Get("addr")
		if addr == "" {
			http.Error(w, "addr required", 400)
			return
		}
		hours, ok := parseWindow(r.URL.Query().Get("window"))
		if !ok {
			hours = 24
		}
		split := r.URL.Query().Get("split") == "node"

		type point struct {
			TS  int64  `json:"ts"`
			RX  uint64 `json:"rx"`
			TX  uint64 `json:"tx"`
			Acq int64  `json:"acq"`
			Den int64  `json:"den"`
		}
		type series struct {
			Node   string  `json:"node,omitempty"`
			Points []point `json:"points"`
		}

		var proxyID int64
		if err := s.db.QueryRow(`SELECT id FROM proxies WHERE addr = ?`, addr).Scan(&proxyID); err != nil {
			http.Error(w, "proxy not found", 404)
			return
		}

		if hours <= 90*24 {
			since := timeNowHour() - int64(hours)
			var query string
			var args []any
			if split {
				query = `SELECT node_id, hour, rx, tx, acq, denied
					FROM proxy_node_hourly WHERE proxy_id = ? AND hour >= ?
					ORDER BY hour ASC`
				args = []any{proxyID, since}
			} else {
				query = `SELECT '', hour, SUM(rx), SUM(tx), SUM(acq), SUM(denied)
					FROM proxy_node_hourly WHERE proxy_id = ? AND hour >= ?
					GROUP BY hour ORDER BY hour ASC`
				args = []any{proxyID, since}
			}
			seriesMap := map[string]*series{}
			ordered := []string{}
			rows, err := s.db.Query(query, args...)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			for rows.Next() {
				var nodeID string
				var hour int64
				var rx, tx uint64
				var acq, den int64
				if err := rows.Scan(&nodeID, &hour, &rx, &tx, &acq, &den); err != nil {
					rows.Close()
					http.Error(w, err.Error(), 500)
					return
				}
				sr, ok := seriesMap[nodeID]
				if !ok {
					seriesMap[nodeID] = &series{Node: nodeID}
					sr = seriesMap[nodeID]
					ordered = append(ordered, nodeID)
				}
				sr.Points = append(sr.Points, point{TS: hour * 3600, RX: rx, TX: tx, Acq: acq, Den: den})
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			out := make([]series, 0, len(ordered))
			for _, id := range ordered {
				out = append(out, *seriesMap[id])
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"series": out})
		} else {
			since := timeNowHour()/24 - int64(hours/24)
			var query string
			var args []any
			if split {
				query = `SELECT node_id, day, rx, tx, acq, denied
					FROM proxy_node_daily WHERE proxy_id = ? AND day >= ?
					ORDER BY day ASC`
				args = []any{proxyID, since}
			} else {
				query = `SELECT '', day, SUM(rx), SUM(tx), SUM(acq), SUM(denied)
					FROM proxy_fleet_daily WHERE proxy_id = ? AND day >= ?
					GROUP BY day ORDER BY day ASC`
				args = []any{proxyID, since}
			}
			seriesMap := map[string]*series{}
			ordered := []string{}
			rows, err := s.db.Query(query, args...)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			for rows.Next() {
				var nodeID string
				var day int64
				var rx, tx uint64
				var acq, den int64
				if err := rows.Scan(&nodeID, &day, &rx, &tx, &acq, &den); err != nil {
					rows.Close()
					http.Error(w, err.Error(), 500)
					return
				}
				sr, ok := seriesMap[nodeID]
				if !ok {
					seriesMap[nodeID] = &series{Node: nodeID}
					sr = seriesMap[nodeID]
					ordered = append(ordered, nodeID)
				}
				sr.Points = append(sr.Points, point{TS: day * 86400, RX: rx, TX: tx, Acq: acq, Den: den})
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			out := make([]series, 0, len(ordered))
			for _, id := range ordered {
				out = append(out, *seriesMap[id])
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"series": out})
		}
	}
}

// Best Proxies hall-of-fame.
// GET /api/proxies/best?limit=200&hide_dead=false
// Composite score = win% × LN(1 + traffic), min sample (acq+denied) >= 20.
func handleProxiesBest(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit := 200
		if l := r.URL.Query().Get("limit"); l != "" {
			if v, err := strconv.Atoi(l); err == nil && v > 0 && v <= 500 {
				limit = v
			}
		}
		hideDead := r.URL.Query().Get("hide_dead") == "true"

		// Days are the natural aggregation unit for this all-time view.
		currentDay := timeNowHour() / 24
		dayCutoff := int64(0) // all time - proxy_fleet_daily is never pruned

		type row struct {
			Addr    string  `json:"addr"`
			Traffic uint64  `json:"traffic"`
			Acq     int64   `json:"acq"`
			Denied  int64   `json:"denied"`
			LastDay int64   `json:"last_day"`
			WinPct  float64 `json:"win_pct"`
			Score   float64 `json:"score"`
			Status  string  `json:"status"` // "active" or "dead Nd ago"
		}

		rows, err := s.db.Query(`
			SELECT p.addr,
			       (SUM(f.rx)+SUM(f.tx)) AS traffic,
			       SUM(f.acq) AS acq, SUM(f.denied) AS denied,
			       MAX(f.day) AS last_day,
			       (CAST(SUM(f.acq) AS REAL) / (SUM(f.acq)+SUM(f.denied))) AS win_pct,
			       (CAST(SUM(f.acq) AS REAL) / (SUM(f.acq)+SUM(f.denied))) * LN(1 + SUM(f.rx)+SUM(f.tx)) AS score
			FROM proxy_fleet_daily f JOIN proxies p ON p.id = f.proxy_id
			WHERE f.day >= ?
			GROUP BY f.proxy_id
			HAVING (SUM(f.acq)+SUM(f.denied)) >= 20
			ORDER BY score DESC
			LIMIT ?`, dayCutoff, limit)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		var out []row
		for rows.Next() {
			var r row
			var traffic int64
			if err := rows.Scan(&r.Addr, &traffic, &r.Acq, &r.Denied, &r.LastDay, &r.WinPct, &r.Score); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			r.Traffic = uint64(traffic)
			r.WinPct *= 100
			daysAgo := currentDay - r.LastDay
			if daysAgo <= 2 {
				r.Status = "active"
			} else {
				r.Status = "dead " + strconv.Itoa(int(daysAgo)) + "d ago"
			}
			if hideDead && r.Status != "active" {
				continue
			}
			out = append(out, r)
		}

		if err := rows.Err(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}
}

// Node contract history: won/denied series from node_hourly.
// GET /api/nodes/contracts?node=ID&window=7d
func handleNodeContracts(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		node := r.URL.Query().Get("node")
		if node == "" {
			http.Error(w, "node required", 400)
			return
		}
		hours, ok := parseWindow(r.URL.Query().Get("window"))
		if !ok {
			hours = 168
		}
		since := timeNowHour() - int64(hours)

		rows, err := s.db.Query(`
			SELECT hour, COALESCE(contracts_acquired,0), COALESCE(contracts_denied,0)
			FROM node_hourly
			WHERE node_id = ? AND hour >= ?
			ORDER BY hour ASC`, node, since)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer rows.Close()

		type contractPoint struct {
			TS  int64 `json:"ts"`
			Acq int64 `json:"acq"`
			Den int64 `json:"denied"`
		}
		var out []contractPoint
		for rows.Next() {
			var hour, acq, den int64
			if err := rows.Scan(&hour, &acq, &den); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			out = append(out, contractPoint{TS: hour * 3600, Acq: acq, Den: den})
		}
		if err := rows.Err(); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)
	}
}

func timeNowHour() int64 { return nowFunc().Unix() / 3600 }

var nowFunc = func() time.Time { return time.Now().UTC() }
