package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- newClientForURL tests ---

func TestNewClientForURL_HTTP(t *testing.T) {
	client := newClientForURL("http://example.com")
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	if client.Timeout != 10*time.Second {
		t.Errorf("expected 10s timeout, got %v", client.Timeout)
	}
	// HTTP should use the default transport (nil Transport field), no custom TLS config.
	if client.Transport != nil {
		t.Errorf("expected nil transport for HTTP, got %T", client.Transport)
	}
}

func TestNewClientForURL_HTTPS(t *testing.T) {
	client := newClientForURL("https://example.com")
	if client == nil {
		t.Fatal("expected non-nil client")
	}
	if client.Timeout != 10*time.Second {
		t.Errorf("expected 10s timeout, got %v", client.Timeout)
	}
	// HTTPS path may or may not get a custom transport depending on
	// x509.SystemCertPool() availability. In either fallback, the TLS
	// MinVersion should be TLS 1.2 when a transport is set.
	if client.Transport != nil {
		tr, ok := client.Transport.(*http.Transport)
		if !ok {
			t.Fatalf("expected *http.Transport, got %T", client.Transport)
		}
		if tr.TLSClientConfig == nil {
			t.Fatal("expected non-nil TLSClientConfig")
		}
		if tr.TLSClientConfig.MinVersion != tls.VersionTLS12 {
			t.Errorf("expected TLS MinVersion 1.2 (%d), got %d", tls.VersionTLS12, tr.TLSClientConfig.MinVersion)
		}
	}
	// Even when SystemCertPool fails (fallback path), timeout is still 10s
	// and a client is returned — never nil.
}

func TestNewClientForURL_InvalidURL(t *testing.T) {
	// newClientForURL never returns nil; it always produces a usable client.
	// A URL like "not-a-url" doesn't start with "https://", so it gets the
	// plain HTTP path — client is non-nil with 10s timeout.
	client := newClientForURL("not-a-url")
	if client == nil {
		t.Fatal("expected non-nil client for invalid URL")
	}
	if client.Timeout != 10*time.Second {
		t.Errorf("expected 10s timeout, got %v", client.Timeout)
	}
}

func TestNewClientForURL_EmptyURL(t *testing.T) {
	// Empty string is not https:// so it takes the plain path.
	client := newClientForURL("")
	if client == nil {
		t.Fatal("expected non-nil client for empty URL")
	}
	if client.Timeout != 10*time.Second {
		t.Errorf("expected 10s timeout, got %v", client.Timeout)
	}
}

// --- postHeartbeat tests ---

func TestPostHeartbeat_Success(t *testing.T) {
	var receivedContentType string
	var receivedBody []byte
	var method string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		receivedContentType = r.Header.Get("Content-Type")
		var err error
		receivedBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading request body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	client := newClientForURL(srv.URL)
	hb := heartbeatReport{
		NodeID:    "test-node-1",
		Timestamp: time.Now().UTC(),
		Uptime:    123.45,
		TotalRX:   1024,
		TotalTX:   2048,
		Clients:   5,
		System:    systemMetrics{HeapMiB: 32, SysMiB: 64, Connections: 10},
	}

	ok := postHeartbeat(context.Background(), client, srv.URL, hb)
	if !ok {
		t.Fatal("expected postHeartbeat to return true on 200")
	}
	if method != "POST" {
		t.Errorf("expected POST method, got %s", method)
	}
	if receivedContentType != "application/json" {
		t.Errorf("expected Content-Type application/json, got %s", receivedContentType)
	}
	// Verify body is valid JSON with expected fields.
	var decoded heartbeatReport
	if err := json.Unmarshal(receivedBody, &decoded); err != nil {
		t.Fatalf("response body is not valid JSON: %v\nbody: %s", err, receivedBody)
	}
	if decoded.NodeID != "test-node-1" {
		t.Errorf("expected node_id test-node-1, got %s", decoded.NodeID)
	}
	if decoded.TotalRX != 1024 {
		t.Errorf("expected rx 1024, got %d", decoded.TotalRX)
	}
	if decoded.Clients != 5 {
		t.Errorf("expected clients 5, got %d", decoded.Clients)
	}
}

func TestPostHeartbeat_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	client := newClientForURL(srv.URL)
	hb := heartbeatReport{
		NodeID: "test-node-2",
		System: systemMetrics{HeapMiB: 16, SysMiB: 32},
	}

	ok := postHeartbeat(context.Background(), client, srv.URL, hb)
	if ok {
		t.Fatal("expected postHeartbeat to return false on 500")
	}
}

func TestPostHeartbeat_NetworkError(t *testing.T) {
	// Use a port that nothing is listening on.
	client := newClientForURL("http://127.0.0.1:1")
	hb := heartbeatReport{
		NodeID: "test-node-3",
		System: systemMetrics{HeapMiB: 8, SysMiB: 16},
	}

	ok := postHeartbeat(context.Background(), client, "http://127.0.0.1:1", hb)
	if ok {
		t.Fatal("expected postHeartbeat to return false on connection refused")
	}
}

func TestPostHeartbeat_ContextCanceled(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before sending

	client := newClientForURL(srv.URL)
	hb := heartbeatReport{
		NodeID: "test-node-4",
		System: systemMetrics{HeapMiB: 8, SysMiB: 16},
	}

	ok := postHeartbeat(ctx, client, srv.URL, hb)
	if ok {
		t.Fatal("expected postHeartbeat to return false with canceled context")
	}
}

func TestPostHeartbeat_VerifyBodyIsNotTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("reading body: %v", err)
		}
		// The body must end with '}' (closing brace of the JSON object).
		if !strings.HasSuffix(strings.TrimSpace(string(body)), "}") {
			t.Errorf("body does not end with JSON object: %s", body)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	client := newClientForURL(srv.URL)
	hb := heartbeatReport{
		NodeID:  "test-node-5",
		Uptime:  999.99,
		TotalRX: 999999,
		TotalTX: 888888,
		Clients: 42,
		System:  systemMetrics{HeapMiB: 128, SysMiB: 256, Connections: 100, Pressure: 0.5},
		Proxies: []proxyStatus{
			{ID: "proxy-1", Status: "up", ContractsAcquired: 10, ContractsDenied: 2},
			{ID: "proxy-2", Status: "dead", ContractsAcquired: 0, ContractsDenied: 1},
		},
	}

	ok := postHeartbeat(context.Background(), client, srv.URL, hb)
	if !ok {
		t.Fatal("expected postHeartbeat to return true")
	}
}
