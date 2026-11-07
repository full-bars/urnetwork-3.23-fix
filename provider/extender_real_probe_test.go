package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
)

type realProbeRequest struct {
	Mode            string
	Port            int
	PublicKey       []byte
	CamouflageKey   []byte
	DestinationHost string
	DestinationPort int
	APIURL          string
	Certificate     []byte
}

type realProbeResult struct {
	Address   string
	PublicKey []byte
	Signature []byte
	Error     string
}

// TestExtenderRealPlatformProbe runs the platform's own probe code (upstream
// connect's net_extender_probe.go and net_extender_camouflage_probe.go, built as a
// separate pinned module under testdata/realprobe) against this repo's real
// extender role, so the activation's required test is exercised by the code the
// real platform runs, not by a stand-in.
//
// It is opt-in. It builds a -race subprocess and fetches the pinned upstream
// module, so it needs the network and takes about a minute. Run it before a
// release, and whenever upstream connect's probe or the extender server changes:
//
//	URNETWORK_REAL_PROBE_TEST=1 go test -race -count=1 -run TestExtenderRealPlatformProbe ./provider/
func TestExtenderRealPlatformProbe(t *testing.T) {
	if os.Getenv("URNETWORK_REAL_PROBE_TEST") == "" {
		t.Skip("opt-in: set URNETWORK_REAL_PROBE_TEST=1 (needs the network, about a minute)")
	}
	// The subprocess is built with race instrumentation too. Its independent
	// module uses the platform's actual connect, not this fork's dialer.
	binary := filepath.Join(t.TempDir(), "realprobe")
	build := exec.Command("go", "build", "-race", "-o", binary, ".")
	build.Dir = "testdata/realprobe"
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build upstream probe: %v\n%s", err, output)
	}
	api := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/hello" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"client_address": r.RemoteAddr})
	}))
	defer api.Close()
	host, portText, err := net.SplitHostPort(api.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	apiPort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	type boundPort struct {
		configured string
		port       int
	}
	startRole := func(allowed []string) (*extenderRole, map[string]int) {
		config := testExtenderRoleConfig(t)
		config.allowedHosts = allowed
		tcp := make(chan int, 1)
		udp := make(chan boundPort, 2)
		config.listen = func(network, address string) (net.Listener, error) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err == nil {
				tcp <- listener.Addr().(*net.TCPAddr).Port
			}
			return listener, err
		}
		config.listenPacket = func(network, address string) (net.PacketConn, error) {
			packet, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err == nil {
				udp <- boundPort{address, packet.LocalAddr().(*net.UDPAddr).Port}
			}
			return packet, err
		}
		role, err := startExtenderRole(context.Background(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { closeExtenderRole(t, role) })
		waitExtenderRoleReady(t, role)
		snapshot := role.snapshot()
		if len(snapshot.ListenErrors) != 0 {
			t.Fatalf("listen errors: %v", snapshot.ListenErrors)
		}
		ports := map[string]int{"tcp": <-tcp}
		for range 2 {
			p := <-udp
			switch p.configured {
			case fmt.Sprintf(":%d", connect.ExtenderQuicPort):
				ports["quic"] = p.port
			case fmt.Sprintf(":%d", connect.ExtenderDnsPort):
				ports["dns"] = p.port
			default:
				t.Fatalf("unexpected packet bind %q", p.configured)
			}
		}
		return role, ports
	}
	role, ports := startRole([]string{host})
	role.mu.Lock()
	key, err := connect.ExtenderPublicKeyFromSeed(role.keySeed)
	role.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	base := realProbeRequest{PublicKey: key, DestinationHost: host, DestinationPort: apiPort, APIURL: api.URL, Certificate: api.Certificate().Raw}
	run := func(t *testing.T, req realProbeRequest) realProbeResult {
		t.Helper()
		input, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary)
		cmd.Stdin = bytes.NewReader(input)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		output, err := cmd.Output()
		if err != nil {
			t.Fatalf("probe process: %v: %s", err, stderr.String())
		}
		var result realProbeResult
		if err := json.Unmarshal(output, &result); err != nil {
			t.Fatalf("probe output %q: %v, stderr %s", output, err, stderr.String())
		}
		return result
	}
	for _, mode := range []string{"tcp", "quic", "dns"} {
		t.Run(mode+"_challenge", func(t *testing.T) {
			req := base
			req.Mode, req.Port = mode, ports[mode]
			result := run(t, req)
			if result.Error != "" {
				t.Fatalf("upstream probe: %s", result.Error)
			}
			if !bytes.Equal(result.PublicKey, key) || len(result.Signature) != ed25519.SignatureSize {
				t.Fatalf("wrong response key or signature length: key=%x signature length=%d", result.PublicKey, len(result.Signature))
			}
			t.Logf("upstream verified fresh challenge signature and identity on %s", mode)
		})
	}
	t.Run("legacy_forward", func(t *testing.T) {
		req := base
		req.Mode, req.Port = "forward", ports["tcp"]
		result := run(t, req)
		if result.Error != "" {
			t.Fatalf("upstream forward: %s", result.Error)
		}
		ip, _, err := net.SplitHostPort(result.Address)
		if err != nil || net.ParseIP(ip) == nil || net.ParseIP(ip).To4() == nil {
			t.Fatalf("forward address %q is not IPv4: %v", result.Address, err)
		}
		t.Logf("verified TLS /hello returned IPv4 address %s", result.Address)
	})
	t.Run("camouflage_key", func(t *testing.T) {
		role.mu.Lock()
		var camouflageKey []byte
		if len(role.servers) != 0 {
			camouflageKey = role.servers[0].RealityPublicKey()
		}
		role.mu.Unlock()
		if len(camouflageKey) == 0 {
			t.Skip("role did not publish a camouflage key")
		}
		req := base
		req.CamouflageKey = camouflageKey
		req.Mode, req.Port = "tcp", ports["tcp"]
		if result := run(t, req); result.Error != "" {
			t.Fatalf("upstream camouflage challenge: %s", result.Error)
		}
		req.Mode = "forward"
		if result := run(t, req); result.Error != "" {
			t.Fatalf("upstream camouflage forward: %s", result.Error)
		}
	})
	t.Run("denied_forward", func(t *testing.T) {
		denied, deniedPorts := startRole([]string{"unrelated.invalid"})
		denied.mu.Lock()
		deniedKey, err := connect.ExtenderPublicKeyFromSeed(denied.keySeed)
		denied.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		req := base
		req.Mode, req.Port, req.PublicKey = "forward", deniedPorts["tcp"], deniedKey
		result := run(t, req)
		if !strings.Contains(result.Error, "status 403") {
			t.Fatalf("expected authorization refusal 403, got %q", result.Error)
		}
		t.Logf("unlisted host rejected: %s", result.Error)
	})
}
