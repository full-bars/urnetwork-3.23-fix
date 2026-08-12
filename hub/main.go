package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

var Version string

// trustedProxyNets holds the CIDR ranges (from URNETWORK_HUB_TRUSTED_PROXIES)
// that clientIP will accept X-Forwarded-For/X-Real-IP from. Left empty by
// default, which means "trust nothing" — X-Forwarded-For is otherwise
// entirely attacker-controlled (any client can send it) and is the sole key
// for the PAKE join rate limiter, so honoring it unconditionally lets an
// attacker put a fresh value on every request and never hit the limit.
var trustedProxyNets []*net.IPNet

func setTrustedProxies(csv string) {
	trustedProxyNets = nil
	for _, rawEntry := range strings.Split(csv, ",") {
		entry := strings.TrimSpace(rawEntry)
		if entry == "" {
			continue
		}
		if !strings.Contains(entry, "/") {
			if ip := net.ParseIP(entry); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				entry = fmt.Sprintf("%s/%d", entry, bits)
			}
		}
		_, ipNet, err := net.ParseCIDR(entry)
		if err != nil {
			// Log the operator's original entry, not the CIDR-mask-appended
			// rewrite, so an invalid value is easy to match against config.
			fmt.Fprintf(os.Stderr, "hub: ignoring invalid URNETWORK_HUB_TRUSTED_PROXIES entry %q: %v\n", strings.TrimSpace(rawEntry), err)
			continue
		}
		trustedProxyNets = append(trustedProxyNets, ipNet)
	}
}

func remoteIPTrusted(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range trustedProxyNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP extracts the real client IP when the hub sits behind a reverse
// proxy (Caddy, nginx). X-Forwarded-For/X-Real-IP are only honored when the
// direct TCP peer (r.RemoteAddr) is in trustedProxyNets — otherwise a client
// can forge either header directly and spoof its apparent identity, which
// matters both for the dashboard's SourceIP display and, more seriously, for
// rate-limiting keyed on this value (see checkJoinRateLimit).
func clientIP(r *http.Request) string {
	if remoteIPTrusted(r.RemoteAddr) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Rightmost entry is the one the trusted proxy itself appended;
			// entries to its left may be attacker-supplied.
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
		if xri := r.Header.Get("X-Real-IP"); xri != "" {
			return strings.TrimSpace(xri)
		}
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return ip
}

// gzipMiddleware transparently compresses responses when the client
// sends Accept-Encoding: gzip. Skips SSE endpoints since gzip buffering
// breaks the streaming response flush pattern.
func gzipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/events" {
			next.ServeHTTP(w, r)
			return
		}
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

var funcMap = template.FuncMap{
	"fmtBytes": fmtBytes,
	"fmtMbps":  fmtMbps,
	"title":    title,
	"fmtAge":   fmtAge,
	"pct": func(a, b int) float64 {
		if b == 0 {
			return 0
		}
		return float64(a) / float64(b) * 100
	},
	"pct64": func(a, b int64) float64 {
		if b == 0 {
			return 0
		}
		return float64(a) / float64(b) * 100
	},
	"add":   func(a, b float64) float64 { return a + b },
	"add64": func(a, b int64) int64 { return a + b },
}

type proxyReport struct {
	ID                string `json:"id"`
	Address           string `json:"addr"`
	Status            string `json:"status"`
	TotalRX           uint64 `json:"rx"`
	TotalTX           uint64 `json:"tx"`
	BillRX            uint64 `json:"bill_rx"`
	BillTX            uint64 `json:"bill_tx"`
	Clients           int64  `json:"clients"`
	MaxAge            int64  `json:"max_age_s"`
	ContractsAcquired int64  `json:"contracts_acquired"`
	ContractsDenied   int64  `json:"contracts_denied"`

	// A-F grade surfaced from the provider's grading pipeline (roadmap
	// step 3). Mirrors the provider report payload; only present when the
	// proxy has been graded.
	Score      float64  `json:"score,omitempty"`
	Graded     bool     `json:"graded,omitempty"`
	Failed     []string `json:"failed,omitempty"`
	Tier       string   `json:"tier,omitempty"`
	LastGraded int64    `json:"last_graded,omitempty"` // unix ts, 0 = never
}

type systemMetrics struct {
	HeapMiB     uint64 `json:"heap_mib"`
	SysMiB      uint64 `json:"sys_mib"`
	Connections int64  `json:"conns"`
}

type nodeState struct {
	NodeID    string        `json:"node_id"`
	Host      string        `json:"host"`
	Version   string        `json:"version"`
	Timestamp time.Time     `json:"ts"`
	Uptime    float64       `json:"uptime"`
	SourceIP  string        `json:"source_ip"`
	TLS       bool          `json:"tls"`
	Proxies   []proxyReport `json:"proxies"`
	System    systemMetrics `json:"sys"`
}

type store struct {
	mu           sync.RWMutex
	db           *sql.DB
	Nodes        map[string]*nodeState        `json:"nodes"`
	rates        map[string]*nodeRate         `json:"-"`
	prevBillable map[string]map[string]uint64 `json:"-"` // nodeID -> proxyID -> last seen BillRX+BillTX
	earning      map[string]map[string]bool   `json:"-"` // nodeID -> proxyID -> earning=yes/no
	proxyIDs     map[string]int64             `json:"-"` // proxy addr -> interned proxies.id
	deltas       *deltaTracker                `json:"-"` // cumulative -> per-interval counters
	broadcast    *broadcaster                 `json:"-"` // live-update SSE fan-out; nil-safe, see broadcaster.publish
}

// proxyStatus is the compact per-proxy fields a heartbeat carries — status
// and contract counters only, no byte-level detail. json tags must match
// provider/bandwidth_reporter.go's proxyStatus.
type proxyStatus struct {
	ID                string `json:"id"`
	Status            string `json:"status"`
	ContractsAcquired int64  `json:"contracts_acquired"`
	ContractsDenied   int64  `json:"contracts_denied"`
}

// heartbeatReport is the lightweight, high-frequency (10-30s) counterpart to
// bandwidthReport (provider/bandwidth_reporter.go): no byte-level detail,
// just enough to keep the dashboard's "last seen", Mbps rate, and per-proxy
// status/contracts live between the much less frequent full /api/report
// ticks. Never persisted to DB. Proxies is sparse — the provider only
// includes entries that changed since its last heartbeat tick (see
// filterChangedProxies in provider/bandwidth_reporter.go), so most ticks
// carry an empty or near-empty slice.
type heartbeatReport struct {
	NodeID    string        `json:"node_id"`
	Timestamp time.Time     `json:"ts"`
	Uptime    float64       `json:"uptime"`
	TotalRX   uint64        `json:"rx"`
	TotalTX   uint64        `json:"tx"`
	Clients   int64         `json:"clients"`
	System    systemMetrics `json:"sys"`
	Proxies   []proxyStatus `json:"proxies,omitempty"`
}

