package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// Retention windows. Proxy snapshots are bulky (one gzipped blob per node per
// tick) so they roll off after a week; the hourly rollups are tiny and kept
// for a year so the dashboard can show long-range history.
const (
	proxySnapshotRetention = 7 * 24 * time.Hour
	nodeHourlyRetention    = 365 * 24 * time.Hour
)

const schema = `
CREATE TABLE IF NOT EXISTS nodes (
  node_id  TEXT PRIMARY KEY,
  host     TEXT,
  version  TEXT,
  uptime   REAL,
  heap_mib INTEGER,
  sys_mib  INTEGER,
  conns    INTEGER,
  ts       INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS proxy_snapshots (
  node_id TEXT NOT NULL,
  ts      INTEGER NOT NULL,
  data    BLOB NOT NULL,            -- gzipped JSON of the []proxyReport array
  PRIMARY KEY (node_id, ts)
);
CREATE INDEX IF NOT EXISTS idx_proxy_snapshots_ts ON proxy_snapshots(ts);

CREATE TABLE IF NOT EXISTS node_hourly (
  node_id      TEXT NOT NULL,
  hour         INTEGER NOT NULL,    -- unix epoch hour (unix / 3600)
  total_rx     INTEGER,
  total_tx     INTEGER,
  bill_rx      INTEGER,
  bill_tx      INTEGER,
  peak_clients INTEGER,
  samples      INTEGER,
  PRIMARY KEY (node_id, hour)
);
CREATE INDEX IF NOT EXISTS idx_node_hourly_hour ON node_hourly(hour);
`

// openDB opens (creating if needed) the hub SQLite database at path with WAL
// journaling and synchronous=NORMAL. WAL lets the dashboard read concurrently
// with the steady trickle of report writes; NORMAL is the standard durability
// tradeoff for WAL (safe across app crashes, only risks the last txns on an OS
// crash, which for best-effort fleet telemetry is fine).
func openDB(path string) (*sql.DB, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create schema: %w", err)
	}
	return db, nil
}

