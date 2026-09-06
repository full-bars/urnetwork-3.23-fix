package urnettools

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
)

type mockControlServer struct {
	listener net.Listener
	requests []controlRequest
	values   map[string]string
	closed   bool
}

func startMockControlServer(t *testing.T, sockPath string) *mockControlServer {
	t.Helper()
	l, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen mock socket: %v", err)
	}
	s := &mockControlServer{
		listener: l,
		values:   make(map[string]string),
	}
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go s.handleConn(conn)
		}
	}()
	return s
}

func (s *mockControlServer) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Bytes()
		var req controlRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = json.NewEncoder(conn).Encode(controlResponse{OK: false, Error: "bad json"})
			return
		}
		s.requests = append(s.requests, req)
		switch req.Cmd {
		case "set":
			s.values[req.Key] = req.Value
			_ = json.NewEncoder(conn).Encode(controlResponse{OK: true})
		case "clear":
			delete(s.values, req.Key)
			_ = json.NewEncoder(conn).Encode(controlResponse{OK: true})
		case "get":
			val, found := s.values[req.Key]
			_ = json.NewEncoder(conn).Encode(controlResponse{OK: true, Value: val, Found: found})
		default:
			_ = json.NewEncoder(conn).Encode(controlResponse{OK: false, Error: "unknown cmd"})
		}
	}
}

func (s *mockControlServer) Close() {
	if !s.closed {
		s.closed = true
		_ = s.listener.Close()
	}
}

