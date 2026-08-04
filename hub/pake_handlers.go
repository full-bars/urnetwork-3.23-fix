package main

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bytemare/opaque"
)

// pendingLoginTTL bounds how long a KE1 can sit in pakeJoinState.pending
// waiting for its matching KE3. OPAQUE's KE1 always succeeds regardless of
// password correctness (failure only surfaces at KE3/LoginFinish), so every
// join attempt — including ones from a client that never follows up —
// inserts an entry. Without a TTL, abandoned handshakes accumulate in memory
// forever; the rate limiter only bounds insertion rate per IP, not total
// standing memory.
const pendingLoginTTL = 2 * time.Minute

type pendingLogin struct {
	output    *opaque.ServerOutput
	expiresAt time.Time
}

// pakeJoinState holds the server-side OPAQUE state for the join handshake.
// Initialized once at startup if a CA password exists.
type pakeJoinState struct {
	mu sync.RWMutex

	skm     *opaque.ServerKeyMaterial
	record  *opaque.ClientRecord
	pending map[string]*pendingLogin
}

func initPakeJoinState(dataDir string) (*pakeJoinState, error) {
	password, _, _, err := loadOrCreateCAMaterial(dataDir)
	if err != nil {
		return nil, fmt.Errorf("CA material: %w", err)
	}
	skm, err := loadOrCreatePakeServerKeys(dataDir)
	if err != nil {
		return nil, fmt.Errorf("PAKE server keys: %w", err)
	}
	record, err := loadOrRegisterPakeJoin(dataDir, skm, password)
	if err != nil {
		return nil, fmt.Errorf("PAKE register: %w", err)
	}
	st := &pakeJoinState{skm: skm, record: record, pending: make(map[string]*pendingLogin)}
	go st.cleanupPendingLoop()
	return st, nil
}

func (st *pakeJoinState) cleanupPendingLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		st.sweepExpiredPending(time.Now())
	}
}

// sweepExpiredPending removes pending logins whose TTL has elapsed as of now.
// Split out from cleanupPendingLoop so it can be exercised directly in tests
// without waiting on the real ticker.
func (st *pakeJoinState) sweepExpiredPending(now time.Time) {
	st.mu.Lock()
	defer st.mu.Unlock()
	for id, p := range st.pending {
		if now.After(p.expiresAt) {
			delete(st.pending, id)
		}
	}
}

// pendingID correlates a KE1 with its later KE3 by hashing the KE1 bytes —
// this is a correlation key, not a security boundary (the handshake itself
// is what authenticates), so a cheap prefix hash is sufficient.
func pendingID(ke1Bytes []byte) string {
	return hex.EncodeToString(ke1Bytes[:min(16, len(ke1Bytes))])
}

func (st *pakeJoinState) HandleKE1(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.Error(w, "POST only", 405)
		return
	}
	var req struct {
		Ke1 string `json:"ke1"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}

	ke1Bytes, err := hex.DecodeString(req.Ke1)
	if err != nil {
		http.Error(w, "bad hex in ke1", 400)
		return
	}
	ke2Bytes, serverOutput, err := pakeServerLoginStep1(st.skm, st.record, ke1Bytes)
	if err != nil {
		http.Error(w, fmt.Sprintf("KE1 rejected: %v", err), 401)
		return
	}

	id := pendingID(ke1Bytes)
	st.mu.Lock()
	st.pending[id] = &pendingLogin{output: serverOutput, expiresAt: time.Now().Add(pendingLoginTTL)}
	st.mu.Unlock()

	json.NewEncoder(w).Encode(map[string]string{
		"ke2": hex.EncodeToString(ke2Bytes),
	})
}

func (st *pakeJoinState) HandleKE3(s *store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", 405)
			return
		}
		var req struct {
			Ke1    string `json:"ke1"`
			Ke3    string `json:"ke3"`
			NodeID string `json:"node_id"`
			Host   string `json:"host,omitempty"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", 400)
			return
		}

		ke1Bytes, err := hex.DecodeString(req.Ke1)
		if err != nil {
			http.Error(w, "bad hex in ke1", 400)
			return
		}
		ke3Bytes, err := hex.DecodeString(req.Ke3)
		if err != nil {
			http.Error(w, "bad hex in ke3", 400)
			return
		}
		id := pendingID(ke1Bytes)

		st.mu.Lock()
		pending, ok := st.pending[id]
		if ok {
			delete(st.pending, id)
		}
		st.mu.Unlock()
		if !ok || time.Now().After(pending.expiresAt) {
			http.Error(w, "unknown or expired KE1", 400)
			return
		}

		sessionKey, err := pakeServerLoginFinish(pending.output, ke3Bytes)
		if err != nil {
			http.Error(w, fmt.Sprintf("KE3 rejected: %v", err), 401)
			return
		}

		credentialHex := hex.EncodeToString(sessionKey)
		if req.NodeID != "" && s != nil {
			if err := s.storeCredential(req.NodeID, credentialHex); err != nil {
				fmt.Fprintf(os.Stderr, "hub: PAKE credential storage failed: %v\n", err)
			}
		}

		json.NewEncoder(w).Encode(map[string]string{
			"credential": credentialHex,
		})
	}
}

