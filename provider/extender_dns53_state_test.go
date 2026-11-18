package main

import (
	"context"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/urnetwork/connect"
	"github.com/urnetwork/connect/extender"
)

// Port 53 fails to bind while 4053 serves. The carrier-level failure is erased
// by the serving port, so the per-port reason is what tells the operator why 53
// is down.
func TestExtenderRoleReportsWhyDns53IsDown(t *testing.T) {
	for _, tc := range []struct {
		name       string
		bindErr    error
		wantReason string
	}{
		{"address in use", &net.OpError{Op: "listen", Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}, extender.BindFailureAddressInUse},
		{"permission denied", &net.OpError{Op: "listen", Err: os.NewSyscallError("bind", syscall.EACCES)}, extender.BindFailurePermissionDenied},
		{"other failure", &net.OpError{Op: "listen", Err: os.NewSyscallError("bind", syscall.ENETDOWN)}, extender.BindFailureUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := testExtenderRoleConfig(t)
			config.dnsPrivilegedPort = true
			config.listenPacket = func(network string, address string) (net.PacketConn, error) {
				if address == ":53" {
					return nil, tc.bindErr
				}
				return net.ListenPacket("udp", "127.0.0.1:0")
			}
			role, err := startExtenderRole(context.Background(), config)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			waitExtenderRoleReady(t, role)
			t.Cleanup(func() { globalExtenderRole.Store(nil) })
			defer closeExtenderRole(t, role)

			snapshot := role.snapshot()
			if got := snapshot.UdpPortReasons[connect.DefaultDnsPort]; got != tc.wantReason {
				t.Fatalf("reason for dns 53 = %q, want %q (all: %v)", got, tc.wantReason, snapshot.UdpPortReasons)
			}
			if got := dns53State(snapshot); got != tc.wantReason {
				t.Fatalf("dns53State = %q, want %q", got, tc.wantReason)
			}
			metrics := renderExtenderRoleMetrics(snapshot)
			want := `urnet_extender_dns53_state{state="` + tc.wantReason + `"} 1`
			if !strings.Contains(metrics, want) {
				t.Fatalf("metrics are missing %q:\n%s", want, metrics)
			}
			if strings.Count(metrics, "urnet_extender_dns53_state{") != len(dns53States) {
				t.Fatalf("the gauge must keep its fixed label set:\n%s", metrics)
			}
		})
	}
}

func TestExtenderRoleDns53BoundAndNotRequested(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested bool
		want      string
	}{
		{"requested and bound", true, "bound"},
		{"never requested", false, "not_requested"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := testExtenderRoleConfig(t)
			config.dnsPrivilegedPort = tc.requested
			role, err := startExtenderRole(context.Background(), config)
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			waitExtenderRoleReady(t, role)
			t.Cleanup(func() { globalExtenderRole.Store(nil) })
			defer closeExtenderRole(t, role)

			snapshot := role.snapshot()
			if len(snapshot.UdpPortReasons) != 0 {
				t.Fatalf("reasons for a role whose ports all bound: %v", snapshot.UdpPortReasons)
			}
			if got := dns53State(snapshot); got != tc.want {
				t.Fatalf("dns53State = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDns53StateWithoutAFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		snapshot extenderRoleSnapshot
		want     string
	}{
		"disabled wins":       {extenderRoleSnapshot{Dns53Disabled: true, DnsPorts: []int{connect.DefaultDnsPort}}, "disabled"},
		"not bound yet":       {extenderRoleSnapshot{Dns53Requested: true}, "pending"},
		"never asked":         {extenderRoleSnapshot{Carriers: []string{connect.ExtenderCarrierTcp}}, "not_requested"},
		"requested, bound":    {extenderRoleSnapshot{Dns53Requested: true, DnsPorts: []int{connect.DefaultDnsPort}}, "bound"},
		"4053 only is not 53": {extenderRoleSnapshot{Dns53Requested: true, Carriers: []string{connect.ExtenderCarrierDns}, DnsPorts: []int{connect.ExtenderDnsPort}}, "pending"},
	} {
		if got := dns53State(tc.snapshot); got != tc.want {
			t.Errorf("%s: dns53State = %q, want %q", name, got, tc.want)
		}
	}
}
