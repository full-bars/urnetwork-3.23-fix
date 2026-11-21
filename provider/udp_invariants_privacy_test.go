package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

func TestUDPInvariantLogOmitsProxyIdentity(t *testing.T) {
	settings := &connect.ProxySettings{Address: "127.0.0.1:24111", Index: 42, Auth: &proxy.Auth{User: "fixture-user-908", Password: "fixture-pass-908"}}
	for _, tc := range []struct {
		name string
		emit func()
	}{
		{"check-ok", func() { logProxyUDPResult(settings, nil, proxyUDPResult{Status: proxyUDPOK, UDPExit: "192.0.2.2"}) }},
		{"check-mismatch", func() {
			logProxyUDPResult(settings, nil, proxyUDPResult{Status: proxyUDPExitMismatch, UDPExit: "192.0.2.2", TCPExit: "192.0.2.3"})
		}},
		{"check-unknown", func() {
			logProxyUDPResult(settings, nil, proxyUDPResult{Status: proxyUDPUnknown, Err: errors.New(settings.Address)})
		}},
		{"check", func() {
			logProxyUDPResult(settings, nil, proxyUDPResult{Status: proxyUDPNoRelay, Err: errors.New(settings.Address + " " + settings.Auth.User + " " + settings.Auth.Password)})
		}},
		{"flow", func() {
			stats := &proxiedUdpRefusalStats{keyRefusals: map[proxiedUdpRefusalKey]*proxiedUdpRefusal{}}
			stats.note(settings, proxiedUdpRefusalNoRelay)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureTlog(t, tc.emit)
			if !strings.Contains(out, udpProxyIdentity(settings)) {
				t.Error("log lacks proxy slot and stable identity")
			}
			for _, secret := range []string{settings.Address, settings.Auth.User, settings.Auth.Password, "192.0.2.2", "192.0.2.3"} {
				if strings.Contains(out, secret) {
					t.Error("log contains proxy identity or credentials")
				}
			}
		})
	}
}
