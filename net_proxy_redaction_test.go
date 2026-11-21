package connect

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// A proxied dial that fails names the slot, never the endpoint: the dialer's
// own error carries the address and port of the proxy.
func TestProxyDialLogNamesTheSlotNotTheEndpoint(t *testing.T) {
	const sentinel = "sentinel-proxy.example.net:1080"
	log := &captureLogger{enabled: true}
	settings := &ConnectSettings{
		Log: log,
		ProxySettings: &ProxySettings{
			Index:   7,
			Network: "tcp",
			Address: sentinel,
		},
		DialContextSettings: &DialContextSettings{
			DialContext: func(context.Context, string, string) (net.Conn, error) {
				return nil, errors.New("socks5 connect tcp " + sentinel + ": connection refused")
			},
		},
	}
	if _, err := settings.DialContext(context.Background(), "tcp", "example.com:443"); err == nil {
		t.Fatal("the dial was expected to fail")
	}
	log.mu.Lock()
	text := strings.Join(append(append([]string{}, log.info...), log.verbose...), "")
	log.mu.Unlock()
	if strings.Contains(text, sentinel) || strings.Contains(text, "sentinel-proxy.example.net") {
		t.Fatalf("the proxy endpoint reached the log: %q", text)
	}
	if !strings.Contains(text, "proxy[7]") {
		t.Fatalf("the log does not name the proxy slot: %q", text)
	}
}

// The redactor names the slot and leaves every other address alone, so a target
// host in the same line survives.
func TestRedactProxyEndpoint(t *testing.T) {
	settings := &ProxySettings{Index: 3, Address: "10.9.8.7:3128"}
	got := RedactProxyEndpoint("dial 10.9.8.7:3128 -> example.com:443 failed at 10.9.8.7:3128", settings)
	if strings.Contains(got, "10.9.8.7") || strings.Contains(got, "3128") {
		t.Fatalf("the endpoint survived: %q", got)
	}
	if !strings.Contains(got, "proxy[3]") {
		t.Fatalf("the slot is missing: %q", got)
	}
	if !strings.Contains(got, "example.com:443") {
		t.Fatalf("the target address was redacted: %q", got)
	}
	if same := RedactProxyEndpoint("nothing to redact", nil); same != "nothing to redact" {
		t.Fatalf("a nil proxy changed the text: %q", same)
	}
}
