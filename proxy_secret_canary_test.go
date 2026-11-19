package connect

import (
	"strings"
	"testing"
	"time"
)

func TestSafeProxyDisplayAddress(t *testing.T) {
	for _, address := range []string{"example.com:1080", "198.51.100.7:1080", "[2001:db8::1]:1080", "direct"} {
		if safeProxyDisplayAddress(address) != address {
			t.Errorf("valid address was hidden: %s", address)
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
