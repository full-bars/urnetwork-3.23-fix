package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestParseEthHexQuantity(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    uint64
		wantErr bool
	}{
		{name: "0x1", input: "0x1", want: 1},
		{name: "0x0", input: "0x0", want: 0},
		{name: "0xff", input: "0xff", want: 255},
		{name: "mixed case hex", input: "0xAbCd", want: 0xabcd},
		// Allowed: parseEthHexQuantity only TrimPrefix's the literal "0x"
		// and leaves bare hex untouched, so ParseUint then accepts it. This
		// is the actual current behaviour; a bare quantity is out of the
		// json-rpc spec but the leniency is intentional in the code today.
		{name: "no 0x prefix succeeds", input: "ff", want: 255},
		{name: "empty string errors", input: "", wantErr: true},
		{name: "bare 0x errors", input: "0x", wantErr: true},
		{name: "non-hex errors", input: "0xzz", wantErr: true},
		{name: "oversized value errors", input: "0xffffffffffffffffffffffff", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseEthHexQuantity(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseEthHexQuantity(%q) = %d, nil; want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEthHexQuantity(%q) unexpected error: %s", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("parseEthHexQuantity(%q) = %d; want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseEthHexBytes(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    []byte
		wantErr bool
	}{
		{name: "two bytes", input: "0x0102", want: []byte{1, 2}},
		{name: "single byte", input: "0x41", want: []byte{0x41}},
		{name: "mixed case", input: "0xAb", want: []byte{0xab}},
		// Both "0x" and "" decode to an empty, error-free slice: the
		// TrimPrefix is a no-op for "", and hex.DecodeString("") yields an
		// empty slice with no error. Assert that actual behaviour.
		{name: "bare 0x yields empty slice", input: "0x", want: []byte{}},
		{name: "empty string yields empty slice", input: "", want: []byte{}},
		{name: "odd length errors", input: "0x010", wantErr: true},
		{name: "non-hex errors", input: "0xzz", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseEthHexBytes(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseEthHexBytes(%q) = %x, nil; want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseEthHexBytes(%q) unexpected error: %s", tt.input, err)
			}
			if !bytesEqual(got, tt.want) {
				t.Errorf("parseEthHexBytes(%q) = %x; want %x", tt.input, got, tt.want)
			}
		})
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestEthRpcHexResult_Success(t *testing.T) {
	var gotMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("server read body: %s", err)
		}
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      int             `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("server could not parse request body %q: %s", body, err)
		}
		if req.JSONRPC != "2.0" {
			t.Errorf("request jsonrpc = %q; want 2.0", req.JSONRPC)
		}
		if req.ID != 1 {
			t.Errorf("request id = %d; want 1", req.ID)
		}
		gotMethod = req.Method
		if len(req.Params) == 0 {
			t.Errorf("request carries no params array")
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"0xdeadbeef"}`)
	}))
	defer server.Close()

	result, err := ethRpcHexResult(context.Background(), server.URL, "eth_call", []any{
		map[string]any{"to": "0x1234", "data": "0xabcd"},
		"latest",
	})
	if err != nil {
		t.Fatalf("ethRpcHexResult unexpected error: %s", err)
	}
	if result != "0xdeadbeef" {
		t.Errorf("ethRpcHexResult = %q; want %q", result, "0xdeadbeef")
	}
	if gotMethod != "eth_call" {
		t.Errorf("the method the function sent in the body = %q; want eth_call", gotMethod)
	}
}

func TestEthRpcHexResult_RpcError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// HTTP 200; the error lives inside the JSON-RPC envelope, exactly
		// how real EVM nodes report a revert.
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"execution reverted"}}`)
	}))
	defer server.Close()

	_, err := ethRpcHexResult(context.Background(), server.URL, "eth_call", []any{})
	if err == nil {
		t.Fatal("expected an error for a json-rpc-level error result")
	}
	if !strings.Contains(err.Error(), "-32000") {
		t.Errorf("error %q does not mention code -32000", err)
	}
	if !strings.Contains(err.Error(), "execution reverted") {
		t.Errorf("error %q does not mention message %q", err, "execution reverted")
	}
}

func TestEthRpcHexResult_HttpError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	_, err := ethRpcHexResult(context.Background(), server.URL, "eth_call", []any{})
	if err == nil {
		t.Fatal("expected an error for http 500")
	}
	if !strings.Contains(err.Error(), "http 500") {
		t.Errorf("error %q does not mention %q", err, "http 500")
	}
}

func TestEthRpcHexResult_MalformedJson(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `not json`)
	}))
	defer server.Close()

	_, err := ethRpcHexResult(context.Background(), server.URL, "eth_call", []any{})
	if err == nil {
		t.Fatal("expected an error for a malformed response body")
	}
	if !strings.Contains(err.Error(), "bad json-rpc response") {
		t.Errorf("error %q does not mention %q", err, "bad json-rpc response")
	}
}

func TestEthRpcHexResult_NonStringResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"nested":"object"}}`)
	}))
	defer server.Close()

	_, err := ethRpcHexResult(context.Background(), server.URL, "eth_call", []any{})
	if err == nil {
		t.Fatal("expected an error when result is present but not a json string")
	}
	if !strings.Contains(err.Error(), "non-string result") {
		t.Errorf("error %q does not mention %q", err, "non-string result")
	}
}

