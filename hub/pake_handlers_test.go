package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func newTestPakeJoinState(t *testing.T, password string) *pakeJoinState {
	t.Helper()
	dir := t.TempDir()
	skm, err := loadOrCreatePakeServerKeys(dir)
	if err != nil {
		t.Fatal(err)
	}
	record, err := loadOrRegisterPakeJoin(dir, skm, password)
	if err != nil {
		t.Fatal(err)
	}
	return &pakeJoinState{skm: skm, record: record, pending: make(map[string]*pendingLogin)}
}

func TestHandleKE1AndKE3_FullHandshakeSucceeds(t *testing.T) {
	const password = "correct horse battery staple"
	st := newTestPakeJoinState(t, password)
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer s.db.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/join/ke1", st.HandleKE1)
	mux.HandleFunc("/api/join/ke3", st.HandleKE3(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	ke1Bytes, client, err := pakeClientLoginStep1(password)
	if err != nil {
		t.Fatalf("pakeClientLoginStep1: %v", err)
	}

	resp, err := http.Post(ts.URL+"/api/join/ke1", "application/json",
		bytes.NewReader(mustJSON(map[string]string{"ke1": hex.EncodeToString(ke1Bytes)})))
	if err != nil {
		t.Fatalf("POST ke1: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("KE1 status = %d, want 200", resp.StatusCode)
	}
	var ke1Resp struct {
		Ke2 string `json:"ke2"`
	}
	json.NewDecoder(resp.Body).Decode(&ke1Resp)
	resp.Body.Close()

	ke2Bytes, err := hex.DecodeString(ke1Resp.Ke2)
	if err != nil {
		t.Fatalf("decode ke2: %v", err)
	}
	ke3Bytes, sessionKey, err := pakeClientLoginFinish(client, ke2Bytes)
	if err != nil {
		t.Fatalf("pakeClientLoginFinish: %v", err)
	}

	resp, err = http.Post(ts.URL+"/api/join/ke3", "application/json",
		bytes.NewReader(mustJSON(map[string]string{
			"ke1":     hex.EncodeToString(ke1Bytes),
			"ke3":     hex.EncodeToString(ke3Bytes),
			"node_id": "test-node",
		})))
	if err != nil {
		t.Fatalf("POST ke3: %v", err)
	}
	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("KE3 status = %d, want 200: %s", resp.StatusCode, body)
	}
	var ke3Resp struct {
		Credential string `json:"credential"`
	}
	json.NewDecoder(resp.Body).Decode(&ke3Resp)
	resp.Body.Close()

	if ke3Resp.Credential != hex.EncodeToString(sessionKey) {
		t.Error("returned credential does not match the client's derived session key")
	}

	ok, _, err := s.validateCredential(context.Background(), ke3Resp.Credential)
	if err != nil {
		t.Fatalf("validateCredential: %v", err)
	}
	if !ok {
		t.Error("credential issued by a successful join does not validate")
	}

	// The pending entry must be consumed by KE3 — a second KE3 for the same
	// KE1 must be rejected, not silently re-accepted.
	resp, err = http.Post(ts.URL+"/api/join/ke3", "application/json",
		bytes.NewReader(mustJSON(map[string]string{
			"ke1":     hex.EncodeToString(ke1Bytes),
			"ke3":     hex.EncodeToString(ke3Bytes),
			"node_id": "test-node",
		})))
	if err != nil {
		t.Fatalf("POST ke3 (replay): %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("replayed KE3 status = %d, want 400", resp.StatusCode)
	}
}

func TestHandleKE1_BadHexRejected(t *testing.T) {
	st := newTestPakeJoinState(t, "pw")
	mux := http.NewServeMux()
	mux.HandleFunc("/api/join/ke1", st.HandleKE1)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/join/ke1", "application/json",
		bytes.NewReader(mustJSON(map[string]string{"ke1": "not valid hex!!"})))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 for malformed hex", resp.StatusCode)
	}
}

func TestHandleKE3_UnknownKE1Rejected(t *testing.T) {
	st := newTestPakeJoinState(t, "pw")
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer s.db.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/join/ke3", st.HandleKE3(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/join/ke3", "application/json",
		bytes.NewReader(mustJSON(map[string]string{
			"ke1":     hex.EncodeToString([]byte("deadbeefdeadbeef")),
			"ke3":     hex.EncodeToString([]byte("deadbeefdeadbeef")),
			"node_id": "n1",
		})))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 for a KE3 with no matching pending KE1", resp.StatusCode)
	}
}

func TestHandleKE3_ExpiredPendingRejected(t *testing.T) {
	const password = "correct horse battery staple"
	st := newTestPakeJoinState(t, password)

	ke1Bytes, client, err := pakeClientLoginStep1(password)
	if err != nil {
		t.Fatalf("pakeClientLoginStep1: %v", err)
	}
	ke2Bytes, serverOutput, err := pakeServerLoginStep1(st.skm, st.record, ke1Bytes)
	if err != nil {
		t.Fatalf("pakeServerLoginStep1: %v", err)
	}
	ke3Bytes, _, err := pakeClientLoginFinish(client, ke2Bytes)
	if err != nil {
		t.Fatalf("pakeClientLoginFinish: %v", err)
	}

	// Plant an already-expired pending entry, simulating a client that took
	// longer than pendingLoginTTL to send KE3.
	st.pending[pendingID(ke1Bytes)] = &pendingLogin{output: serverOutput, expiresAt: time.Now().Add(-time.Minute)}

	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatalf("openStore: %v", err)
	}
	defer s.db.Close()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/join/ke3", st.HandleKE3(s))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/api/join/ke3", "application/json",
		bytes.NewReader(mustJSON(map[string]string{
			"ke1":     hex.EncodeToString(ke1Bytes),
			"ke3":     hex.EncodeToString(ke3Bytes),
			"node_id": "n1",
		})))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("status = %d, want 400 for an expired pending KE1", resp.StatusCode)
	}
}

func TestSweepExpiredPendingRemovesOnlyExpired(t *testing.T) {
	st := &pakeJoinState{pending: make(map[string]*pendingLogin)}
	now := time.Now()

	st.pending["expired"] = &pendingLogin{expiresAt: now.Add(-time.Second)}
	st.pending["fresh"] = &pendingLogin{expiresAt: now.Add(time.Hour)}

	st.sweepExpiredPending(now)

	if _, ok := st.pending["expired"]; ok {
		t.Error("sweepExpiredPending left an expired entry in place")
	}
	if _, ok := st.pending["fresh"]; !ok {
		t.Error("sweepExpiredPending removed a non-expired entry")
	}
}