func TestControlClient_SocketReachableRoundTrip(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "provider.sock")
	server := startMockControlServer(t, sockPath)
	defer server.Close()

	p := Provider{StateDir: dir}

	legacyFile := filepath.Join(dir, "node_name")
	if err := os.WriteFile(legacyFile, []byte("old-legacy"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := applySetOverride(p, "node-name", "edge-prod-01", false); err != nil {
		t.Fatalf("applySetOverride set: %v", err)
	}

	if _, err := os.Stat(legacyFile); !os.IsNotExist(err) {
		t.Fatalf("legacy file %s should have been removed on socket set", legacyFile)
	}

	queueFile := filepath.Join(dir, "pending_overrides.json")
	if _, err := os.Stat(queueFile); !os.IsNotExist(err) {
		t.Fatalf("pending_overrides.json should not exist when socket is reachable")
	}

	if server.values["node_name"] != "edge-prod-01" {
		t.Fatalf("server value for node_name = %q, want edge-prod-01", server.values["node_name"])
	}

	val, src, found, err := queryControlOverride(p, "node_name")
	if err != nil {
		t.Fatalf("queryControlOverride: %v", err)
	}
	if !found || val != "edge-prod-01" || src != "socket" {
		t.Fatalf("queryControlOverride got (%q, %q, %v), want (edge-prod-01, socket, true)", val, src, found)
	}

	if err := applySetOverride(p, "node-name", "off", false); err != nil {
		t.Fatalf("applySetOverride clear: %v", err)
	}
	if _, ok := server.values["node_name"]; ok {
		t.Fatalf("node_name should be cleared from server, but still present")
	}

	_, _, foundAfterClear, _ := queryControlOverride(p, "node_name")
	if foundAfterClear {
		t.Fatalf("node_name should not be found after clear")
	}
}

func TestControlClient_SocketUnavailableQueueFallback(t *testing.T) {
	dir := t.TempDir()
	p := Provider{StateDir: dir}

	queueFile := filepath.Join(dir, "pending_overrides.json")

	if err := applySetOverride(p, "report-interval", "30s", false); err != nil {
		t.Fatalf("applySetOverride 1: %v", err)
	}

	b, err := os.ReadFile(queueFile)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	var ops []pendingOp
	if err := json.Unmarshal(b, &ops); err != nil {
		t.Fatalf("unmarshal queue file: %v", err)
	}
	if len(ops) != 1 || ops[0].Op != "set" || ops[0].Key != "report_interval" || ops[0].Value != "30s" {
		t.Fatalf("unexpected ops[0]: %+v", ops)
	}

	if err := applySetOverride(p, "gomemlimit", "512MiB", false); err != nil {
		t.Fatalf("applySetOverride 2: %v", err)
	}

	b, err = os.ReadFile(queueFile)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	if err := json.Unmarshal(b, &ops); err != nil {
		t.Fatalf("unmarshal queue file: %v", err)
	}
	if len(ops) != 2 {
		t.Fatalf("expected 2 ops in queue, got %d", len(ops))
	}
	if ops[1].Op != "set" || ops[1].Key != "gomemlimit" || ops[1].Value != "512MiB" {
		t.Fatalf("unexpected ops[1]: %+v", ops[1])
	}

	if err := applySetOverride(p, "report-interval", "off", false); err != nil {
		t.Fatalf("applySetOverride clear: %v", err)
	}

	b, err = os.ReadFile(queueFile)
	if err != nil {
		t.Fatalf("read queue file: %v", err)
	}
	if err := json.Unmarshal(b, &ops); err != nil {
		t.Fatalf("unmarshal queue file: %v", err)
	}
	if len(ops) != 3 {
		t.Fatalf("expected 3 ops in queue, got %d", len(ops))
	}
	if ops[2].Op != "clear" || ops[2].Key != "report_interval" {
		t.Fatalf("unexpected ops[2]: %+v", ops[2])
	}

	val, src, found, err := queryControlOverride(p, "gomemlimit")
	if err != nil || !found || val != "512MiB" || src != "pending" {
		t.Fatalf("queryControlOverride(gomemlimit) got (%q, %q, %v, %v), want (512MiB, pending, true, nil)", val, src, found, err)
	}

	_, _, reportFound, _ := queryControlOverride(p, "report_interval")
	if reportFound {
		t.Fatalf("report_interval should not be found after clear op was queued")
	}
}

func TestControlClient_All14KeysCanonicalizationAndValidation(t *testing.T) {
	dir := t.TempDir()
	p := Provider{StateDir: dir}

	testCases := []struct {
		cliKey       string
		canonicalKey string
		value        string
	}{
		{"node-name", "node_name", "node-1"},
		{"report-url", "report_url", "https://hub.example.com/reports"},
		{"report-interval", "report_interval", "15s"},
		{"fast-auth", "fast_auth", "on"},
		{"self-heal", "proxy_self_heal", "on"},
		{"proxy-url-max", "proxy_url_max", "1000"},
		{"proxy-url-refresh", "proxy_url_refresh", "2h"},
		{"cleanup-scope", "proxy_dead_cleanup_scope", "all"},
		{"cleanup-interval", "proxy_dead_cleanup_interval", "30m"},
		{"hot-restart", "hot_restart", "on"},
		{"gomemlimit", "gomemlimit", "1GiB"},
		{"gogc", "gogc", "200"},
		{"profile", "profile", "turbo-v4"},
		{"ramlogs", "ramlogs", "on"},
	}

	for _, tc := range testCases {
		canon, ok := canonicalControlKey(tc.cliKey)
		if !ok {
			t.Errorf("canonicalControlKey(%s) returned false", tc.cliKey)
		}
		if canon != tc.canonicalKey {
			t.Errorf("canonicalControlKey(%s) = %s, want %s", tc.cliKey, canon, tc.canonicalKey)
		}

		if err := validateControlValue(canon, tc.value); err != nil {
			t.Errorf("validateControlValue(%s, %s): unexpected error: %v", canon, tc.value, err)
		}

		if err := applySetOverride(p, tc.cliKey, tc.value, false); err != nil {
			t.Errorf("applySetOverride(%s, %s): unexpected error: %v", tc.cliKey, tc.value, err)
		}
	}

	data, err := os.ReadFile(filepath.Join(dir, "pending_overrides.json"))
	if err != nil {
		t.Fatalf("read queue: %v", err)
	}
	var ops []pendingOp
	if err := json.Unmarshal(data, &ops); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	if len(ops) != len(testCases) {
		t.Fatalf("expected %d ops in queue, got %d", len(testCases), len(ops))
	}
}

func TestControlClient_InvalidValuesRejected(t *testing.T) {
	dir := t.TempDir()
	p := Provider{StateDir: dir}

	badCases := []struct {
		key   string
		value string
	}{
		{"report-interval", "2s"},
		{"report-interval", "invalid"},
		{"proxy-url-max", "-5"},
		{"proxy-url-max", "abc"},
		{"cleanup-scope", "invalid"},
		{"cleanup-interval", "10s"},
		{"hot-restart", "maybe"},
		{"ramlogs", "sometimes"},
		{"profile", "super-fast"},
		{"gomemlimit", "not-a-size"},
		{"gogc", "hundred"},
	}

	for _, bc := range badCases {
		if err := applySetOverride(p, bc.key, bc.value, false); err == nil {
			t.Errorf("expected error for (%s, %s), got nil", bc.key, bc.value)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "pending_overrides.json")); !os.IsNotExist(err) {
		t.Fatalf("pending_overrides.json should not have been created for invalid values")
	}
}
