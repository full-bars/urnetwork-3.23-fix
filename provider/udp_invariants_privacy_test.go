package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
	"golang.org/x/net/proxy"
)

// A UDP check or refusal line names the proxy by slot and address, the way the
// rest of the provider does, and never prints the account or the password. The
// exit addresses stay: they are how an operator tells a working relay from one
// whose exits differ.
func TestUDPInvariantLogNamesTheProxyAddress(t *testing.T) {
	settings := &connect.ProxySettings{Address: "127.0.0.1:24111", Index: 42, Auth: &proxy.Auth{User: "fixture-user-908", Password: "fixture-pass-908"}}
	const label = "proxy[42] (127.0.0.1:24111)"
	for _, tc := range []struct {
		name  string
		emit  func()
		shown []string
	}{
		{
			name:  "check-ok",
			emit:  func() { logProxyUDPResult(settings, nil, proxyUDPResult{Status: proxyUDPOK, UDPExit: "192.0.2.2"}) },
			shown: []string{"192.0.2.2"},
		},
		{
			name: "check-mismatch",
			emit: func() {
				logProxyUDPResult(settings, nil, proxyUDPResult{Status: proxyUDPExitMismatch, UDPExit: "192.0.2.2", TCPExit: "192.0.2.3"})
			},
			shown: []string{"192.0.2.2", "192.0.2.3"},
		},
		{
			name: "check-unknown",
			emit: func() {
				logProxyUDPResult(settings, nil, proxyUDPResult{Status: proxyUDPUnknown, Err: errors.New(settings.Address)})
			},
		},
		{
			name: "check-no-relay",
			emit: func() {
				logProxyUDPResult(settings, nil, proxyUDPResult{Status: proxyUDPNoRelay, Err: errors.New(settings.Address + " " + settings.Auth.User + " " + settings.Auth.Password)})
			},
		},
		{
			name: "flow",
			emit: func() {
				stats := &proxiedUdpRefusalStats{keyRefusals: map[proxiedUdpRefusalKey]*proxiedUdpRefusal{}}
				stats.note(settings, proxiedUdpRefusalNoRelay)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureTlog(t, tc.emit)
			for _, want := range append([]string{label}, tc.shown...) {
				if !strings.Contains(out, want) {
					t.Errorf("log lacks %q: %q", want, out)
				}
			}
			for _, secret := range []string{settings.Auth.User, settings.Auth.Password} {
				if strings.Contains(out, secret) {
					t.Errorf("log contains the account credential %q: %q", secret, out)
				}
			}
		})
	}
}

// The label is the slot and the address only: a key built with an account does
// not put the account in it.
func TestUDPProxyIdentityShowsTheAddress(t *testing.T) {
	settings := &connect.ProxySettings{
		Address: "203.0.113.10:1080",
		Index:   7,
		Auth:    &proxy.Auth{User: "sentinel-user", Password: "sentinel-pass"},
	}
	got := udpProxyIdentity(settings)
	if want := "proxy[7] (203.0.113.10:1080)"; got != want {
		t.Fatalf("udpProxyIdentity = %q, want %q", got, want)
	}
	for _, leaked := range []string{"sentinel-user", "sentinel-pass"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("the label leaked %q: %q", leaked, got)
		}
	}
}
