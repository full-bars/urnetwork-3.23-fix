package connect

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
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

// The H1 strategy's V(2) select line names the slot that answered, never where
// the proxy is: clientDialer.String() carries the endpoint for diagnostics, so
// the select line has to be built from the redacted form.
func TestProxyDialLogSelectLineNamesTheSlot(t *testing.T) {
	const sentinel = "198.51.100.7:1080"
	settings := &ClientStrategySettings{
		ProxySettings: &ProxySettings{Index: 5, Network: "tcp", Address: sentinel},
	}
	dialer := &clientDialer{settings: settings, description: "h1"}

	line := dialer.logText()
	if strings.Contains(line, sentinel) || strings.Contains(line, "198.51.100.7") {
		t.Fatalf("the select line carries the proxy endpoint: %q", line)
	}
	if !strings.Contains(line, "proxy[5]") {
		t.Fatalf("the select line does not name the proxy slot: %q", line)
	}
	if !strings.Contains(line, "success=") || !strings.Contains(line, "h1") {
		t.Fatalf("the select line lost the dialer's diagnostics: %q", line)
	}

	errLine := dialer.logErrText(errors.New("socks5 connect tcp " + sentinel + ": connection refused"))
	if strings.Contains(errLine, sentinel) || strings.Contains(errLine, "198.51.100.7") {
		t.Fatalf("the select error carries the proxy endpoint: %q", errLine)
	}
	if !strings.Contains(errLine, "proxy[5]") || !strings.Contains(errLine, "connection refused") {
		t.Fatalf("the select error lost the slot or the reason: %q", errLine)
	}
}

// A proxy configured by DNS name dials an address it resolved, and a SOCKS
// failure carries that numeric hop: redacting only the configured literal
// leaves the proxy's address in the log.
func TestRedactProxyEndpointResolvedHop(t *testing.T) {
	settings := &ProxySettings{Index: 4, Network: "tcp", Address: "proxy.example.net:1080"}
	got := RedactProxyEndpoint(
		"socks5 dial proxy.example.net:1080: dial tcp 203.0.113.9:1080: connect: connection refused",
		settings,
	)
	for _, leaked := range []string{"proxy.example.net", "203.0.113.9"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("the resolved proxy hop survived: %q in %q", leaked, got)
		}
	}
	if !strings.Contains(got, "proxy[4]") || !strings.Contains(got, "connection refused") {
		t.Fatalf("the line lost the slot or the reason: %q", got)
	}

	// the resolved hop can be v6, with brackets and the same port
	got = RedactProxyEndpoint("dial tcp [2001:db8::9]:1080: connect: connection refused", settings)
	if strings.Contains(got, "2001:db8::9") {
		t.Fatalf("the resolved v6 proxy hop survived: %q", got)
	}

	// a target address on another port is not the proxy and must survive
	got = RedactProxyEndpoint("proxy.example.net:1080 -> example.com:443, retry 198.51.100.20:443", settings)
	for _, kept := range []string{"example.com:443", "198.51.100.20:443"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("a target address was redacted: %q in %q", kept, got)
		}
	}

	// a proxy configured by numeric address is not a name: another host on the
	// same port cannot be told apart from it and must not be rewritten
	numeric := &ProxySettings{Index: 6, Network: "tcp", Address: "10.9.8.7:1080"}
	got = RedactProxyEndpoint("10.9.8.7:1080 -> 10.0.0.15:1080", numeric)
	if !strings.Contains(got, "10.0.0.15:1080") {
		t.Fatalf("an unrelated dial target was redacted: %q", got)
	}
	if strings.Contains(got, "10.9.8.7") {
		t.Fatalf("the configured proxy was not redacted: %q", got)
	}
}

