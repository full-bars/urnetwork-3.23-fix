package connect

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

func TestProxySecretSettingsFormatting(t *testing.T) {
	s := ProxySettings{Index: 1, Network: "tcp", Address: "example.com:1080?token=canary-token-5518", Auth: &proxy.Auth{User: "canary-user-7731", Password: "canary-pass-9042"}}
	for _, value := range []any{s, &s} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			got := fmt.Sprintf(format, value)
			for _, secret := range []string{"canary-user-7731", "canary-pass-9042", "canary-token-5518"} {
				if strings.Contains(got, secret) {
					t.Errorf("format %s exposes a canary", format)
				}
			}
		}
	}
	s.Address = "example.com:1080"
	if !strings.Contains(fmt.Sprint(s), "example.com:1080") {
		t.Error("settings output hides a valid address")
	}
}

func TestSafeProxyDisplayAddress(t *testing.T) {
	for _, address := range []string{"example.com:1080", "198.51.100.7:1080", "[2001:db8::1]:1080", "direct"} {
		if safeProxyDisplayAddress(address) != address {
			t.Errorf("valid address was hidden: %s", address)
		}
	}
}

func TestProxySecretDialerString(t *testing.T) {
	settings := DefaultClientStrategySettings()
	settings.ProxySettings = &ProxySettings{Address: "socks5://canary-user-7731:canary-pass-9042@example.com:1080?token=canary-token-5518"}
	dialer := &clientDialer{settings: settings}
	got := dialer.String()
	for _, secret := range []string{"canary-user-7731", "canary-pass-9042", "canary-token-5518"} {
		if strings.Contains(got, secret) {
			t.Error("dialer output contains a canary")
		}
	}
	settings.ProxySettings.Address = "example.com:1080"
	if !strings.Contains(dialer.String(), "example.com:1080") {
		t.Error("dialer output hides a valid address")
	}
}

func TestProxySecretHealthErrors(t *testing.T) {
	for _, record := range []func(int, error){RecordProxyAuthFailure, RecordProxyTransportDrop} {
		RegisterProxy(991732, "example.com:1080", "example.com:1080")
		err := errors.New("dial socks5://canary-user-7731:canary-pass-9042@example.com:1080?token=canary-token-5518")
		record(991732, err)
		markProxyDown(991732)
		got := fmt.Sprint(ProxyHealthHeartbeat(false).Dead)
		UnregisterProxy(991732)
		for _, secret := range []string{"canary-user-7731", "canary-pass-9042", "canary-token-5518"} {
			if strings.Contains(got, secret) {
				t.Error("health error output contains a canary")
			}
		}
	}
}

func TestProxySecretHealthRegistry(t *testing.T) {
	address := "socks5://canary-user-7731:canary-pass-9042@example.com:1080?token=canary-token-5518"
	RegisterProxy(991731, address, address)
	defer UnregisterProxy(991731)
	got := formatProxyEntry(991731, address)
	for _, cell := range ProxyCells(time.Now()) {
		if cell.Index == 991731 {
			got += cell.Addr
		}
	}
	for _, secret := range []string{"canary-user-7731", "canary-pass-9042", "canary-token-5518"} {
		if strings.Contains(got, secret) {
			t.Error("health output contains a canary")
		}
	}
}