type nodeRate struct {
	ts     time.Time
	rx     uint64
	tx     uint64
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
		proxyIDs:     make(map[string]int64),
		deltas:       newDeltaTracker(),
		broadcast:    newBroadcaster(),
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

// hashCredential returns the SHA-256 hex digest of a bearer credential. Only
// the hash is ever persisted or compared against — the raw session key
// (unlike an ephemeral AKE key, this one lives on as a long-standing bearer
// secret) never touches disk or a SQL equality clause in the clear.
func hashCredential(credentialHex string) string {
	sum := sha256.Sum256([]byte(credentialHex))
	return hex.EncodeToString(sum[:])
}

// storeCredential saves a per-node credential after successful PAKE join.
// The credential is the hex-encoded session key; only its hash is stored.
func (s *store) storeCredential(nodeID, credentialHex string) error {
	if s.db == nil {
		return nil
	}
	_, err := s.db.Exec(`
		INSERT INTO node_credentials (node_id, credential_hash, created_at)
		VALUES (?, ?, strftime('%s','now'))
		ON CONFLICT(node_id) DO UPDATE SET
			credential_hash = excluded.credential_hash,
			created_at = strftime('%s','now'),
			revoked_at = NULL`,
		nodeID, hashCredential(credentialHex))
	return err
}

// validateCredential checks if a credential hex string matches an active
// (non-revoked) entry, and if so returns the node_id it was issued to. A v2
// credential only proves "some registered node" — callers MUST check the
// returned node_id against the node_id acted upon in the request, or a
// compromised node can act on behalf of any other node.
func (s *store) validateCredential(ctx context.Context, credentialHex string) (bool, string, error) {
	if s.db == nil {
		return false, "", nil
	}
	var nodeID string
	err := s.db.QueryRowContext(ctx, `
		SELECT node_id FROM node_credentials
		WHERE credential_hash = ? AND revoked_at IS NULL`, hashCredential(credentialHex)).Scan(&nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	return true, nodeID, nil
}

// revokeCredential marks a node's credential as revoked. Used for node removal.
func (s *store) revokeCredential(nodeID string) error {
	if s.db == nil {
		return nil
	}
	_, err := s.db.Exec(`UPDATE node_credentials SET revoked_at = strftime('%s','now') WHERE node_id = ?`, nodeID)
	return err
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

// heartbeat applies a lightweight, high-frequency ping to a node that has
// already been established by a full /api/report: it refreshes the
// freshness timestamp and Mbps rate the same way upsert does, but never
// touches Proxies (heartbeats carry no per-proxy detail) and never persists
// to DB (see persist() in store_db.go — this intentionally bypasses it).
// Returns false, and does nothing, for a node ID that has never sent a full
// report yet, since there's no proxy list to show on the dashboard.
func (s *store) heartbeat(nodeID string, hb *heartbeatReport) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	n, ok := s.Nodes[nodeID]
	if !ok {
		return false
	}

	if prev, ok := s.rates[nodeID]; ok {
		dt := hb.Timestamp.Sub(prev.ts).Seconds()
		if dt > 1 && hb.TotalRX >= prev.rx && hb.TotalTX >= prev.tx {
			prev.mbpsRx = float64(hb.TotalRX-prev.rx) / dt * 8 / 1_000_000
			prev.mbpsTx = float64(hb.TotalTX-prev.tx) / dt * 8 / 1_000_000
		}
		prev.ts = hb.Timestamp
		prev.rx = hb.TotalRX
		prev.tx = hb.TotalTX
	} else {
		s.rates[nodeID] = &nodeRate{ts: hb.Timestamp, rx: hb.TotalRX, tx: hb.TotalTX}
	}

	if len(hb.Proxies) > 0 {
		byID := make(map[string]int, len(n.Proxies))
		for i, p := range n.Proxies {
			byID[p.ID] = i
		}
		for _, ps := range hb.Proxies {
			if i, ok := byID[ps.ID]; ok {
				n.Proxies[i].Status = ps.Status
				n.Proxies[i].ContractsAcquired = ps.ContractsAcquired
				n.Proxies[i].ContractsDenied = ps.ContractsDenied
			}
		}
	}

	n.Timestamp = hb.Timestamp
	n.Uptime = hb.Uptime
	n.System = hb.System
	return true
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
	TotalClients                          int64
	TotalRX, TotalTX                      uint64
	BillRX, BillTX                        uint64
	Earning, TotalProxies                 int
	MbpsRX, MbpsTX                        float64
	ContractsAcquired, ContractsDenied    int64
}

type proxSummary struct {
	Up, Connecting, Degraded, Dead     int
	Clients                            int64
	TotalRX, TotalTX                   uint64
	BillRX, BillTX                     uint64
	Earning                            int
	ContractsAcquired, ContractsDenied int64
}

// proxyRow pairs a reported proxy with its hub-computed earning state for
// template rendering. proxyReport itself stays a plain decode/persist target.
type proxyRow struct {
	proxyReport
	Earning bool
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
	ProxyList []proxyRow
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

// ctxAuthNodeID carries the node_id a v2 credential was issued to, so
// handlers can reject requests whose body targets a different node_id. Only
// set for v2 (per-node) auth; a v1 static token is treated as admin and
// grants unrestricted access, matching prior behavior.
type ctxKey int

const ctxAuthNodeID ctxKey = iota

// authNodeID returns the node_id bound to this request by a v2 credential,
// and whether one was set at all (false for v1 static-token/admin requests,
// which are unrestricted).
func authNodeID(r *http.Request) (string, bool) {
	id, ok := r.Context().Value(ctxAuthNodeID).(string)
	return id, ok
}

func requireAuth(token string, next http.HandlerFunc, validateV2 func(context.Context, string) (bool, string, error)) http.HandlerFunc {
	if token == "" && validateV2 == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !strings.HasPrefix(authHeader, prefix) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		given := strings.TrimPrefix(authHeader, prefix)

		// Check v1 static token first
		if token != "" && subtle.ConstantTimeCompare([]byte(given), []byte(token)) == 1 {
			next(w, r)
			return
		}

		// Check v2 per-node credential
		if validateV2 != nil {
			ok, nodeID, err := validateV2(r.Context(), given)
			if err == nil && ok {
				r = r.WithContext(context.WithValue(r.Context(), ctxAuthNodeID, nodeID))
				next(w, r)
				return
			}
		}

		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}
}

// requireBasicAuth gates the dashboard and read-only API behind HTTP Basic
// Auth. Any username is accepted — only the password is checked — since the
// goal is to keep the fleet dashboard from being viewable by anyone who
// merely knows the hub's address, not to distinguish individual operators.
func requireBasicAuth(pass string, next http.HandlerFunc) http.HandlerFunc {
	if pass == "" {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		_, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="urnetwork-hub"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func handleReport(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		var ns nodeState
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
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
		if authID, restricted := authNodeID(r); restricted && authID != ns.NodeID {
			http.Error(w, "forbidden: credential does not match node_id", 403)
			return
		}
		ns.Timestamp = time.Now().UTC()
		ns.SourceIP = clientIP(r)
		ns.TLS = r.TLS != nil
		fmt.Printf("report from %s: %d proxies\n", ns.NodeID, len(ns.Proxies))
		s.upsert(ns.NodeID, &ns)
		s.broadcast.publish()
		w.WriteHeader(204)
	}
}

// handleHeartbeat serves the lightweight companion to /api/report (see
// heartbeatReport). A 202 for an unknown node tells the provider "received,
// but I have no baseline for you yet" without it being treated as an error
// worth retry-storming over — the provider's own report loop will establish
// the node on its next full-report tick.
func handleHeartbeat(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", 405)
			return
		}
		var hb heartbeatReport
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<16))
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if err := json.Unmarshal(body, &hb); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if hb.NodeID == "" {
			http.Error(w, "missing node_id", 400)
			return
		}
		if authID, restricted := authNodeID(r); restricted && authID != hb.NodeID {
			http.Error(w, "forbidden: credential does not match node_id", 403)
			return
		}
		hb.Timestamp = time.Now().UTC()
		if !s.heartbeat(hb.NodeID, &hb) {
			w.WriteHeader(202)
			return
		}
		s.broadcast.publish()
		w.WriteHeader(204)
	}
}

