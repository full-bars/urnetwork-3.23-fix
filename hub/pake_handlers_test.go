package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// doHubJoin is the client-side entrypoint exercised below. On success it
// returns normally (main.go calls os.Exit(0) itself), so the happy path can
// run in-process. Every error path calls os.Exit(1) directly, so those are
// exercised via a re-exec'd subprocess (see runDoHubJoinSubprocess) rather
// than risking the test binary itself.

// TestDoHubJoin_FullRoundTripSucceeds exercises doHubJoin end to end against
// a real KE1/KE3 handler pair, the same way hub-join is used in production.
// This is a regression test for the PR that switched both POSTs to a single
// shared *http.Client with a 30s timeout (instead of two bare http.Post
// calls): if the client wiring were broken, one leg of the handshake would
// fail and the credential file would never be written.
func TestDoHubJoin_FullRoundTripSucceeds(t *testing.T) {
	const password = "hub join integration test password"
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

	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("HOSTNAME", "test-node")

	restoreStdin := setStdinFromString(t, password+"\n")
	defer restoreStdin()

	doHubJoin(context.Background(), ts.URL)

	credPath := filepath.Join(homeDir, ".urnetwork", "hub.credential")
	credBytes, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatalf("reading saved credential: %v", err)
	}
	credentialHex := strings.TrimSpace(string(credBytes))
	if credentialHex == "" {
		t.Fatal("doHubJoin wrote an empty credential file")
	}

	ok, nodeID, err := s.validateCredential(context.Background(), credentialHex)
	if err != nil {
		t.Fatalf("validateCredential: %v", err)
	}
	if !ok {
		t.Error("credential written by doHubJoin does not validate against the hub store")
	}
	if nodeID != "test-node" {
		t.Errorf("nodeID = %q, want %q", nodeID, "test-node")
	}
}

// setStdinFromString temporarily replaces os.Stdin with a pipe fed by s, and
// returns a func to restore the original os.Stdin. doHubJoin reads the
// join password from os.Stdin via io.ReadAll, so this lets the happy-path
// test run doHubJoin in-process without touching the real terminal.
func setStdinFromString(t *testing.T, s string) func() {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	orig := os.Stdin
	os.Stdin = r
	go func() {
		io.WriteString(w, s)
		w.Close()
	}()
	return func() { os.Stdin = orig }
}

// TestDoHubJoin_MalformedKE1ResponseBodyExitsWithDecodeError covers the new
// error handling added to doHubJoin: a KE1 response with a 200 status but a
// body that isn't valid JSON must abort the join with a clear "decode KE2
// response" error, instead of the previous behavior of silently ignoring
// the decode error and continuing with a zero-value Ke2 field. Because the
// error path calls os.Exit(1), it must run in a re-exec'd subprocess.
func TestDoHubJoin_MalformedKE1ResponseBodyExitsWithDecodeError(t *testing.T) {
	if os.Getenv("GO_WANT_HUB_JOIN_SUBPROCESS") == "1" {
		doHubJoin(context.Background(), os.Getenv("HUB_JOIN_TEST_URL"))
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/join/ke1", func(w http.ResponseWriter, r *http.Request) {
		// 200 status, but a body that fails JSON decoding.
		w.Write([]byte("{not valid json"))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	output, exitCode := runDoHubJoinSubprocess(t, ts.URL, "password\n")

	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1; output: %s", exitCode, output)
	}
	if !strings.Contains(output, "decode KE2 response") {
		t.Errorf("expected output to mention %q, got: %s", "decode KE2 response", output)
	}
}

// TestDoHubJoin_ValidJSONMissingKE2FieldDoesNotTriggerDecodeError is a
// boundary check on the same new error handling: a syntactically valid JSON
// body that simply omits the "ke2" field must NOT be reported as a decode
// error (json.Decode succeeds; Ke2 is just the empty string). The join
// still fails, but later, when the empty KE2 bytes fail to deserialize —
// confirming the new check is scoped to actual decode failures.
func TestDoHubJoin_ValidJSONMissingKE2FieldDoesNotTriggerDecodeError(t *testing.T) {
	if os.Getenv("GO_WANT_HUB_JOIN_SUBPROCESS") == "1" {
		doHubJoin(context.Background(), os.Getenv("HUB_JOIN_TEST_URL"))
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/join/ke1", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("{}"))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	output, exitCode := runDoHubJoinSubprocess(t, ts.URL, "password\n")

	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1; output: %s", exitCode, output)
	}
	if strings.Contains(output, "decode KE2 response") {
		t.Errorf("a missing (but syntactically valid) ke2 field should not be reported as a decode error, got: %s", output)
	}
	if !strings.Contains(output, "KE3") {
		t.Errorf("expected the join to fail later at the KE3 step, got: %s", output)
	}
}

// runDoHubJoinSubprocess re-executes the current test binary with
// -test.run restricted to the calling test, and GO_WANT_HUB_JOIN_SUBPROCESS
// set so that test's body calls doHubJoin(hubURL) directly and lets it run
// to completion (including any os.Exit call) in a separate process. This
// avoids os.Exit(1) inside doHubJoin's error paths from killing the real
// test binary.
func runDoHubJoinSubprocess(t *testing.T, hubURL, stdin string) (output string, exitCode int) {
	t.Helper()

	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$")
	cmd.Env = append(os.Environ(),
		"GO_WANT_HUB_JOIN_SUBPROCESS=1",
		"HUB_JOIN_TEST_URL="+hubURL,
		"HOME="+t.TempDir(),
	)
	cmd.Stdin = strings.NewReader(stdin)

	out, err := cmd.CombinedOutput()
	if err == nil {
		return string(out), 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return string(out), exitErr.ExitCode()
	}
	t.Fatalf("failed to run subprocess: %v; output: %s", err, out)
	return "", -1
}