func TestEthRpcHexResult_ContextCancelled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The cancellation must beat the round-trip regardless of what the
		// server would answer, so the handler just stalls out the timeout.
		time.Sleep(2 * time.Second)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"0x1"}`)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call

	start := time.Now()
	_, err := ethRpcHexResult(ctx, server.URL, "eth_chainId", []any{})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from an already-cancelled context")
	}
	// context.Canceled surfaces immediately on the request path; well under
	// ethRpcTimeout (15s) and far under the spec's 1s guard.
	if elapsed > time.Second {
		t.Errorf("call took %s after cancellation; expected a prompt failure (well under 1s)", elapsed)
	}
}

// snRPCServer returns an httptest server speaking a minimal eth_chainId +
// eth_call json-rpc surface, answering chainId with the given hex quantity
// and eth_call with the given hex data.
func snRPCServer(chainId string, callResult string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		switch req.Method {
		case "eth_chainId":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%q}`, chainId)
		case "eth_call":
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":%q}`, callResult)
		default:
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`)
		}
	}))
}

// wantSnDigest is a distinctive 32-byte headBindDigest the fake eth_call
// endpoints return, chosen so a short-return or off-by-one regression can't
// silently pass on an all-zero value.
func wantSnDigest() (out [32]byte) {
	for i := range out {
		out[i] = byte(i + 1)
	}
	return out
}

func TestSnReadHeadBindDigest_FirstEndpointSucceeds(t *testing.T) {
	want := wantSnDigest()
	server := snRPCServer("0x1", "0x"+hex.EncodeToString(want[:]))
	defer server.Close()

	digest, chainId, rpcUrl, err := snReadHeadBindDigest(context.Background(),
		[]string{server.URL}, "0x1234", []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("snReadHeadBindDigest unexpected error: %s", err)
	}
	if digest != want {
		t.Errorf("digest = %x; want %x", digest, want)
	}
	if chainId != 1 {
		t.Errorf("chainId = %d; want 1", chainId)
	}
	if rpcUrl != server.URL {
		t.Errorf("rpcUrl = %q; want %q", rpcUrl, server.URL)
	}
}

func TestSnReadHeadBindDigest_FailoverToSecondEndpoint(t *testing.T) {
	want := wantSnDigest()

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad.Close()

	good := snRPCServer("0x1", "0x"+hex.EncodeToString(want[:]))
	defer good.Close()

	digest, chainId, rpcUrl, err := snReadHeadBindDigest(context.Background(),
		[]string{bad.URL, good.URL}, "0x1234", []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("snReadHeadBindDigest should fail over to the second endpoint, got error: %s", err)
	}
	if rpcUrl != good.URL {
		t.Errorf("rpcUrl = %q; want the second (working) endpoint %q", rpcUrl, good.URL)
	}
	if chainId != 1 {
		t.Errorf("chainId = %d; want 1", chainId)
	}
	if digest != want {
		t.Errorf("digest = %x; want %x", digest, want)
	}
}

func TestSnReadHeadBindDigest_ShortReturnData(t *testing.T) {
	// eth_call returns valid hex but only 4 bytes (< 32): that endpoint must
	// be treated as unusable (printed as "expected >= 32"), not trusted.
	server := snRPCServer("0x1", "0x1234")
	defer server.Close()

	old := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %s", err)
	}
	os.Stdout = w
	defer func() { os.Stdout = old }()

	_, _, _, callErr := snReadHeadBindDigest(context.Background(),
		[]string{server.URL}, "0x1234", []byte{1, 2, 3})

	w.Close()
	out, _ := io.ReadAll(r)
	os.Stdout = old

	if callErr == nil {
		t.Fatal("short headBindDigest data must fail when it is the only endpoint")
	}
	if callErr.Error() != "no --rpc endpoint answered headBindDigest" {
		t.Errorf("error = %q; want %q", callErr, "no --rpc endpoint answered headBindDigest")
	}
	if !strings.Contains(string(out), "expected >= 32") {
		t.Errorf("stdout %q does not mention %q", out, "expected >= 32")
	}
}

func TestSnReadHeadBindDigest_BadChainIdFailsOverToNextEndpoint(t *testing.T) {
	want := wantSnDigest()

	// First endpoint answers eth_chainId with a non-hex value. Per
	// snReadHeadBindDigest, a parseEthHexQuantity failure on eth_chainId
	// must be treated as a per-endpoint failure (printed, then `continue`),
	// not a fatal error — the loop must still try the next endpoint.
	badChainId := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Method string `json:"method"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		if req.Method == "eth_chainId" {
			io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":"0xzz"}`)
			return
		}
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"method not found"}}`)
	}))
	defer badChainId.Close()

	good := snRPCServer("0x1", "0x"+hex.EncodeToString(want[:]))
	defer good.Close()

	digest, chainId, rpcUrl, err := snReadHeadBindDigest(context.Background(),
		[]string{badChainId.URL, good.URL}, "0x1234", []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("snReadHeadBindDigest should fail over past a malformed eth_chainId, got error: %s", err)
	}
	if rpcUrl != good.URL {
		t.Errorf("rpcUrl = %q; want the second (working) endpoint %q", rpcUrl, good.URL)
	}
	if chainId != 1 {
		t.Errorf("chainId = %d; want 1", chainId)
	}
	if digest != want {
		t.Errorf("digest = %x; want %x", digest, want)
	}
}

func TestSnReadHeadBindDigest_AllEndpointsFail(t *testing.T) {
	bad1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad1.Close()
	bad2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer bad2.Close()

	_, _, _, err := snReadHeadBindDigest(context.Background(),
		[]string{bad1.URL, bad2.URL}, "0x1234", []byte{1, 2, 3})
	if err == nil {
		t.Fatal("expected an error when every endpoint fails")
	}
	if err.Error() != "no --rpc endpoint answered headBindDigest" {
		t.Errorf("error = %q; want %q", err, "no --rpc endpoint answered headBindDigest")
	}
}
