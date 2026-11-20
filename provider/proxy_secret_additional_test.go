package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urnetwork/connect"
)

func TestProxySecretSlowRetryAndAdmissionLogs(t *testing.T) {
	const address = "socks5://canary-user-7731:canary-pass-9042@example.com:1080?token=canary-token-5518"
	secrets := []string{"canary-user-7731", "canary-pass-9042", "canary-token-5518"}
	state := newProxySlowRetryState()
	state.Proxies[address] = &proxySlowRetryEntry{}
	out := captureTlog(t, func() {
		adopted, split := state.adoptLegacy([]*connect.ProxySettings{authSettings(address, "a", "p"), authSettings(address, "b", "p")})
		if adopted != 1 || split != 1 {
			t.Fatal("slow retry migration did not split")
		}
	})
	withTempHome(t)
	resetAdmissionStateCache()
	t.Cleanup(resetAdmissionStateCache)
	path, err := proxyURLStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	out += captureTlog(t, func() {
		if urlProxyPassesAdmission(context.Background(), address) {
			t.Fatal("invalid cache admitted proxy")
		}
	})
	if !strings.Contains(out, "DENYING") || !strings.Contains(out, "slow-retry state") {
		t.Fatal("expected logs were not emitted")
	}
	for _, secret := range secrets {
		if strings.Contains(out, secret) {
			t.Fatal("proxy log contains a canary")
		}
	}
}
