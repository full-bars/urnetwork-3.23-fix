package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"
)

func TestExtenderRoleRequested(t *testing.T) {
	tests := map[string]bool{
		"":         false,
		"off":      false,
		"0":        false,
		"false":    false,
		"on":       true,
		"ON":       true,
		" 1 ":      true,
		"true":     true,
		"yes":      false,
		"extender": false,
	}
	for value, want := range tests {
		got := extenderRoleRequested(func(string) string { return value })
		if got != want {
			t.Errorf("URNETWORK_EXTENDER=%q: got %v, want %v", value, got, want)
		}
	}
}

func TestExtenderDnsPrivilegedPortRequested(t *testing.T) {
	tests := map[string]bool{
		"":    false,
		"off": false,
		"0":   false,
		"on":  true,
		"1":   true,
		"yes": false,
	}
	for value, want := range tests {
		got := extenderDnsPrivilegedPortRequested(func(string) string { return value })
		if got != want {
			t.Errorf("URNETWORK_EXTENDER_DNS53=%q: got %v, want %v", value, got, want)
		}
	}
}

func TestExtenderRoleEligibleOnlyForDirectSlot(t *testing.T) {
	proxied := &connect.ProxySettings{Address: "127.0.0.1:1080"}
	tests := []struct {
		name          string
		proxySettings *connect.ProxySettings
		directEnabled bool
		want          bool
	}{
		{"direct slot", nil, true, true},
		{"direct disabled", nil, false, false},
		{"proxied slot", proxied, true, false},
		{"proxied and direct disabled", proxied, false, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := extenderRoleEligible(test.proxySettings, test.directEnabled); got != test.want {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
}

func TestLoadOrCreateExtenderKeySeed(t *testing.T) {
	path := filepath.Join(t.TempDir(), extenderKeyFileName)

	seed, created, err := loadOrCreateExtenderKeySeed(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created {
		t.Fatal("first use did not create the key")
	}
	if len(seed) != 32 {
		t.Fatalf("seed is %d bytes, want 32", len(seed))
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0600 {
		t.Fatalf("key mode is %04o, want 0600", mode)
	}

	again, created, err := loadOrCreateExtenderKeySeed(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if created {
		t.Fatal("reload regenerated the key")
	}
	if string(again) != string(seed) {
		t.Fatal("reload returned a different seed")
	}
}

func TestLoadOrCreateExtenderKeySeedRefusesCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), extenderKeyFileName)
	corrupt := []byte("short")
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}

	_, _, err := loadOrCreateExtenderKeySeed(path)
	if err == nil {
		t.Fatal("a corrupt key file was accepted")
	}
	if !strings.Contains(err.Error(), "refusing to replace") {
		t.Fatalf("unexpected error: %v", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(after) != string(corrupt) {
		t.Fatal("the corrupt key file was silently replaced")
	}
}

func TestDefaultExtenderRoleConfigPorts(t *testing.T) {
	config := defaultExtenderRoleConfig()

	modes := config.ports[connect.ExtenderTcpPort]
	if !containsConnectMode(modes, connect.ExtenderConnectModeTcpTls) {
		t.Fatalf("tcp %d does not carry the tcp carrier: %v", connect.ExtenderTcpPort, modes)
	}
	if !containsConnectMode(modes, connect.ExtenderConnectModeQuic) {
		t.Fatalf("tcp %d does not carry the quic carrier: %v", connect.ExtenderTcpPort, modes)
	}
	dnsModes := config.ports[connect.ExtenderDnsPort]
	if !containsConnectMode(dnsModes, connect.ExtenderConnectModeDns) {
		t.Fatalf("dns %d does not carry the dns carrier: %v", connect.ExtenderDnsPort, dnsModes)
	}
	if config.retryInterval != extenderTcpRetryInterval {
		t.Fatalf("retry interval is %s, want %s", config.retryInterval, extenderTcpRetryInterval)
	}
}

func containsConnectMode(modes []connect.ExtenderConnectMode, want connect.ExtenderConnectMode) bool {
	for _, mode := range modes {
		if mode == want {
			return true
		}
	}
	return false
}

func testExtenderRoleConfig(t *testing.T) extenderRoleConfig {
	t.Helper()
	return extenderRoleConfig{
		keyPath: filepath.Join(t.TempDir(), extenderKeyFileName),
		ports: map[int][]connect.ExtenderConnectMode{
			connect.ExtenderTcpPort: {
				connect.ExtenderConnectModeTcpTls,
				connect.ExtenderConnectModeQuic,
			},
			connect.ExtenderDnsPort: {
				connect.ExtenderConnectModeDns,
			},
		},
		retryInterval: time.Second,
		listen: func(network string, address string) (net.Listener, error) {
			return net.Listen("tcp", "127.0.0.1:0")
		},
		listenPacket: func(network string, address string) (net.PacketConn, error) {
			return net.ListenPacket("udp", "127.0.0.1:0")
		},
		forwardDialer: &net.Dialer{Timeout: time.Second},
	}
}

func waitExtenderRoleReady(t *testing.T, role *extenderRole) {
	t.Helper()
	select {
	case <-role.Ready():
	case <-time.After(15 * time.Second):
		t.Fatal("extender role did not finish its first bind attempt")
	}
}

func closeExtenderRole(t *testing.T, role *extenderRole) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		role.CloseAndWait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("extender role did not stop")
	}
}

func TestExtenderRoleBindsAllCarriersAndStopsCleanly(t *testing.T) {
	config := testExtenderRoleConfig(t)
	role, err := startExtenderRole(context.Background(), config)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitExtenderRoleReady(t, role)

	snapshot := role.snapshot()
	if snapshot.State != extenderStateServing {
		t.Fatalf("state is %q, want %q (listen errors: %v)", snapshot.State, extenderStateServing, snapshot.ListenErrors)
	}
	for _, carrier := range []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	} {
		if !containsString(snapshot.Carriers, carrier) {
			t.Fatalf("carrier %q is not listening; carriers=%v errors=%v", carrier, snapshot.Carriers, snapshot.ListenErrors)
		}
	}
	if len(snapshot.DnsPorts) != 1 || snapshot.DnsPorts[0] != connect.ExtenderDnsPort {
		t.Fatalf("dns ports are %v, want [%d]", snapshot.DnsPorts, connect.ExtenderDnsPort)
	}
	metrics := renderExtenderRoleMetrics(snapshot)
	for _, want := range []string{
		"urnet_extender_enabled 1",
		`urnet_extender_state{state="serving"} 1`,
		`urnet_extender_carrier_listening{carrier="tcp"} 1`,
		`urnet_extender_carrier_listening{carrier="quic"} 1`,
		`urnet_extender_carrier_listening{carrier="dns"} 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics are missing %q:\n%s", want, metrics)
		}
	}

	closeExtenderRole(t, role)
	if state := role.snapshot().State; state != extenderStateClosed {
		t.Fatalf("state after close is %q, want %q", state, extenderStateClosed)
	}
}

func TestExtenderRoleKeepsServingWhenTcpIsTaken(t *testing.T) {
	config := testExtenderRoleConfig(t)
	config.listen = func(network string, address string) (net.Listener, error) {
		return nil, errors.New("bind: address already in use")
	}
	role, err := startExtenderRole(context.Background(), config)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitExtenderRoleReady(t, role)
	t.Cleanup(func() { globalExtenderRole.Store(nil) })

	snapshot := role.snapshot()
	if snapshot.State != extenderStateTcpUnavailable {
		t.Fatalf("state is %q, want %q", snapshot.State, extenderStateTcpUnavailable)
	}
	if containsString(snapshot.Carriers, connect.ExtenderCarrierTcp) {
		t.Fatalf("tcp is claimed listening: %v", snapshot.Carriers)
	}
	for _, carrier := range []string{connect.ExtenderCarrierQuic, connect.ExtenderCarrierDns} {
		if !containsString(snapshot.Carriers, carrier) {
			t.Fatalf("carrier %q is not listening; carriers=%v", carrier, snapshot.Carriers)
		}
	}
	if !strings.Contains(snapshot.ListenErrors[connect.ExtenderCarrierTcp], "address already in use") {
		t.Fatalf("tcp listen error is not recorded: %v", snapshot.ListenErrors)
	}
	metrics := renderExtenderRoleMetrics(snapshot)
	for _, want := range []string{
		`urnet_extender_state{state="tcp_unavailable"} 1`,
		`urnet_extender_carrier_listening{carrier="tcp"} 0`,
		`urnet_extender_carrier_listen_error{carrier="tcp"} 1`,
		`urnet_extender_carrier_listening{carrier="quic"} 1`,
	} {
		if !strings.Contains(metrics, want) {
			t.Fatalf("metrics are missing %q:\n%s", want, metrics)
		}
	}

	closeExtenderRole(t, role)
}

func TestExtenderRoleProductionPortsLiveBind(t *testing.T) {
	// Opt-in: 443 is privileged on most hosts, and this binds the real
	// production ports rather than the loopback ephemeral ones. Run it as
	// root with URNETWORK_EXTENDER_LIVE_BIND=1 when verifying a release.
	if os.Getenv("URNETWORK_EXTENDER_LIVE_BIND") != "1" {
		t.Skip("set URNETWORK_EXTENDER_LIVE_BIND=1 (and run privileged) to bind the production ports")
	}
	config := defaultExtenderRoleConfig()
	config.keyPath = filepath.Join(t.TempDir(), extenderKeyFileName)
	config.retryInterval = 500 * time.Millisecond
	role, err := startExtenderRole(context.Background(), config)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitExtenderRoleReady(t, role)
	t.Cleanup(func() { globalExtenderRole.Store(nil) })

	snapshot := role.snapshot()
	if snapshot.State != extenderStateServing {
		t.Fatalf("state is %q, want %q (listen errors: %v)", snapshot.State, extenderStateServing, snapshot.ListenErrors)
	}
	for _, carrier := range []string{
		connect.ExtenderCarrierTcp,
		connect.ExtenderCarrierQuic,
		connect.ExtenderCarrierDns,
	} {
		if !containsString(snapshot.Carriers, carrier) {
			t.Fatalf("carrier %q is not listening; carriers=%v errors=%v", carrier, snapshot.Carriers, snapshot.ListenErrors)
		}
	}
	if !slices.Contains(snapshot.DnsPorts, connect.ExtenderDnsPort) {
		t.Fatalf("dns ports are %v, want %d", snapshot.DnsPorts, connect.ExtenderDnsPort)
	}
	closeExtenderRole(t, role)
}

func TestExtenderRoleDns53FailureKeepsServing(t *testing.T) {
	config := testExtenderRoleConfig(t)
	config.dnsPrivilegedPort = true
	config.listenPacket = func(network string, address string) (net.PacketConn, error) {
		if address == ":53" {
			return nil, errors.New("bind: address already in use")
		}
		return net.ListenPacket("udp", "127.0.0.1:0")
	}
	role, err := startExtenderRole(context.Background(), config)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	waitExtenderRoleReady(t, role)
	t.Cleanup(func() { globalExtenderRole.Store(nil) })

	snapshot := role.snapshot()
	if snapshot.State != extenderStateServing {
		t.Fatalf("state is %q, want %q", snapshot.State, extenderStateServing)
	}
	if !slices.Equal(snapshot.DnsPorts, []int{connect.ExtenderDnsPort}) {
		t.Fatalf("dns ports are %v, want [%d]", snapshot.DnsPorts, connect.ExtenderDnsPort)
	}
	if 0 < len(snapshot.ListenErrors[connect.ExtenderCarrierDns]) {
		t.Fatalf("dns carrier is marked failed though 4053 serves: %v", snapshot.ListenErrors)
	}

	closeExtenderRole(t, role)
}

func TestExtenderRoleMetricsOffAndRelayedBytes(t *testing.T) {
	off := renderExtenderRoleMetrics(extenderRoleSnapshot{})
	for _, want := range []string{
		"urnet_extender_enabled 0",
		`urnet_extender_state{state="off"} 1`,
		`urnet_extender_carrier_listening{carrier="tcp"} 0`,
		`urnet_extender_relay_bytes_total{dir="ingress"} 0`,
	} {
		if !strings.Contains(off, want) {
			t.Fatalf("off metrics are missing %q:\n%s", want, off)
		}
	}

	serving := renderExtenderRoleMetrics(extenderRoleSnapshot{
		Enabled:     true,
		State:       extenderStateServing,
		Carriers:    []string{connect.ExtenderCarrierTcp},
		Connections: 2,
		Stats: extender.ExtenderStats{
			IngressByteCount: 1234,
			EgressByteCount:  5678,
		},
		TcpRetries: 3,
	})
	for _, want := range []string{
		"urnet_extender_enabled 1",
		`urnet_extender_connections_active 2`,
		`urnet_extender_relay_bytes_total{dir="ingress"} 1234`,
		`urnet_extender_relay_bytes_total{dir="egress"} 5678`,
		"urnet_extender_tcp_retry_total 3",
	} {
		if !strings.Contains(serving, want) {
			t.Fatalf("serving metrics are missing %q:\n%s", want, serving)
		}
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