func handleNodes(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Support /api/nodes/<id>/proxies for lazy-loaded proxy detail rows
		path := strings.TrimPrefix(r.URL.Path, "/api/nodes/")
		if path != "" && path != r.URL.Path {
			if strings.HasSuffix(path, "/proxies") {
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
			// Unknown subpath
			http.NotFound(w, r)
			return
		}
		// Return lightweight node list with aggregate counts (no proxy details)
		type nodeSummary struct {
			NodeID            string        `json:"node_id"`
			Host              string        `json:"host"`
			Version           string        `json:"version"`
			Timestamp         time.Time     `json:"ts"`
			Uptime            float64       `json:"uptime"`
			SourceIP          string        `json:"source_ip"`
			TLS               bool          `json:"tls"`
			Proxies           int           `json:"proxies"`
			Up                int           `json:"up"`
			Connecting        int           `json:"connecting"`
			Degraded          int           `json:"degraded"`
			Dead              int           `json:"dead"`
			Earning           int           `json:"earning"`
			Clients           int64         `json:"clients"`
			RX                uint64        `json:"rx"`
			TX                uint64        `json:"tx"`
			BillRX            uint64        `json:"bill_rx"`
			BillTX            uint64        `json:"bill_tx"`
			ContractsAcquired int64         `json:"contracts_acquired"`
			ContractsDenied   int64         `json:"contracts_denied"`
			MbpsRX            float64       `json:"mbps_rx"`
			MbpsTX            float64       `json:"mbps_tx"`
			System            systemMetrics `json:"sys"`
		}
		nodes := s.list()
		out := make([]nodeSummary, 0, len(nodes))
		for _, n := range nodes {
			var up, connecting, degraded, dead int
			var cAcquired, cDenied int64
			var totalRX, totalTX, billRX, billTX uint64
			var clients int64
			for _, p := range n.Proxies {
				switch p.Status {
				case "up":
					up++
				case "connecting":
					connecting++
				case "degraded":
					degraded++
				default:
					dead++
				}
				cAcquired += p.ContractsAcquired
				cDenied += p.ContractsDenied
				totalRX += p.TotalRX
				totalTX += p.TotalTX
				billRX += p.BillRX
				billTX += p.BillTX
				clients += p.Clients
			}
			mbpsRX, mbpsTX := s.getRate(n.NodeID)
			earning := 0
			nodeEarning := s.getEarning(n.NodeID)
			for _, p := range n.Proxies {
				if nodeEarning[p.ID] {
					earning++
				}
			}
			out = append(out, nodeSummary{
				NodeID:            n.NodeID,
				Host:              n.Host,
				Version:           n.Version,
				Timestamp:         n.Timestamp,
				Uptime:            n.Uptime,
				SourceIP:          n.SourceIP,
				TLS:               n.TLS,
				Proxies:           len(n.Proxies),
				Up:                up,
				Connecting:        connecting,
				Degraded:          degraded,
				Dead:              dead,
				Earning:           earning,
				Clients:           clients,
				RX:                totalRX,
				TX:                totalTX,
				BillRX:            billRX,
				BillTX:            billTX,
				ContractsAcquired: cAcquired,
				ContractsDenied:   cDenied,
				MbpsRX:            mbpsRX,
				MbpsTX:            mbpsTX,
				System:            n.System,
			})
		}
		json.NewEncoder(w).Encode(out)
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
		if authID, restricted := authNodeID(r); restricted && authID != req.NodeID {
			http.Error(w, "forbidden: credential does not match node_id", 403)
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
		if err := s.revokeCredential(req.NodeID); err != nil {
			fmt.Printf("revoke %s PAKE credential: %v\n", req.NodeID, err)
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
			var ps proxSummary
			for _, p := range n.Proxies {
				ps.TotalRX += p.TotalRX
				ps.TotalTX += p.TotalTX
				ps.BillRX += p.BillRX
				ps.BillTX += p.BillTX
				ps.Clients += p.Clients
				ps.ContractsAcquired += p.ContractsAcquired
				ps.ContractsDenied += p.ContractsDenied
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
			// Accumulate fleet totals
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
			sm.ContractsAcquired += ps.ContractsAcquired
			sm.ContractsDenied += ps.ContractsDenied
			// Compute per-node earning
			ps.Earning = 0
			for _, p := range n.Proxies {
				if nodeEarning[p.ID] {
					sm.Earning++
					ps.Earning++
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
			sm.MbpsRX += mbpsRX
			sm.MbpsTX += mbpsTX

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
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.BoolVar(showVersion, "v", false, "print version and exit (alias)")

	addr := flag.String("addr", ":8080", "listen address")
	tlsAddr := flag.String("tls-addr", "", "HTTPS listen address (empty disables TLS)")
	dataDir := flag.String("data", ".", "data directory for hub.json")
	showPassword := flag.Bool("show-password", false, "print the CA password and exit")
	doMintOnboardToken := flag.Bool("mint-onboard-token", false, "mint an onboarding token and exit")
	hubJoin := flag.String("hub-join", "", "run PAKE join against a hub URL (reads password from stdin)")

	flag.Parse()
	if *showVersion {
		v := Version
		if v == "" {
			v = "dev"
		}
		fmt.Println("urnetwork-hub " + v)
		os.Exit(0)
	}

	if *showPassword {
		pwPath := filepath.Join(*dataDir, "hub.password")
		data, err := os.ReadFile(pwPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hub: no password found (run 'hub init' first): %v\n", err)
			os.Exit(1)
		}
		fmt.Println(strings.TrimSpace(string(data)))
		os.Exit(0)
	}

	if *doMintOnboardToken {
		pwPath := filepath.Join(*dataDir, "hub.password")
		if _, err := os.Stat(pwPath); os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "hub: no password found (run 'hub init' first)\n")
			os.Exit(1)
		}
		password, salt, _, err := loadOrCreateCAMaterial(*dataDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hub: CA setup failed: %v\n", err)
			os.Exit(1)
		}
		ca, err := deriveCA(password, salt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hub: CA derivation failed: %v\n", err)
			os.Exit(1)
		}
		token, expiry, err := mintOnboardToken(*dataDir, time.Now(), onboardTokenTTL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hub: mint token failed: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("Token: %s\n", token)
		fmt.Printf("Expires: %s\n", expiry.Format(time.RFC3339))
		if fp, err := ca.caFingerprint(); err == nil {
			fmt.Printf("CA fingerprint: %s\n", fp)
		}
		os.Exit(0)
	}

	if *hubJoin != "" {
		joinCtx, joinCancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer joinCancel()
		doHubJoin(joinCtx, *hubJoin)
		os.Exit(0)
	}

	hubToken := os.Getenv("URNETWORK_HUB_TOKEN")
	if hubToken == "" {
		fmt.Println("hub: WARNING URNETWORK_HUB_TOKEN not set — /api/report and /api/nodes/remove are unauthenticated")
	}

	// Comma-separated list of reverse-proxy IPs/CIDRs (e.g. "127.0.0.1,10.0.0.0/8")
	// allowed to set X-Forwarded-For/X-Real-IP. Unset means no proxy is trusted
	// and clientIP always falls back to the raw TCP peer address.
	setTrustedProxies(os.Getenv("URNETWORK_HUB_TRUSTED_PROXIES"))

	dashboardPass := os.Getenv("URNETWORK_HUB_DASHBOARD_PASS")
	if dashboardPass == "" {
		fmt.Println("hub: WARNING URNETWORK_HUB_DASHBOARD_PASS not set — dashboard and read-only API are unauthenticated")
	}

	s, err := openStore(*dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hub: open store: %v\n", err)
		os.Exit(1)
	}
	defer s.db.Close()

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	s.startRetention(ctx)

	mux := http.NewServeMux()

	var tlsSrv *http.Server

	// v2 credential validator: checks Bearer token against stored node credentials,
	// returning the node_id it was issued to so requireAuth can bind the request
	// to that node.
	validateV2 := func(ctx context.Context, credential string) (bool, string, error) {
		return s.validateCredential(ctx, credential)
	}

	mux.HandleFunc("/api/report", requireAuth(hubToken, handleReport(s), validateV2))
	mux.HandleFunc("/api/heartbeat", requireAuth(hubToken, handleHeartbeat(s), validateV2))
	mux.HandleFunc("/api/nodes/remove", requireAuth(hubToken, handleNodeRemove(s), validateV2))
	mux.HandleFunc("/api/nodes/contracts", requireBasicAuth(dashboardPass, handleNodeContracts(s)))
	mux.HandleFunc("/api/nodes/", requireBasicAuth(dashboardPass, handleNodes(s)))
	mux.HandleFunc("/api/proxies/top", requireBasicAuth(dashboardPass, handleProxiesTop(s)))
	mux.HandleFunc("/api/proxies/best", requireBasicAuth(dashboardPass, handleProxiesBest(s)))
	mux.HandleFunc("/api/proxies/history", requireBasicAuth(dashboardPass, handleProxiesHistory(s)))
	mux.HandleFunc("/api/history", requireBasicAuth(dashboardPass, handleHistory(s)))
	mux.HandleFunc("/api/events", requireBasicAuth(dashboardPass, handleEvents(s)))

	// PAKE join endpoints - auto-enabled when hub has a password
	var pakeJoinState *pakeJoinState
	pwPath := filepath.Join(*dataDir, "hub.password")
	if _, err := os.Stat(pwPath); err == nil {
		var err error
		pakeJoinState, err = initPakeJoinState(*dataDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hub: PAKE init skipped (will use v1 token): %v\n", err)
		} else {
			go joinLimiter.cleanupLoop()
			mux.HandleFunc("/api/join/ke1", joinLimiter.middleware(pakeJoinState.HandleKE1))
			mux.HandleFunc("/api/join/ke3", joinLimiter.middleware(pakeJoinState.HandleKE3(s)))
			fmt.Println("hub: PAKE join endpoints enabled at /api/join/ke1, /api/join/ke3")
		}
	}

	wrapped := gzipMiddleware(mux)

	tlsListen := *tlsAddr
	if tlsListen == "" {
		tlsListen = os.Getenv("URNETWORK_HUB_TLS_ADDR")
	}

	if tlsListen != "" {
		// CA-based TLS setup
		password, salt, generated, err := loadOrCreateCAMaterial(*dataDir)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hub: CA setup failed: %v\n", err)
			os.Exit(1)
		}
		if generated {
			fmt.Printf("hub: Generated new password. Save it now — retrieve later with 'hub show-password'\n")
		}

		ca, err := deriveCA(password, salt)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hub: CA derivation failed: %v\n", err)
			os.Exit(1)
		}
		if fp, err := ca.caFingerprint(); err == nil {
			fmt.Printf("hub: CA fingerprint %s\n", fp)
		}

		// Write CA cert (non-fatal — the hub can still serve TLS without it on disk)
		caPath := filepath.Join(*dataDir, "ca.crt")
		if err := os.WriteFile(caPath, ca.certPEM, 0644); err != nil {
			fmt.Fprintf(os.Stderr, "hub: WARNING: could not write CA cert to disk: %v\n", err)
		}

		// Issue initial leaf
		leaf, err := ca.issueLeaf(leafSANs(), leafValidHours*time.Hour)
		if err != nil {
			fmt.Fprintf(os.Stderr, "hub: initial leaf issuance failed: %v\n", err)
			os.Exit(1)
		}
		leafHolder := &atomic.Pointer[tls.Certificate]{}
		leafHolder.Store(&leaf)

		// Leaf rotation goroutine
		go func() {
			ticker := time.NewTicker(leafRotationHours * time.Hour)
			defer ticker.Stop()
			for range ticker.C {
				newLeaf, err := ca.issueLeaf(leafSANs(), leafValidHours*time.Hour)
				if err != nil {
					fmt.Fprintf(os.Stderr, "hub: leaf rotation failed: %v\n", err)
					continue
				}
				leafHolder.Store(&newLeaf)
				fmt.Printf("hub: leaf certificate rotated\n")

				// Re-write ca.crt to disk in case it was deleted mid-run.
				// Providers fetch it at /api/ca-cert, but operators
				// may copy it directly from the data dir.
				if err := os.WriteFile(caPath, ca.certPEM, 0644); err != nil {
					fmt.Fprintf(os.Stderr, "hub: WARNING: could not rewrite CA cert: %v\n", err)
				}
			}
		}()

		// Extended /api/cert endpoint
		mux.HandleFunc("/api/cert", func(w http.ResponseWriter, r *http.Request) {
			currentLeaf := leafHolder.Load()
			w.Header().Set("Content-Type", "application/json")
			caFP, _ := ca.caFingerprint()
			json.NewEncoder(w).Encode(map[string]string{
				"fingerprint":    fmt.Sprintf("SHA256:%x", sha256.Sum256(currentLeaf.Certificate[0])),
				"pem":            strings.TrimSpace(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: currentLeaf.Certificate[0]}))),
				"ca_pem":         strings.TrimSpace(string(ca.certPEM)),
				"ca_fingerprint": caFP,
			})
		})

		// CA cert endpoint for onboarding and token-based auto-bootstrap
		mux.HandleFunc("/api/ca-cert", handleCACert(*dataDir, ca, hubToken))

		// Onboard script endpoint
		_, port, _ := net.SplitHostPort(tlsListen)
		mux.HandleFunc("/onboard.sh", handleOnboardScript(port))

		tlsSrv = &http.Server{
			Addr:    tlsListen,
			Handler: wrapped,
			TLSConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
				GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
					return leafHolder.Load(), nil
				},
			},
			// ReadHeaderTimeout/IdleTimeout guard against Slowloris-style
			// connection exhaustion (dribbled headers, or opened-and-idle
			// connections). WriteTimeout is deliberately left unset — it
			// would kill the long-lived /api/events SSE stream.
			ReadHeaderTimeout: 10 * time.Second,
			IdleTimeout:       120 * time.Second,
		}
		go func() {
			fmt.Printf("hub: HTTPS listening on %s\n", tlsListen)
			if err := tlsSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "hub: HTTPS: %v\n", err)
			}
		}()
	}

	mux.HandleFunc("/", requireBasicAuth(dashboardPass, handleDashboard(s)))

	plainSrv := &http.Server{
		Addr:              *addr,
		Handler:           wrapped,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		if tlsSrv != nil {
			_ = tlsSrv.Shutdown(shutdownCtx)
		}
		_ = plainSrv.Shutdown(shutdownCtx)
	}()

	fmt.Printf("hub listening on %s (data: %s)\n", *addr, filepath.Join(*dataDir, "hub.db"))
	if err := plainSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "hub: %v\n", err)
	}
}