// gzipJSON marshals v to JSON and gzip-compresses it. Proxy lists are highly
// repetitive (thousands of near-identical rows) so gzip shrinks a snapshot by
// roughly an order of magnitude before it hits the blob column.
func gzipJSON(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// gunzipJSON reverses gzipJSON into out.
func gunzipJSON(data []byte, out any) error {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// proxyTotals sums the per-proxy counters of a snapshot into node-level totals
// plus the peak client count. The per-proxy TotalRX/TX are cumulative since the
// proxy started, so the sums are a cumulative gauge for the node.
func proxyTotals(proxies []proxyReport) (totalRX, totalTX, billRX, billTX uint64, peakClients int64) {
	for _, p := range proxies {
		totalRX += p.TotalRX
		totalTX += p.TotalTX
		billRX += p.BillRX
		billTX += p.BillTX
		if p.Clients > peakClients {
			peakClients = p.Clients
		}
	}
	return
}

// persist writes a single report to the database: the current node row, a
// gzipped proxy snapshot, and the current hour's rollup. It is called from
// store.upsert while holding s.mu, after the in-memory cache has been updated.
func (s *store) persist(state *nodeState) error {
	if s.db == nil {
		return nil
	}
	ts := state.Timestamp.Unix()
	totalRX, totalTX, billRX, billTX, peakClients := proxyTotals(state.Proxies)

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`
		INSERT INTO nodes (node_id, host, version, uptime, heap_mib, sys_mib, conns, ts)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id) DO UPDATE SET
			host=excluded.host, version=excluded.version, uptime=excluded.uptime,
			heap_mib=excluded.heap_mib, sys_mib=excluded.sys_mib,
			conns=excluded.conns, ts=excluded.ts`,
		state.NodeID, state.Host, state.Version, state.Uptime,
		state.System.HeapMiB, state.System.SysMiB, state.System.Connections, ts,
	); err != nil {
		return fmt.Errorf("upsert node: %w", err)
	}

	blob, err := gzipJSON(state.Proxies)
	if err != nil {
		return fmt.Errorf("gzip snapshot: %w", err)
	}
	if _, err := tx.Exec(`
		INSERT INTO proxy_snapshots (node_id, ts, data) VALUES (?, ?, ?)
		ON CONFLICT(node_id, ts) DO UPDATE SET data=excluded.data`,
		state.NodeID, ts, blob,
	); err != nil {
		return fmt.Errorf("insert snapshot: %w", err)
	}

	// Rollup for the hour this report lands in. Totals reflect the latest
	// snapshot (cumulative gauge); peak_clients takes the max seen this hour;
	// samples counts how many reports contributed. Recalculating from the
	// latest snapshot rather than tracking deltas keeps this correct even if a
	// node restarts and its cumulative counters reset.
	hour := ts / 3600
	if _, err := tx.Exec(`
		INSERT INTO node_hourly
			(node_id, hour, total_rx, total_tx, bill_rx, bill_tx, peak_clients, samples)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1)
		ON CONFLICT(node_id, hour) DO UPDATE SET
			total_rx=excluded.total_rx, total_tx=excluded.total_tx,
			bill_rx=excluded.bill_rx, bill_tx=excluded.bill_tx,
			peak_clients=MAX(node_hourly.peak_clients, excluded.peak_clients),
			samples=node_hourly.samples + 1`,
		state.NodeID, hour, totalRX, totalTX, billRX, billTX, peakClients,
	); err != nil {
		return fmt.Errorf("upsert rollup: %w", err)
	}

	return tx.Commit()
}

// deleteFromDB removes a node and all of its history. Used by the dashboard's
// node-remove action.
func (s *store) deleteFromDB(nodeID string) error {
	if s.db == nil {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM nodes WHERE node_id = ?`,
		`DELETE FROM proxy_snapshots WHERE node_id = ?`,
		`DELETE FROM node_hourly WHERE node_id = ?`,
	} {
		if _, err := tx.Exec(q, nodeID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// loadLatestFromDB rebuilds the in-memory cache from the most recent snapshot
// of each known node, so a hub restart shows the last state immediately instead
// of an empty dashboard until the next round of reports. Stale nodes whose last
// report is older than the proxy-snapshot retention window are skipped.
func (s *store) loadLatestFromDB() error {
	if s.db == nil {
		return nil
	}
	rows, err := s.db.Query(`SELECT node_id, host, version, uptime, heap_mib, sys_mib, conns, ts FROM nodes`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type nodeRow struct {
		id, host, version          string
		uptime                     float64
		heap, sys                  uint64
		conns, ts                  int64
	}
	var nrows []nodeRow
	for rows.Next() {
		var n nodeRow
		if err := rows.Scan(&n.id, &n.host, &n.version, &n.uptime, &n.heap, &n.sys, &n.conns, &n.ts); err != nil {
			return err
		}
		nrows = append(nrows, n)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	for _, n := range nrows {
		var blob []byte
		err := s.db.QueryRow(
			`SELECT data FROM proxy_snapshots WHERE node_id = ? ORDER BY ts DESC LIMIT 1`,
			n.id,
		).Scan(&blob)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return err
		}
		var proxies []proxyReport
		if err := gunzipJSON(blob, &proxies); err != nil {
			return err
		}
		state := &nodeState{
			NodeID:    n.id,
			Host:      n.host,
			Version:   n.version,
			Timestamp: time.Unix(n.ts, 0).UTC(),
			Uptime:    n.uptime,
			Proxies:   proxies,
			System:    systemMetrics{HeapMiB: n.heap, SysMiB: n.sys, Connections: n.conns},
		}
		s.Nodes[n.id] = state

		// Seed the rate baseline so the first post-restart report can compute a
		// rate instead of starting from zero.
		totalRX, totalTX, _, _, _ := proxyTotals(proxies)
		s.rates[n.id] = &nodeRate{ts: state.Timestamp, rx: totalRX, tx: totalTX}
	}
	return nil
}

// importJSON loads a legacy hub.json into the database once during migration.
// It writes each node through persist so the rollups and snapshots are seeded
// from whatever last state the JSON held.
func (s *store) importJSON(jsonPath string) (int, error) {
	data, err := os.ReadFile(jsonPath)
	if err != nil {
		return 0, err
	}
	var legacy struct {
		Nodes map[string]*nodeState `json:"nodes"`
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return 0, err
	}
	n := 0
	for id, state := range legacy.Nodes {
		if state == nil {
			continue
		}
		state.NodeID = id
		if state.Timestamp.IsZero() {
			state.Timestamp = time.Now().UTC()
		}
		if err := s.persist(state); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// hourlyRow is one bucket of a node's rollup history, returned by /api/history.
type hourlyRow struct {
	Hour        int64  `json:"hour"` // unix epoch seconds at the start of the hour
	TotalRX     uint64 `json:"total_rx"`
	TotalTX     uint64 `json:"total_tx"`
	BillRX      uint64 `json:"bill_rx"`
	BillTX      uint64 `json:"bill_tx"`
	PeakClients int64  `json:"peak_clients"`
	Samples     int64  `json:"samples"`
}

// history returns up to the last `hours` hourly rollups for a node (or all
// nodes when nodeID is empty), most recent first.
func (s *store) history(nodeID string, hours int) ([]hourlyRow, error) {
	if s.db == nil {
		return nil, nil
	}
	if hours <= 0 {
		hours = 24
	}
	since := time.Now().Add(-time.Duration(hours) * time.Hour).Unix() / 3600

	var (
		rows *sql.Rows
		err  error
	)
	if nodeID == "" {
		rows, err = s.db.Query(`
			SELECT hour, total_rx, total_tx, bill_rx, bill_tx, peak_clients, samples
			FROM node_hourly WHERE hour >= ? ORDER BY hour DESC`, since)
	} else {
		rows, err = s.db.Query(`
			SELECT hour, total_rx, total_tx, bill_rx, bill_tx, peak_clients, samples
			FROM node_hourly WHERE node_id = ? AND hour >= ? ORDER BY hour DESC`, nodeID, since)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []hourlyRow{}
	for rows.Next() {
		var h hourlyRow
		var hour int64
		if err := rows.Scan(&hour, &h.TotalRX, &h.TotalTX, &h.BillRX, &h.BillTX, &h.PeakClients, &h.Samples); err != nil {
			return nil, err
		}
		h.Hour = hour * 3600
		out = append(out, h)
	}
	return out, rows.Err()
}

// pruneSnapshots / pruneHourly enforce the retention windows. They run on their
// own cadences from startRetention.
func (s *store) pruneSnapshots() (int64, error) {
	if s.db == nil {
		return 0, nil
	}
	cutoff := time.Now().Add(-proxySnapshotRetention).Unix()
	res, err := s.db.Exec(`DELETE FROM proxy_snapshots WHERE ts < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *store) pruneHourly() (int64, error) {
	if s.db == nil {
		return 0, nil
	}
	cutoff := time.Now().Add(-nodeHourlyRetention).Unix() / 3600
	res, err := s.db.Exec(`DELETE FROM node_hourly WHERE hour < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// startRetention launches the background pruning loops: snapshots hourly,
// hourly rollups daily. A small startup jitter avoids hammering the DB right at
// boot. Both stop when ctx is cancelled.
func (s *store) startRetention(ctx context.Context) {
	go retentionLoop(ctx, time.Hour, "proxy_snapshots", s.pruneSnapshots)
	go retentionLoop(ctx, 24*time.Hour, "node_hourly", s.pruneHourly)
}

func retentionLoop(ctx context.Context, every time.Duration, label string, prune func() (int64, error)) {
	// jitter the first run so the two loops don't fire simultaneously on boot
	select {
	case <-ctx.Done():
		return
	case <-time.After(time.Duration(rand.Int63n(int64(time.Minute)))):
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		if n, err := prune(); err != nil {
			fmt.Printf("retention(%s): %v\n", label, err)
		} else if n > 0 {
			fmt.Printf("retention(%s): pruned %d rows\n", label, n)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