// Only whole address tokens are the proxy: a plain substring replace turned a
// proxy at 10.0.0.1 into corruption elsewhere on the line.
func TestProxyRedactWholeTokens(t *testing.T) {
	settings := &ProxySettings{Index: 2, Network: "tcp", Address: "10.0.0.1:1080"}
	got := RedactProxyEndpoint("10.0.0.1:1080 -> 110.0.0.15:80 and 10.0.0.10:80", settings)
	if strings.Contains(got, "10.0.0.1:1080") {
		t.Fatalf("the proxy endpoint survived: %q", got)
	}
	for _, kept := range []string{"110.0.0.15:80", "10.0.0.10:80"} {
		if !strings.Contains(got, kept) {
			t.Fatalf("a neighbouring address was corrupted: %q in %q", kept, got)
		}
	}
	if !strings.Contains(got, "proxy[2]") {
		t.Fatalf("the slot is missing: %q", got)
	}

	// the bare host is a token too, and its longer neighbours are not
	got = RedactProxyEndpoint("dial 10.0.0.1 then 10.0.0.11 and 110.0.0.1", settings)
	if strings.Contains(got, "1proxy[2]") || strings.Contains(got, "proxy[2]1") || strings.Contains(got, "proxy[2]5") {
		t.Fatalf("a neighbouring address was corrupted: %q", got)
	}
	if !strings.Contains(got, "10.0.0.11") || !strings.Contains(got, "110.0.0.1") {
		t.Fatalf("a neighbouring address was redacted: %q", got)
	}
	if !strings.Contains(got, "proxy[2]") {
		t.Fatalf("the slot is missing: %q", got)
	}
}

// A v6 proxy is bracketed with its port, and a hostname proxy matches its name
// whatever the case, while a longer name that merely ends in it does not.
func TestProxyRedactBracketsAndNames(t *testing.T) {
	v6 := &ProxySettings{Index: 8, Network: "tcp", Address: "[2001:db8::1]:1080"}
	got := RedactProxyEndpoint("dial tcp [2001:db8::1]:1080: refused", v6)
	if strings.Contains(got, "[2001:db8::1]") {
		t.Fatalf("the v6 endpoint survived: %q", got)
	}
	if !strings.Contains(got, "proxy[8]") || !strings.Contains(got, "refused") {
		t.Fatalf("the line lost the slot or the reason: %q", got)
	}
	got = RedactProxyEndpoint("also [2001:db8::1]:8080 and 2001:db8::10", v6)
	if strings.Contains(got, "[2001:db8::1]:8080") {
		t.Fatalf("the v6 host on another port survived: %q", got)
	}
	if !strings.Contains(got, "2001:db8::10") {
		t.Fatalf("a neighbouring v6 address was redacted: %q", got)
	}

	name := &ProxySettings{Index: 9, Network: "tcp", Address: "Socks.Example.NET:1080"}
	got = RedactProxyEndpoint("socks5 dial socks.example.net:1080: refused", name)
	if strings.Contains(got, "socks.example.net") {
		t.Fatalf("the hostname endpoint survived: %q", got)
	}
	if !strings.Contains(got, "proxy[9]") {
		t.Fatalf("the slot is missing: %q", got)
	}
	got = RedactProxyEndpoint("notsocks.example.net:1080 other.example.net:1080", name)
	if !strings.Contains(got, "notsocks.example.net:1080") || !strings.Contains(got, "other.example.net:1080") {
		t.Fatalf("a neighbouring hostname was redacted: %q", got)
	}
}

// A wrapped url.Error and an errors.As chain are the shapes the transport hands
// the redactor, and an empty or nil proxy must not touch the text at all.
func TestProxyRedactWrappedErrorsAndEmptySettings(t *testing.T) {
	settings := &ProxySettings{Index: 5, Network: "tcp", Address: "10.0.0.1:1080"}
	wrapped := fmt.Errorf("h1 dial: %w", &url.Error{
		Op:  "Get",
		URL: "https://example.com",
		Err: errors.New("socks5 connect 10.0.0.1:1080: connection refused"),
	})
	var urlErr *url.Error
	if !errors.As(wrapped, &urlErr) {
		t.Fatal("the url.Error was not reachable")
	}
	for _, text := range []string{wrapped.Error(), urlErr.Error()} {
		got := RedactProxyEndpoint(text, settings)
		if strings.Contains(got, "10.0.0.1") {
			t.Fatalf("the endpoint survived a wrapped error: %q", got)
		}
		if !strings.Contains(got, "proxy[5]") || !strings.Contains(got, "connection refused") {
			t.Fatalf("the line lost the slot or the reason: %q", got)
		}
		if !strings.Contains(got, "example.com") {
			t.Fatalf("the target was redacted: %q", got)
		}
	}

	// a line about the proxy names its slot even when it does not name it
	if got := RedactProxyEndpoint("connection reset by peer", settings); !strings.Contains(got, "proxy[5]") {
		t.Fatalf("an unlabelled line about the proxy: %q", got)
	}

	// no proxy configured: the text passes through untouched
	for _, empty := range []*ProxySettings{nil, {}} {
		if got := RedactProxyEndpoint("plain text", empty); got != "plain text" {
			t.Fatalf("text changed with no proxy configured: %q", got)
		}
	}
}