func pemDecodeCert(pemBytes []byte) []byte {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil
	}
	return block.Bytes
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
.layout { display: flex; min-height: calc(100vh - 40px); }
.sidebar { background: #0f172a; border-right: 1px solid #1e293b; width: 140px; flex-shrink: 0; padding: 8px 0; }
.nav-item { padding: 8px 14px; cursor: pointer; color: #64748b; font-size: 12px; font-weight: 500; user-select: none; border-left: 2px solid transparent; }
.nav-item:hover { color: #94a3b8; background: #1a2332; }
.nav-item.active { color: #60a5fa; border-left-color: #60a5fa; background: #1a2332; font-weight: 600; }
.nav-item .badge { float: right; font-size: 10px; background: #1e293b; padding: 1px 5px; border-radius: 3px; color: #94a3b8; }
.content { flex: 1; overflow-x: hidden; }
.page { display: none; }
.page.active { display: block; }
.page-header { padding: 12px 20px; font-size: 13px; font-weight: 600; color: #64748b; border-bottom: 1px solid #1e293b; text-transform: uppercase; letter-spacing: 0.05em; }
.window-pills { display: flex; gap: 6px; padding: 8px 20px; background: #0f172a; border-bottom: 1px solid #1e293b; align-items: center; }
.window-pills .pill { background: #1a2332; border: 1px solid #1e293b; color: #94a3b8; padding: 3px 10px; border-radius: 5px; font-size: 11px; cursor: pointer; user-select: none; }
.window-pills .pill:hover { border-color: #60a5fa; }
.window-pills .pill.on { background: #60a5fa; color: #fff; border-color: #60a5fa; }
.window-pills .info { margin-left: auto; font-size: 11px; color: #64748b; }
.spark { font-family: monospace; letter-spacing: -1px; font-size: 13px; }
.bar-h { height: 6px; border-radius: 3px; margin: 3px 0; }
.bar-h.g { background: linear-gradient(90deg, #60a5fa, #4ade80); }
.win-bar { display: flex; height: 7px; border-radius: 3px; overflow: hidden; margin-top: 2px; }
.win-bar .w { background: #4ade80; } .win-bar .l { background: #ef4444; }
.node-tag { background: #1a2332; border-radius: 3px; padding: 0 4px; color: #a78bfa; font-size: 10px; }
.table-section-header { padding: 8px 20px; font-size: 11px; color: #64748b; border-top: 1px solid #1e293b; cursor: pointer; user-select: none; }
.table-section-header:hover { color: #94a3b8; }
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
.grade-badge { display: inline-block; padding: 1px 6px; border-radius: 4px; font-size: 11px; font-weight: 700; min-width: 18px; text-align: center; }
.grade-none { color: #475569; font-size: 11px; }
.grade-A { background: rgba(74,222,128,0.18); color: #4ade80; }
.grade-B { background: rgba(163,230,53,0.18); color: #a3e635; }
.grade-C { background: rgba(250,204,21,0.18); color: #facc15; }
.grade-D { background: rgba(251,146,60,0.18); color: #fb923c; }
.grade-F { background: rgba(239,68,68,0.18); color: #ef4444; }
.sort-arrow { margin-left: 4px; font-size: 10px; }
.remove-btn { cursor: pointer; padding: 2px 6px; border-radius: 4px; font-size: 16px; line-height: 1; color: #64748b; }
.remove-btn:hover { background: rgba(239,68,68,0.2); color: #ef4444; }
.hidden { display: none !important; }
.drawer-overlay { position: fixed; top: 0; left: 0; right: 0; bottom: 0; background: rgba(0,0,0,0.5); z-index: 100; display: none; }
.drawer-overlay.open { display: block; }
.drawer { position: fixed; top: 0; right: -90vw; width: 90vw; max-width: 1200px; height: 100%; background: #0f172a; border-left: 1px solid #1e293b; z-index: 101; transition: right 0.25s ease; overflow-y: auto; overflow-x: hidden; display: flex; flex-direction: column; }
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
.chart-box { background: #0f172a; border-radius: 8px; border: 1px solid #1e293b; padding: 16px; overflow: hidden; }
.chart-box.compact { padding: 8px; overflow: hidden; }
.chart-box.compact .u-title { font-size: 12px !important; }
.charts-row { display: grid; grid-template-columns: repeat(auto-fill, minmax(320px, 1fr)); gap: 12px; padding: 12px 24px 4px; }
@media (max-width: 900px) { .charts-row { grid-template-columns: 1fr; padding: 8px 16px 0; } }
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
.ip-tag { display: inline-block; margin-left: 8px; padding: 0 6px; border: 1px solid; border-radius: 4px; font-size: 10px; font-weight: 500; vertical-align: middle; }
.sort-arrow { margin-left: 4px; font-size: 10px; }
</style>
</head>
<body>
<div class="layout">
<nav class="sidebar">
<div class="nav-item active" data-page="overview" onclick="switchPage('overview')">Overview</div>
<div class="nav-item" data-page="servers" onclick="switchPage('servers')">Servers</div>
<div class="nav-item" data-page="proxies" onclick="switchPage('proxies')">Proxies</div>
<div class="nav-item" data-page="contracts" onclick="switchPage('contracts')">Contracts</div>
	<div class="nav-item" data-page="best" onclick="switchPage('best')">Best Proxies</div>
</nav>
<main class="content">

<!-- ===== OVERVIEW ===== -->
<div id="page-overview" class="page active">
<div class="header">
<h1>URnetwork Hub <small>fleet bandwidth dashboard</small></h1>
<div class="cards">
<div class="card card-proxies"><div class="label">Total Proxies</div><div class="value">{{.Sum.TotalProxies}}</div><div class="sub">across {{.Sum.Nodes}} nodes</div></div>
<div class="card card-up"><div class="label">Healthy</div><div class="value">{{.Sum.Up}}</div><div class="sub">{{printf "%.1f" (pct .Sum.Up .Sum.TotalProxies)}}% of fleet</div></div>
<div class="card card-degraded"><div class="label">Degraded</div><div class="value">{{.Sum.Degraded}}</div><div class="sub">{{.Sum.Dead}} dead</div></div>
<div class="card card-earn"><div class="label">Earning</div><div class="value">{{printf "%.1f" (pct .Sum.Earning .Sum.Up)}}%</div><div class="sub">{{.Sum.Earning}} / {{.Sum.Up}} up proxies</div></div>
<div class="card card-clients"><div class="label">Active Clients</div><div class="value">{{.Sum.TotalClients}}</div><div class="sub">{{printf "%s" (fmtBytes .Sum.TotalRX)}} RX / {{printf "%s" (fmtBytes .Sum.TotalTX)}} TX</div></div>
<div class="card card-up"><div class="label">Throughput</div><div class="value">{{printf "%.0f" (add .Sum.MbpsRX .Sum.MbpsTX)}} Mbps</div><div class="sub">{{printf "%.1f" .Sum.MbpsRX}} in / {{printf "%.1f" .Sum.MbpsTX}} out</div></div>
<div class="card card-earn"><div class="label">Contract Win Rate</div><div class="value">{{printf "%.1f" (pct64 .Sum.ContractsAcquired (add64 .Sum.ContractsAcquired .Sum.ContractsDenied))}}%</div><div class="sub">{{.Sum.ContractsAcquired}} acquired / {{.Sum.ContractsDenied}} denied</div></div>
</div>
</div>
<div class="charts-row">
<div class="chart-box compact">
<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:4px"><span style="font-size:11px;color:#64748b">Total Traffic</span><button onclick="resetFleetChart('fleet-traffic')" style="background:none;border:none;color:#64748b;cursor:pointer;font-size:11px">Reset zoom</button></div>
<div id="fleet-traffic" style="height:160px"></div>
</div>
<div class="chart-box compact">
<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:4px"><span style="font-size:11px;color:#64748b">Billable Traffic</span><button onclick="resetFleetChart('fleet-billable')" style="background:none;border:none;color:#64748b;cursor:pointer;font-size:11px">Reset zoom</button></div>
<div id="fleet-billable" style="height:160px"></div>
</div>
<div class="chart-box compact">
<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:4px"><span style="font-size:11px;color:#64748b">Peak Clients</span><button onclick="resetFleetChart('fleet-clients')" style="background:none;border:none;color:#64748b;cursor:pointer;font-size:11px">Reset zoom</button></div>
<div id="fleet-clients" style="height:160px"></div>
</div>
<div class="chart-box compact">
<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:4px"><span style="font-size:11px;color:#64748b">Fleet Nodes</span><button onclick="resetFleetChart('fleet-nodes')" style="background:none;border:none;color:#64748b;cursor:pointer;font-size:11px">Reset zoom</button></div>
<div id="fleet-nodes" style="height:160px"></div>
</div>
<div class="chart-box compact">
<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:4px"><span style="font-size:11px;color:#64748b">Contracts/hr</span><button onclick="resetFleetChart('fleet-contracts')" style="background:none;border:none;color:#64748b;cursor:pointer;font-size:11px">Reset zoom</button></div>
<div id="fleet-contracts" style="height:160px"></div>
</div>
<div class="chart-box compact">
<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:4px"><span style="font-size:11px;color:#64748b">Throughput Mbps</span><button onclick="resetFleetChart('fleet-mbps')" style="background:none;border:none;color:#64748b;cursor:pointer;font-size:11px">Reset zoom</button></div>
<div id="fleet-mbps" style="height:160px"></div>
</div>
<div class="chart-box compact">
<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:4px"><span style="font-size:11px;color:#64748b">Bill Ratio %</span><button onclick="resetFleetChart('fleet-billratio')" style="background:none;border:none;color:#64748b;cursor:pointer;font-size:11px">Reset zoom</button></div>
<div id="fleet-billratio" style="height:160px"></div>
</div>
<div class="chart-box compact">
<div style="display:flex;justify-content:space-between;align-items:center;margin-bottom:4px"><span style="font-size:11px;color:#64748b">Contract Win Rate %</span><button onclick="resetFleetChart('fleet-winrate')" style="background:none;border:none;color:#64748b;cursor:pointer;font-size:11px">Reset zoom</button></div>
<div id="fleet-winrate" style="height:160px"></div>
</div>
</div>
</div>

<!-- ===== SERVERS ===== -->
<div id="page-servers" class="page">
<div class="page-header">Servers</div>
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
<th class="num" data-col="proxies" onclick="sortBy('proxies')">Proxies <span class="sort-arrow"></span></th>
<th class="num" data-col="clients" onclick="sortBy('clients')">Clients <span class="sort-arrow"></span></th>
<th class="num" data-col="rx" onclick="sortBy('rx')">RX <span class="sort-arrow"></span></th>
<th class="num" data-col="tx" onclick="sortBy('tx')">TX <span class="sort-arrow"></span></th>
<th class="num" data-col="billrx" onclick="sortBy('billrx')">Bill RX <span class="sort-arrow"></span></th>
<th class="num" data-col="billtx" onclick="sortBy('billtx')">Bill TX <span class="sort-arrow"></span></th>
<th class="num" data-col="rate-rx" onclick="sortBy('rate-rx')">In Mbps <span class="sort-arrow"></span></th>
<th class="num" data-col="rate-tx" onclick="sortBy('rate-tx')">Out Mbps <span class="sort-arrow"></span></th>
<th class="num" data-col="earning" onclick="sortBy('earning')">Earning <span class="sort-arrow"></span></th>
<th class="num" data-col="contracts" onclick="sortBy('contracts')">Contracts <span class="sort-arrow"></span></th>
<th></th>
</tr>
</thead>
<tbody id="node-body">
<tr><td colspan="14" style="text-align:center;color:#64748b;padding:20px">Loading servers...</td></tr>
</tbody>
</table>
</div>
</div>

<!-- ===== PROXIES ===== -->
<div id="page-proxies" class="page">
<div class="page-header">Proxies</div>
<div class="window-pills">
<span class="pill" onclick="setProxiesWindow('1h',this)">1h</span>
<span class="pill" onclick="setProxiesWindow('24h',this)">24h</span>
<span class="pill on" onclick="setProxiesWindow('7d',this)">7d</span>
<span class="pill" onclick="setProxiesWindow('30d',this)">30d</span>
<span class="pill" onclick="setProxiesWindow('1y',this)">1y</span>
<select id="proxies-sort" onchange="loadProxies()" style="background:#1a2332;border:1px solid #1e293b;color:#e2e8f0;padding:3px 8px;border-radius:5px;font-size:11px">
<option value="traffic">Traffic</option>
<option value="contracts">Contracts</option>
<option value="denied">Denied</option>
</select>
<select id="proxies-node" onchange="loadProxies()" style="background:#1a2332;border:1px solid #1e293b;color:#e2e8f0;padding:3px 8px;border-radius:5px;font-size:11px">
<option value="">All nodes</option>
</select>
<span class="info" id="proxies-info">Loading...</span>
</div>
<div id="proxies-charts" style="display:flex;gap:8px;padding:10px 20px;"></div>
<div class="table-section-header" style="font-weight:600">ACTIVE PROXIES</div>
<div class="table-wrap"><table id="proxies-active-table"><thead><tr><th class="num">#</th><th onclick="sortProxies('addr')" style="cursor:pointer">Proxy</th><th class="num" onclick="sortProxies('traffic')" style="cursor:pointer">Traffic</th><th class="num" onclick="sortProxies('acq')" style="cursor:pointer">Won</th><th class="num" onclick="sortProxies('denied')" style="cursor:pointer">Denied</th><th class="num" onclick="sortProxies('winPct')" style="cursor:pointer">Win%</th><th class="num" onclick="sortProxies('nodes')" style="cursor:pointer">Nodes</th></tr></thead><tbody id="proxies-active-body"><tr><td colspan="7" style="text-align:center;color:#64748b;padding:20px">Loading...</td></tr></tbody></table></div>
<div class="table-section-header" onclick="toggleIdleProxies()" style="font-weight:600">IDLE PROXIES <span id="idle-count"></span><span style="color:#475569;margin-left:8px;font-weight:400">click to expand</span></div>
<div id="proxies-idle-wrap" class="hidden"><div class="table-wrap"><table id="proxies-idle-table"><thead><tr><th>Proxy</th><th>Nodes</th><th>Last activity</th></tr></thead><tbody id="proxies-idle-body"></tbody></table></div></div>
</div>

<!-- ===== CONTRACTS ===== -->
<div id="page-contracts" class="page">
<div class="page-header">Contracts</div>
<div class="window-pills">
<span class="pill" onclick="setContractsWindow('1h',this)">1h</span>
<span class="pill" onclick="setContractsWindow('24h',this)">24h</span>
<span class="pill on" onclick="setContractsWindow('7d',this)">7d</span>
<span class="pill" onclick="setContractsWindow('30d',this)">30d</span>
<span class="info" id="contracts-fleet-info">Loading...</span>
</div>
<div class="chart-wrap"><div class="chart-box"><div id="contracts-fleet-chart"></div></div></div>
<div class="table-section-header" style="font-weight:600">PER SERVER &middot; sorted by win rate &middot; click for detail</div>
<div class="table-wrap"><table id="contracts-table"><thead><tr><th>Server</th><th class="num">Won</th><th class="num">Denied</th><th class="num">Win%</th><th style="width:24%">Split</th></tr></thead><tbody id="contracts-body"><tr><td colspan="5" style="text-align:center;color:#64748b;padding:20px">Loading...</td></tr></tbody></table></div>
</div>

<!-- ===== BEST PROXIES ===== -->
<div id="page-best" class="page">
<div class="page-header">Best Proxies <span style="font-size:12px;color:#475569;font-weight:400">composite score: win% &times; ln(1 + traffic)</span></div>
<div class="window-pills">
<label style="font-size:11px;color:#94a3b8;cursor:pointer"><input type="checkbox" id="hide-dead" onchange="loadBestProxies()"> Hide dead</label>
<span class="info" id="best-info">Loading...</span>
</div>
<div class="table-wrap"><table><thead><tr><th class="num">#</th><th onclick="sortBest('addr')" style="cursor:pointer">Address</th><th class="num" onclick="sortBest('tier')" style="cursor:pointer">Grade</th><th class="num" onclick="sortBest('score')" style="cursor:pointer">Score</th><th class="num" onclick="sortBest('win_pct')" style="cursor:pointer">Win%</th><th class="num" onclick="sortBest('traffic')" style="cursor:pointer">Traffic</th><th class="num" onclick="sortBest('contracts')" style="cursor:pointer">Contracts</th><th onclick="sortBest('last_day')" style="cursor:pointer">Last seen</th><th onclick="sortBest('status')" style="cursor:pointer">Status</th></tr></thead><tbody id="best-body"><tr><td colspan="9" style="text-align:center;color:#64748b;padding:20px">Loading...</td></tr></tbody></table></div>
</div>

</main>
</div>

<div class="drawer-overlay" id="drawer-overlay" onclick="closeDrawer()"></div>
<div class="drawer" id="drawer">
<div class="drawer-header"><h2 id="drawer-title">Node</h2><span class="drawer-close" onclick="closeDrawer()">&#10005;</span></div>
<div class="drawer-body" id="drawer-body"><div class="loading">Select a node to view proxy details</div></div>
</div>
<div class="footer">
<div class="auto-refresh">
<input type="checkbox" id="auto-refresh" checked onchange="toggleRefresh()">
<label for="auto-refresh">auto-refresh</label>
<span class="countdown" id="countdown">30s</span>
</div>
<div><a href="/api/nodes">/api/nodes (JSON)</a></div>
</div>

<script>
var fleetCharts = [], fleetChartData = null;
var historyChart = null, historyHours = 24;
var contractsChart = null;

// === Page switching ===
function switchPage(name) {
  document.querySelectorAll('.nav-item').forEach(function(n){n.classList.remove('active');});
  document.querySelectorAll('.page').forEach(function(p){p.classList.remove('active');});
  document.querySelector('.nav-item[data-page="'+name+'"]').classList.add('active');
  document.getElementById('page-'+name).classList.add('active');
  if (location.hash !== '#'+name) history.replaceState(null, '', '#'+name);
  if (name === 'overview') loadFleetChart();
  if (name === 'proxies') loadProxies();
  if (name === 'contracts') loadContracts();
  if (name === 'best') loadBestProxies();
  if (name === 'proxies' || name === 'contracts') loadNodeOptions();
}
window.addEventListener('hashchange', function() {
  var page = (location.hash || '#overview').replace('#', '');
  switchPage(page);
});
(function() {
  var page = (location.hash || '#overview').replace('#', '');
  switchPage(page);
})();

// === Best Proxies ===
var bestRequestSeq = 0, bestRows = [], bestSortCol = 'score', bestSortDir = -1;
function sortBest(col) {
  if (bestSortCol === col) bestSortDir *= -1; else { bestSortCol = col; bestSortDir = -1; }
  renderBest();
}
function renderBest() {
  var rows = bestRows.slice();
  rows.sort(function(a,b){
    var va = a[bestSortCol], vb = b[bestSortCol];
    if (typeof va === 'number') return bestSortDir * (va - vb);
    return bestSortDir * String(va).localeCompare(String(vb));
  });
  var tbody = document.getElementById('best-body');
  var html = '';
  for (var i = 0; i < rows.length; i++) {
    var r = rows[i];
    var cl = r.win_pct >= 80 ? 'g' : r.win_pct >= 60 ? 'y' : 'r';
    var traffic = r.traffic > 1099511627776 ? (r.traffic/1099511627776).toFixed(1)+'T' : r.traffic > 1073741824 ? (r.traffic/1073741824).toFixed(1)+'G' : r.traffic > 1048576 ? (r.traffic/1048576).toFixed(1)+'M' : r.traffic > 1024 ? (r.traffic/1024).toFixed(1)+'K' : r.traffic;
    var lastSeen = r.last_day === (Math.floor(Date.now()/86400000)) ? 'today' : ((Math.floor(Date.now()/86400000) - r.last_day) + 'd ago');
    var statusDot = r.status === 'active' ? '<span class="dot alive" style="background:#22c55e"></span>' : '<span class="dot" style="background:#64748b"></span>';
    var gradeCell = r.graded ? '<span class="grade-badge grade-' + r.tier + '">' + r.tier + '</span>' : '<span class="grade-none">—</span>';
    html += '<tr><td>' + (i+1) + '</td><td>' + r.addr + '</td><td class="num">' + gradeCell + '</td><td class="num">' + r.score.toFixed(2) + '</td><td class="num ' + cl + '">' + r.win_pct.toFixed(1) + '%</td><td class="num">' + traffic + '</td><td class="num">' + r.acq + '/' + r.denied + '</td><td>' + lastSeen + '</td><td>' + statusDot + r.status + '</td></tr>';
  }
  tbody.innerHTML = html || '<tr><td colspan="9" style="text-align:center;color:#64748b;padding:20px">No proxy data yet</td></tr>';
}
function loadBestProxies() {
  if (!document.getElementById('page-best').classList.contains('active')) return;
  var hideDead = document.getElementById('hide-dead').checked;
  var url = '/api/proxies/best?limit=200&hide_dead=' + hideDead;
  document.getElementById('best-info').textContent = 'Loading...';
  var thisSeq = ++bestRequestSeq;
  fetch(url).then(function(r){return r.json();}).then(function(rows){
    if (thisSeq !== bestRequestSeq) return;
    if (!rows || rows.length === 0) { bestRows = []; renderBest(); document.getElementById('best-info').textContent = '0 proxies'; return; }
    // Store with sortable keys including derived fields
    bestRows = rows.map(function(r){ return { addr: r.addr, tier: r.graded ? r.tier : '', graded: !!r.graded, score: r.score, win_pct: r.win_pct, traffic: r.traffic, acq: r.acq, denied: r.denied, contracts: (r.acq||0)+(r.denied||0), last_day: r.last_day, status: r.status }; });
    renderBest();
    document.getElementById('best-info').textContent = rows.length + ' proxies';
  }).catch(function(){
    if (thisSeq !== bestRequestSeq) return;
    document.getElementById('best-info').textContent = 'Error loading';
  });
}

// === Fleet charts (Overview) ===
function makeChart(el, opts, data) {
  if (!el) return;
  el.innerHTML = '';
  var w = el.clientWidth || 400;
  if (w < 50) w = 400;
  opts.width = w; opts.height = 150; opts.cursor = { show: true }; opts.legend = { show: true };
  if (!opts.axes) opts.axes = [{ show: false }, { stroke: '#64748b', grid: { stroke: '#1e293b', width: 1 }, size: 55 }];
  var chart = new uPlot(opts, data, el);
  el._uplot = chart;
  fleetCharts.push(chart);
}
function resetFleetChart(id) {
  var el = document.getElementById(id);
  if (!el) return;
  if (el._uplot) { el._uplot.destroy(); }
  fleetCharts = fleetCharts.filter(function(c) { return c !== el._uplot; });
  loadFleetChart();
}
function loadFleetChart() {
  if (fleetCharts.length > 0) return;
  fetch('/api/history?hours=24').then(function(r){return r.json();}).then(function(data){
    if (!data || data.length === 0) return;
    var byHour = {};
    for (var i = 0; i < data.length; i++) {
      var h = data[i];
      if (!byHour[h.hour]) byHour[h.hour] = { rx: 0, tx: 0, bill_rx: 0, bill_tx: 0, clients: 0, count: 0, acquired: 0, denied: 0 };
      byHour[h.hour].rx += h.total_rx; byHour[h.hour].tx += h.total_tx;
      byHour[h.hour].bill_rx += h.bill_rx; byHour[h.hour].bill_tx += h.bill_tx;
      byHour[h.hour].clients += h.peak_clients; byHour[h.hour].count++;
      byHour[h.hour].acquired += (h.contracts_acquired || 0); byHour[h.hour].denied += (h.contracts_denied || 0);
    }
    var hours = Object.keys(byHour).sort();
    var labels = [], rx = [], tx = [], brx = [], btx = [], clients = [], nodes = [], acquired = [], denied = [];
    // Seed cumulative deltas from the first hour so the initial spike doesn't
    // dominate the y-axis and flatten subsequent values.
    var first = hours.length > 0 ? byHour[hours[0]] : null;
    var prevRx = first ? first.rx : 0, prevTx = first ? first.tx : 0;
    var prevBRx = first ? first.bill_rx : 0, prevBTx = first ? first.bill_tx : 0;
    var prevAcq = first ? first.acquired : 0, prevDen = first ? first.denied : 0;
    hours.forEach(function(h) {
      labels.push(parseInt(h));
      var drx = byHour[h].rx - prevRx; rx.push(drx >= 0 ? drx : 0); prevRx = byHour[h].rx;
      var dtx = byHour[h].tx - prevTx; tx.push(dtx >= 0 ? dtx : 0); prevTx = byHour[h].tx;
      var dbrx = byHour[h].bill_rx - prevBRx; brx.push(dbrx >= 0 ? dbrx : 0); prevBRx = byHour[h].bill_rx;
      var dbtx = byHour[h].bill_tx - prevBTx; btx.push(dbtx >= 0 ? dbtx : 0); prevBTx = byHour[h].bill_tx;
      clients.push(byHour[h].clients); nodes.push(byHour[h].count);
      var dacq = byHour[h].acquired - prevAcq; acquired.push(dacq >= 0 ? dacq : 0); prevAcq = byHour[h].acquired;
      var dden = byHour[h].denied - prevDen; denied.push(dden >= 0 ? dden : 0); prevDen = byHour[h].denied;
    });
    makeChart(document.getElementById('fleet-traffic'), { series: [{}, { label: 'RX/hr', stroke: '#60a5fa', fill: 'rgba(96,165,250,0.1)', width: 1.5, value: function(u,v){return fmtBytes(v)+'/h';} }, { label: 'TX/hr', stroke: '#4ade80', fill: 'rgba(74,222,128,0.1)', width: 1.5, value: function(u,v){return fmtBytes(v)+'/h';} }] }, [labels, rx, tx]);
    makeChart(document.getElementById('fleet-billable'), { series: [{}, { label: 'Bill RX/hr', stroke: '#f59e0b', fill: 'rgba(245,158,11,0.1)', width: 1.5, value: function(u,v){return fmtBytes(v)+'/h';} }, { label: 'Bill TX/hr', stroke: '#a78bfa', fill: 'rgba(167,139,250,0.1)', width: 1.5, value: function(u,v){return fmtBytes(v)+'/h';} }] }, [labels, brx, btx]);
    makeChart(document.getElementById('fleet-clients'), { series: [{}, { label: 'Peak clients', stroke: '#f472b6', fill: 'rgba(244,114,182,0.1)', width: 1.5 }] }, [labels, clients]);
    makeChart(document.getElementById('fleet-nodes'), { series: [{}, { label: 'Reporting nodes', stroke: '#22d3ee', fill: 'rgba(34,211,238,0.1)', width: 1.5 }] }, [labels, nodes]);
    makeChart(document.getElementById('fleet-contracts'), { series: [{}, { label: 'Acquired/hr', stroke: '#4ade80', fill: 'rgba(74,222,128,0.1)', width: 1.5 }, { label: 'Denied/hr', stroke: '#f87171', fill: 'rgba(248,113,113,0.1)', width: 1.5 }] }, [labels, acquired, denied]);
    var mbps = [], billRatio = [], winRate = [];
    for (var i = 0; i < rx.length; i++) {
      var totBytes = (rx[i]||0)+(tx[i]||0); var billBytes = (brx[i]||0)+(btx[i]||0);
      mbps.push(totBytes > 0 ? (totBytes / 3600 * 8 / 1048576) : 0);
      billRatio.push(totBytes > 0 ? (billBytes / totBytes * 100) : 0);
      var a = acquired[i]||0, d = denied[i]||0;
      winRate.push((a+d) > 0 ? (a/(a+d)*100) : 0);
    }
    makeChart(document.getElementById('fleet-mbps'), { series: [{}, { label: 'Mbps', stroke: '#34d399', fill: 'rgba(52,211,153,0.1)', width: 1.5, value: function(u,v){return v.toFixed(1)+' Mbps';} }] }, [labels, mbps]);
    makeChart(document.getElementById('fleet-billratio'), { series: [{}, { label: 'Bill %', stroke: '#fbbf24', fill: 'rgba(251,191,36,0.1)', width: 1.5, value: function(u,v){return v.toFixed(1)+'%';} }] }, [labels, billRatio]);
    makeChart(document.getElementById('fleet-winrate'), { series: [{}, { label: 'Win %', stroke: '#818cf8', fill: 'rgba(129,140,248,0.1)', width: 1.5, value: function(u,v){return v.toFixed(1)+'%';} }] }, [labels, winRate]);
  }).catch(function(){});
}

// === Servers table ===
function applyFilter() {
  var q = document.getElementById('filter-input').value.toLowerCase();
  var status = document.getElementById('filter-status').value;
  var tbody = document.querySelector('#node-table tbody');
  var visible = 0;
  var rows = tbody.querySelectorAll('tr');
  rows.forEach(function(r){
    if (!r.classList.contains('expandable')) return;
    var match = r.getAttribute('data-id').toLowerCase().indexOf(q) >= 0;
    if (status !== 'all' && r.getAttribute('data-status') !== status) match = false;
    r.classList.toggle('hidden', !match);
    if (match) visible++;
  });
  var showGroup = false;
  for (var i = rows.length - 1; i >= 0; i--) {
    var r = rows[i];
    if (r.classList.contains('group-header')) {
      r.classList.toggle('hidden', !showGroup);
      showGroup = false;
    } else if (!r.classList.contains('hidden')) {
      showGroup = true;
    }
  }
  document.getElementById('filter-count').textContent = visible + ' / ' + document.querySelectorAll('#node-table tbody tr.expandable').length + ' nodes';
}
var sortState = {};
function sortBy(col) {
  var dir = sortState[col] === -1 ? 1 : -1; sortState[col] = dir;
  document.querySelectorAll('th[data-col]').forEach(function(th){th.classList.remove('sorted');th.querySelector('.sort-arrow').textContent='';});
  var th = document.querySelector('th[data-col="'+col+'"]');
  th.classList.add('sorted'); th.querySelector('.sort-arrow').textContent = dir === -1 ? '\u25BC' : '\u25B2';
  var tbody = document.querySelector('#node-table tbody');
  var rows = Array.from(tbody.querySelectorAll('tr.expandable'));
  rows.sort(function(a,b){return cmpNode(a.cells[getColIndex(col)].textContent.trim(), b.cells[getColIndex(col)].textContent.trim(), dir);});
  rows.forEach(function(r){tbody.appendChild(r);});
}
function getColIndex(col) {
  return {node:0,heartbeat:1,uptime:2,proxies:3,clients:4,rx:5,tx:6,billrx:7,billtx:8,'rate-rx':9,'rate-tx':10,earning:11,contracts:12}[col]||0;
}
function parseSortValue(s) {
  s = s.trim(); if (s === '') return -1;
  var m = s.match(/^([\d.]+)\s*([KMGTPE]?B)$/i);
  if (m) return parseFloat(m[1]) * ({'B':1,'KB':1024,'MB':1048576,'GB':1073741824,'TB':1099511627776}[m[2].toUpperCase()]||1);
  if (/^-?[\d.]+$/.test(s)) return parseFloat(s);
  return s;
}
function cmpNode(a,b,dir){var pa=parseSortValue(a),pb=parseSortValue(b);if(typeof pa==='number'&&typeof pb==='number')return dir*(pa-pb);return dir*String(a).localeCompare(String(b),undefined,{numeric:true});}
function reapplySort() {
  var cols = Object.keys(sortState);
  if (cols.length === 0) return;
  var col = cols[0], dir = sortState[col];
  var tbody = document.querySelector('#node-table tbody');
  if (!tbody) return;
  var rows = Array.from(tbody.querySelectorAll('tr.expandable'));
  rows.sort(function(a,b){return cmpNode(a.cells[getColIndex(col)].textContent.trim(), b.cells[getColIndex(col)].textContent.trim(), dir);});
  rows.forEach(function(r){tbody.appendChild(r);});
}

// === Drawer ===
var drawerNodeId = null, proxyDrawer = {};
function openDrawer(id) {
  drawerNodeId = id;
  document.getElementById('drawer-overlay').classList.add('open');
  document.getElementById('drawer').classList.add('open');
  document.getElementById('drawer-title').textContent = 'Node: ' + id;
  document.getElementById('drawer-body').innerHTML = '<div class="loading">Loading proxies...</div>';
  fetch('/api/nodes/' + id + '/proxies').then(function(r){return r.json();}).then(function(proxies){
    if (!proxies || proxies.length === 0) { document.getElementById('drawer-body').innerHTML = '<div class="loading">No proxy data</div>'; return; }
    proxyDrawer = { data: proxies, col: 'clients', dir: -1 };
    proxies.sort(function(a,b){if(b.clients!==a.clients)return b.clients-a.clients;return(b.bill_rx||0)-(a.bill_rx||0);});
    renderProxyDrawer();
  }).catch(function(){document.getElementById('drawer-body').innerHTML='<div class="loading">Failed to load proxies</div>';});
}
function renderProxyDrawer() {
  var d = proxyDrawer;
  var sortOrder = {up:0,connecting:1,degraded:2,dead:3};
  var cols = [{key:'id',label:'ID',num:false},{key:'addr',label:'Address',num:false},{key:'grade',label:'Grade',num:false,fn:function(p){if(!p.graded)return 0;return {A:6,B:5,C:4,D:3,F:2}[p.tier]||1;}},{key:'status',label:'Status',num:false,fn:function(p){return sortOrder[p.status]||9;}},{key:'clients',label:'Clients',num:true},{key:'max_age_s',label:'Age',num:true},{key:'rx',label:'RX',num:true},{key:'tx',label:'TX',num:true},{key:'bill_rx',label:'Bill RX',num:true},{key:'bill_tx',label:'Bill TX',num:true},{key:'contracts_acquired',label:'Won',num:true},{key:'contracts_denied',label:'Lost',num:true}];
  d.data.sort(function(a,b){var col=cols.find(function(c){return c.key===d.col;});var va=col&&col.fn?col.fn(a):a[d.col];var vb=col&&col.fn?col.fn(b):b[d.col];if(typeof va==='number')return d.dir*(va-vb);return d.dir*String(va).localeCompare(String(vb));});
  var html = '<table id="drawer-table"><thead><tr>';
  cols.forEach(function(col){var arrow=col.key===d.col?(d.dir===-1?' &#9660;':' &#9650;'):'';html+='<th'+(col.num?' class="num"':'')+' onclick="sortDrawer(\''+col.key+'\')">'+col.label+arrow+'</th>';});
  html+='</tr></thead><tbody>';
  d.data.forEach(function(p){var gradeCell=p.graded?'<span class="grade-badge grade-'+p.tier+'">'+p.tier+'</span>':'<span class="grade-none">—</span>';html+='<tr><td class="num-mono">'+p.id+'</td><td class="truncate">'+p.addr+'</td><td>'+gradeCell+'</td><td><span class="proxy-status '+p.status+'"></span>'+p.status+'</td><td class="num">'+p.clients+'</td><td class="num">'+fmtAge(p.max_age_s)+'</td><td class="num">'+fmtBytes(p.rx)+'</td><td class="num">'+fmtBytes(p.tx)+'</td><td class="num">'+fmtBytes(p.bill_rx)+'</td><td class="num">'+fmtBytes(p.bill_tx)+'</td><td class="num">'+(p.contracts_acquired||0)+'</td><td class="num">'+(p.contracts_denied||0)+'</td></tr>';});
  html+='</tbody></table>';
  document.getElementById('drawer-body').innerHTML = html;
}
function sortDrawer(col){if(proxyDrawer.col===col)proxyDrawer.dir*=-1;else{proxyDrawer.col=col;proxyDrawer.dir=-1;}renderProxyDrawer();}
function closeDrawer(){document.getElementById('drawer-overlay').classList.remove('open');document.getElementById('drawer').classList.remove('open');}

// === Auto-refresh ===
var secondsLeft = 30, refreshing = false;
setInterval(function tick(){
  if (!document.getElementById('auto-refresh').checked) { document.getElementById('countdown').textContent = 'paused'; return; }
  secondsLeft--;
  if (secondsLeft <= 0) { secondsLeft = 30; refreshDashboard(); return; }
  document.getElementById('countdown').textContent = secondsLeft + 's';
}, 1000);
function toggleRefresh() { if (document.getElementById('auto-refresh').checked) secondsLeft = 30; }

// === Live updates (SSE) ===
// Pushes a bare "something changed" signal from the hub the moment a
// heartbeat or report lands, so the dashboard doesn't wait out the full
// 30s poll above. The poll stays as a backstop for links where SSE gets
// buffered/stripped (e.g. some reverse proxies) — EventSource retries on
// its own with native backoff, no custom reconnect logic needed here.
if (window.EventSource) {
  var liveEvents = new EventSource('/api/events');
  var sseTimer = null;
  liveEvents.onmessage = function() {
    if (sseTimer !== null) return;
    sseTimer = setTimeout(function() { sseTimer = null; refreshDashboard(); }, 5000);
  };
}

function refreshDashboard() {
  if (refreshing) return; refreshing = true;
  var fc = document.getElementById('filter-count'); if (fc) fc.textContent = 'Refreshing\u2026';
  fetch('/api/nodes').then(function(r){if(!r.ok)throw new Error(r.status===429?'rate limited':'error '+r.status);return r.json();}).then(function(nodes){
    var tbody = document.querySelector('#node-table tbody');
    var totalProxies = 0, totalUp = 0, totalDeg = 0, totalDead = 0, totalClients = 0, totalEarning = 0, nodeCount = 0, totalRX = 0, totalTX = 0;

    // Assign a color to each unique source IP from a fixed palette
    var ipColors = {}, palette = ['#6366f1','#8b5cf6','#ec4899','#f43f5e','#f97316','#eab308','#22c55e','#14b8a6','#06b6d4','#3b82f6'];
    var ci = 0;
    nodes.sort(function(a,b){return (a.source_ip||'unknown').localeCompare(b.source_ip||'unknown');});
    nodes.forEach(function(n){
      nodeCount++; totalProxies += n.proxies; totalUp += n.up; totalDeg += n.degraded; totalDead += n.dead;
      totalClients += n.clients; totalEarning += n.earning; totalRX += n.rx; totalTX += n.tx;
      var ip = n.source_ip || 'unknown';
      if (!ipColors[ip]) { ipColors[ip] = palette[ci % palette.length]; ci++; }
    });

    var frag = document.createDocumentFragment();
    nodes.forEach(function(n){
      var ip = n.source_ip || 'unknown';
      var ipColor = ipColors[ip];
      var ago = fmtAgo(n.ts), uptime = fmtUptime(n.uptime), color = n.ts ? nodeColor(n.ts) : '#ef4444';
      var sc = n.dead > 0 ? 'dead' : (n.degraded > 0 ? 'degraded' : 'up');
      var tlsIcon = n.tls ? '<span style="color:#4ade80;font-size:11px" title="Encrypted (TLS)">&#128274;</span> ' : '';
      var tr = document.createElement('tr'); tr.className = 'expandable'; tr.setAttribute('data-id', n.node_id); tr.setAttribute('data-status', sc);
      tr.setAttribute('data-ip', ip);
      tr.onclick = function(){openDrawer(n.node_id);};
      tr.innerHTML = '<td class="node-id"><span class="dot'+(n.up>0?' alive':'')+'" style="background:'+color+'"></span>'+tlsIcon+n.node_id+' <span class="version">'+(n.sys.host||'')+'</span><span class="ip-tag" style="border-color:'+ipColor+';color:'+ipColor+'">'+ip+'</span></td><td>'+ago+'</td><td>'+uptime+'</td><td class="num">'+n.up+(n.degraded>0?' <span class="status-badge degraded">'+n.degraded+'</span>':'')+(n.dead>0?' <span class="status-badge dead">'+n.dead+'</span>':'')+'</td><td class="num">'+n.clients+'</td><td class="num">'+fmtBytes(n.rx)+'</td><td class="num">'+fmtBytes(n.tx)+'</td><td class="num">'+fmtBytes(n.bill_rx)+'</td><td class="num">'+fmtBytes(n.bill_tx)+'</td><td class="num">'+(n.mbps_rx?n.mbps_rx.toFixed(1):'')+'</td><td class="num">'+(n.mbps_tx?n.mbps_tx.toFixed(1):'')+'</td><td class="num">'+n.earning+'/'+n.up+'</td><td><span class="remove-btn" onclick="event.stopPropagation();removeNode(\''+n.node_id+'\')">&#10005;</span></td>';
      frag.appendChild(tr);
    });
    tbody.innerHTML = '';
    tbody.appendChild(frag);
    document.querySelectorAll('.card')[0].innerHTML = '<div class="label">Total Proxies</div><div class="value">'+totalProxies+'</div><div class="sub">across '+nodeCount+' nodes</div>';
    document.querySelectorAll('.card')[1].innerHTML = '<div class="label">Healthy</div><div class="value">'+totalUp+'</div><div class="sub">'+(totalProxies>0?(totalUp/totalProxies*100).toFixed(1):'0')+'% of fleet</div>';
    document.querySelectorAll('.card')[2].innerHTML = '<div class="label">Degraded</div><div class="value">'+totalDeg+'</div><div class="sub">'+totalDead+' dead</div>';
    document.querySelectorAll('.card')[3].innerHTML = '<div class="label">Earning</div><div class="value">'+(totalUp>0?(totalEarning/totalUp*100).toFixed(1):'0')+'%</div><div class="sub">'+totalEarning+' / '+totalUp+' up</div>';
    document.querySelectorAll('.card')[4].innerHTML = '<div class="label">Active Clients</div><div class="value">'+totalClients+'</div><div class="sub">'+fmtBytes(totalRX)+' RX / '+fmtBytes(totalTX)+' TX</div>';
    applyFilter();
    reapplySort();
  }).catch(function(e){var fc=document.getElementById('filter-count');if(fc)fc.textContent='Error: '+(e&&e.message||e||'unknown');}).then(function(){refreshing=false;});
}
function removeNode(nodeId) {
  if (!confirm('Remove ' + nodeId + ' from dashboard?')) return;
  fetch('/api/nodes/remove', {method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({node_id:nodeId})})
    .then(function(r){if(r.ok)refreshDashboard();});
}

// === Utility ===
function fmtBytes(b){if(!b&&b!==0)return '0 B';if(b<1024)return b+' B';var u='KMGTPE',i=-1,n=b;while(n>=1024&&i<u.length-1){n/=1024;i++;}return n.toFixed(1)+' '+u[i]+'B';}
function fmtAge(s){if(!s||s===0)return'&mdash;';if(s<60)return s+'s';if(s<3600)return Math.round(s/60)+'m';return Math.round(s/3600)+'h';}
function fmtAgo(ts){if(!ts)return'never';var d=(Date.now()-new Date(ts).getTime())/1000;if(d<10)return'now';if(d<60)return Math.round(d)+'s';if(d<3600)return Math.round(d/60)+'m';return Math.round(d/3600)+'h';}
function fmtUptime(s){if(!s)return'0s';var h=Math.floor(s/3600),d=Math.floor(h/24);if(d>0)return d+'d '+(h%24)+'h';if(h>0)return h+'h';return Math.floor(s/60)+'m';}
function nodeColor(ts){var age=(Date.now()-new Date(ts).getTime())/1000;if(age<420)return'#22c55e';if(age<900)return'#eab308';return'#ef4444';}

// === Load node options for proxy/contract filters ===
var nodeOptionsLoaded = false;
function loadNodeOptions() {
  if (nodeOptionsLoaded) return;
  fetch('/api/nodes').then(function(r){return r.json();}).then(function(nodes){
    var sel = document.getElementById('proxies-node');
    nodes.forEach(function(n){var o=document.createElement('option');o.value=n.node_id;o.textContent=n.node_id;sel.appendChild(o);});
    nodeOptionsLoaded = true;
  }).catch(function(){});
}

// === Proxies page ===
var proxiesWindow = '7d';
function setProxiesWindow(w, btn) {
  proxiesWindow = w;
  document.querySelectorAll('#page-proxies .pill').forEach(function(p){p.classList.remove('on');});
  if (btn) btn.classList.add('on');
  loadProxies();
}
var proxiesRequestSeq = 0, proxiesRows = [], proxiesSortCol = null, proxiesSortDir = -1;
function sortProxies(col) {
  if (proxiesSortCol === col) proxiesSortDir *= -1; else { proxiesSortCol = col; proxiesSortDir = -1; }
  if (!proxiesRows.length) return;
  var rows = proxiesRows.slice();
  rows.sort(function(a,b){
    var va = a[col], vb = b[col];
    if (col === 'addr') return proxiesSortDir * String(va).localeCompare(String(vb));
    if (col === 'winPct') { var wa = (a.acq+a.denied) > 0 ? (a.acq/(a.acq+a.denied)) : 0, wb = (b.acq+b.denied) > 0 ? (b.acq/(b.acq+b.denied)) : 0; return proxiesSortDir * (wa - wb); }
    return proxiesSortDir * ((va||0) - (vb||0));
  });
  proxyRenderTable(rows);
}
function proxyRenderTable(rows) {
  var tbody = document.getElementById('proxies-active-body'), html = '';
  rows.forEach(function(r,i){
    var traffic = (r.rx||0)+(r.tx||0), acq = r.acq||0, den = r.denied||0, total = acq+den;
    var winPct = total > 0 ? (acq/total*100).toFixed(1) : '\u2014';
    var cl = winPct >= 90 ? 'g' : (winPct >= 70 ? 'y' : 'r');
    html += '<tr><td class=\"num\">'+(i+1)+'</td><td><span class=\"b\">'+r.addr+'</span></td><td class=\"num\">'+fmtBytes(traffic)+'</td><td class=\"num g\">'+acq+'</td><td class=\"num r\">'+den+'</td><td class=\"num '+cl+'\">'+winPct+(winPct!=='\u2014'?'%':'')+'</td><td class=\"num\">'+(r.nodes||0)+'</td></tr>';
  });
  tbody.innerHTML = html;
  document.getElementById('proxies-info').textContent = rows.length+' active';
  var idle = rows.filter(function(r){return (r.rx||0)+(r.tx||0)===0&&(r.acq||0)+(r.denied||0)===0;});
  document.getElementById('idle-count').textContent = ' ('+idle.length+' idle)';
  var ibody = document.getElementById('proxies-idle-body');
  if (idle.length===0) { ibody.innerHTML = '<tr><td colspan=\"3\" style=\"text-align:center;color:#475569\">No idle proxies</td></tr>'; }
  else { var ih=''; idle.forEach(function(r){ih+='<tr><td>'+r.addr+'</td><td class=\"num\">'+(r.nodes||0)+'</td><td class=\"num\">\u2014</td></tr>';}); ibody.innerHTML = ih; }
}
function loadProxies() {
  if (!document.getElementById('page-proxies').classList.contains('active')) return;
  var sort = document.getElementById('proxies-sort').value;
  var node = document.getElementById('proxies-node').value;
  var url = '/api/proxies/top?window='+proxiesWindow+'&sort='+sort+'&limit=100'+(node?'&node='+node:'');
  document.getElementById('proxies-info').textContent = 'Loading...';
  var thisSeq = ++proxiesRequestSeq;
  fetch(url).then(function(r){return r.json();}).then(function(rows){
    // A newer request (dropdown/window changed again while this one was
    // in flight) has already landed — a slow "all nodes" response must
    // never clobber a faster, more recent one.
    if (thisSeq !== proxiesRequestSeq) return;
    if (!rows || rows.length === 0) { document.getElementById('proxies-active-body').innerHTML = '<tr><td colspan="7" style="text-align:center;color:#64748b;padding:20px">No proxy data in this window</td></tr>'; document.getElementById('proxies-info').textContent = '0 proxies'; proxiesRows = []; return; }
    proxiesRows = rows;
    proxyRenderTable(rows);
  }).catch(function(){
    if (thisSeq !== proxiesRequestSeq) return;
    document.getElementById('proxies-info').textContent = 'Error loading';
  });
}
function toggleIdleProxies() {
  document.getElementById('proxies-idle-wrap').classList.toggle('hidden');
}

// === Contracts page ===
var contractsWindow = '7d';
function setContractsWindow(w, btn) {
  contractsWindow = w;
  document.querySelectorAll('#page-contracts .pill').forEach(function(p){p.classList.remove('on');});
  if (btn) btn.classList.add('on');
  loadContracts();
}
function loadContracts() {
  if (!document.getElementById('page-contracts').classList.contains('active')) return;
  document.getElementById('contracts-fleet-info').textContent = 'Loading...';
  // Fetch per-node contracts from /api/nodes/contracts for each node visible in the server table
  fetch('/api/nodes').then(function(r){return r.json();}).then(function(nodes){
    var nodeIDs = nodes.map(function(n){return n.node_id;});
    if (nodeIDs.length === 0) { document.getElementById('contracts-fleet-info').textContent = 'No nodes'; return; }
    var promises = nodeIDs.map(function(id){return fetch('/api/nodes/contracts?node='+id+'&window='+contractsWindow).then(function(r){return r.json();}).then(function(d){return {node:id,data:d};});});
    Promise.all(promises).then(function(results){
      // Build per-node totals
      var serverRows = [];
      var fleetAcq = 0, fleetDen = 0;
      var fleetByHour = {};
      results.forEach(function(r){
        var acq = 0, den = 0;
        if (r.data) {
          r.data.forEach(function(p){acq+=p.acq;den+=p.denied;var h=p.ts/3600;if(!fleetByHour[h])fleetByHour[h]={acq:0,den:0};fleetByHour[h].acq+=p.acq;fleetByHour[h].den+=p.denied;});
        }
        fleetAcq += acq; fleetDen += den;
        var total = acq + den;
        serverRows.push({node:r.node, acq:acq, den:den, total:total, pct:total>0?(acq/total*100).toFixed(1):'—'});
      });
      serverRows.sort(function(a,b){var pa=parseFloat(a.pct),pb=parseFloat(b.pct);if(isNaN(pa))return 1;if(isNaN(pb))return-1;return pb-pa;});
      document.getElementById('contracts-fleet-info').textContent = 'fleet: '+fleetAcq+' won / '+fleetDen+' denied / '+(fleetAcq+fleetDen>0?(fleetAcq/(fleetAcq+fleetDen)*100).toFixed(1)+'%':'—');
      // Render fleet chart
      var hours=Object.keys(fleetByHour).sort();
      var labels=[],acqArr=[],denArr=[];
      hours.forEach(function(h){labels.push(parseInt(h)*3600);acqArr.push(fleetByHour[h].acq);denArr.push(fleetByHour[h].den);});
      var el = document.getElementById('contracts-fleet-chart');
      if (contractsChart) { contractsChart.destroy(); contractsChart = null; }
      if (labels.length > 0) {
        contractsChart = new uPlot({width:el.clientWidth||700,height:160,cursor:{show:true},legend:{show:true},axes:[{stroke:'#64748b',grid:{stroke:'#1e293b',width:1}},{stroke:'#64748b',grid:{stroke:'#1e293b',width:1}}],series:[{label:'Time',value:'{HH}:{mm}'},{label:'Won',stroke:'#4ade80',fill:'rgba(74,222,128,0.15)',width:2},{label:'Denied',stroke:'#f87171',fill:'rgba(248,113,113,0.15)',width:2}]},[labels,acqArr,denArr],el);
      }
      // Render per-server table
      var body = document.getElementById('contracts-body');
      var html = '';
      serverRows.forEach(function(r){
        var pct = parseFloat(r.pct), cl = isNaN(pct)?'':(pct>=90?'g':(pct>=70?'y':'r'));
        html += '<tr onclick="openDrawer(\''+r.node+'\')" style="cursor:pointer">';
        html += '<td>'+r.node+'</td>';
        html += '<td class="num g">'+r.acq+'</td>';
        html += '<td class="num r">'+r.den+'</td>';
        html += '<td class="num '+cl+'">'+r.pct+(r.pct!=='—'?'%':'')+'</td>';
        var wp;
        if (r.total > 0) { var ww = Math.round(r.acq/r.total*100); wp = '<div class="win-bar"><div class="w" style="width:'+ww+'%"></div><div class="l" style="width:'+(100-ww)+'%"></div></div>'; }
        else { wp = '<div class="win-bar"><div class="l" style="width:100%"></div></div>'; }
        html += '<td>'+wp+'</td></tr>';
      });
      body.innerHTML = html;
    }).catch(function(){document.getElementById('contracts-fleet-info').textContent='Error';});
  }).catch(function(){document.getElementById('contracts-fleet-info').textContent='Error';});
}

// === Init ===
loadFleetChart();
refreshDashboard();
</script>
</body>
</html>
`))