type joinRateLimiter struct {
	mu       sync.Mutex
	attempts map[string]int
	resets   map[string]time.Time
}

var joinLimiter = &joinRateLimiter{
	attempts: make(map[string]int),
	resets:   make(map[string]time.Time),
}

func (rl *joinRateLimiter) checkJoinRateLimit(ip string) bool {
	const maxAttempts = 5
	const window = time.Minute * 5

	now := time.Now()
	lastReset := rl.resets[ip]
	if now.Sub(lastReset) > window {
		rl.attempts[ip] = 0
		rl.resets[ip] = now
	}

	if rl.attempts[ip] >= maxAttempts {
		return false
	}
	rl.attempts[ip]++
	return true
}

func (rl *joinRateLimiter) middleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rl.mu.Lock()
		ok := rl.checkJoinRateLimit(clientIP(r))
		rl.mu.Unlock()
		if !ok {
			http.Error(w, "rate limited - too many join attempts", 429)
			return
		}
		next(w, r)
	}
}

func (rl *joinRateLimiter) cleanupLoop() {
	for {
		time.Sleep(time.Minute * 5)
		rl.mu.Lock()
		now := time.Now()
		for ip, reset := range rl.resets {
			if now.Sub(reset) > time.Minute*6 {
				delete(rl.attempts, ip)
				delete(rl.resets, ip)
			}
		}
		rl.mu.Unlock()
	}
}

// doHubJoin is the client-side PAKE join command. It delegates the actual
// handshake steps to pakeClientLoginStep1/pakeClientLoginFinish (pake_join.go)
// rather than reimplementing them, so this production entrypoint runs the
// same code the unit tests exercise.
func doHubJoin(hubURL string) {
	passwordBytes, err := io.ReadAll(os.Stdin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hub-join: read password: %v\n", err)
		os.Exit(1)
	}
	password := strings.TrimSpace(string(passwordBytes))
	if password == "" {
		fmt.Fprintf(os.Stderr, "hub-join: no password on stdin\n")
		os.Exit(1)
	}

	baseURL := strings.TrimRight(hubURL, "/")
	check := func(err error, msg string) {
		if err != nil {
			fmt.Fprintf(os.Stderr, "hub-join: %s: %v\n", msg, err)
			os.Exit(1)
		}
	}

	ke1Bytes, client, err := pakeClientLoginStep1(password)
	check(err, "KE1")

	httpClient := &http.Client{Timeout: 30 * time.Second}

	resp, err := httpClient.Post(baseURL+"/api/join/ke1", "application/json",
		bytes.NewReader(mustJSON(map[string]string{"ke1": hex.EncodeToString(ke1Bytes)})))
	check(err, "POST KE1")
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Fprintf(os.Stderr, "hub-join: KE1 rejected: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}
	var ke1Resp struct {
		Ke2 string `json:"ke2"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ke1Resp); err != nil {
		resp.Body.Close()
		check(err, "decode KE2 response")
	}
	resp.Body.Close()

	ke2Bytes, err := hex.DecodeString(ke1Resp.Ke2)
	check(err, "decode KE2 hex")

	ke3Bytes, sessionKey, err := pakeClientLoginFinish(client, ke2Bytes)
	check(err, "KE3")
	credentialHex := hex.EncodeToString(sessionKey)

	nodeID := os.Getenv("HOSTNAME")
	if nodeID == "" {
		host, _ := os.Hostname()
		nodeID = host
	}

	resp, err = httpClient.Post(baseURL+"/api/join/ke3", "application/json",
		bytes.NewReader(mustJSON(map[string]string{
			"ke1":     hex.EncodeToString(ke1Bytes),
			"ke3":     hex.EncodeToString(ke3Bytes),
			"node_id": nodeID,
		})))
	check(err, "POST KE3")
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Fprintf(os.Stderr, "hub-join: KE3 rejected: %s\n", strings.TrimSpace(string(body)))
		os.Exit(1)
	}
	resp.Body.Close()

	credDir := filepath.Join(os.Getenv("HOME"), ".urnetwork")
	credPath := filepath.Join(credDir, "hub.credential")
	if err := os.MkdirAll(credDir, 0700); err == nil {
		os.WriteFile(credPath, []byte(credentialHex+"\n"), 0600)
		fmt.Printf("hub-join: credential saved to %s\n", credPath)
	}

	fmt.Println("hub-join: PAKE join successful")
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}
