package connect

import "testing"

func TestProxyInternalDohKeepsNamesForProxy(t *testing.T) {
	settings := DefaultClientStrategySettings()
	settings.ProxySettings = &ProxySettings{Network: "tcp", Address: "127.0.0.1:1"}
	settings.InternalDohDomains = []string{"space.example"}
	got, resolver := clientStrategySettingsWithInternalDoh(settings)
	if resolver != nil {
		resolver.Close()
		t.Fatal("proxied strategy installed a local resolver")
	}
	if got != settings {
		t.Fatal("proxied strategy replaced its settings")
	}
}
