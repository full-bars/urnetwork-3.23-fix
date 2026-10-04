package connect

import (
	"strings"
	"testing"
)

// TestStunProbeSuffixRendering verifies the compact per-provider x per-family
// breakdown is rendered in stable order regardless of map iteration order.
func TestStunProbeSuffixRendering(t *testing.T) {
	p := stunProbe
	p.mu.Lock()
	p.result = &stunProbeResult{
		Providers: map[string][2]stunFamilyResult{
			"stunprotocol": {stunFamFail, stunFamFail},
			"meteredca":    {stunFamOK, stunFamFail},
			"google":       {stunFamOK, stunFamOK},
		},
	}
	p.mu.Unlock()

	s := p.suffix()
	// Order before pipes is alphabetical by provider key.
	if !strings.Contains(s, "google: v4=ok v6=ok") {
		t.Errorf("suffix missing google OK: %q", s)
	}
	if !strings.Contains(s, "meteredca: v4=ok v6=fail") {
		t.Errorf("suffix missing meteredca: %q", s)
	}
	if !strings.Contains(s, "stunprotocol: v4=fail v6=fail") {
		t.Errorf("suffix missing stunprotocol: %q", s)
	}
	g := strings.Index(s, "google:")

	mi := strings.Index(s, "meteredca:")
	if !(g < mi) {
		t.Errorf("suffix not alphabetical: %q", s)
	}
	if s == "" || !strings.HasPrefix(s, " | ") {
		t.Errorf("suffix should start with ' | ': %q", s)
	}

	// No probe yet -> empty (unit-test default).
	p.mu.Lock()
	p.result = nil
	p.mu.Unlock()
	if got := p.suffix(); got != "" {
		t.Errorf("nil result should yield empty suffix, got %q", got)
	}
}

func TestStunProviderOf(t *testing.T) {
	cases := map[string]string{
		"stun.l.google.com":     "google",
		"stun4.l.google.com":    "google",
		"openrelay.metered.ca":  "meteredca",
		"stun.stunprotocol.org": "stunprotocol",
		"stun.cloudflare.com":   "cloudflare",
		"whatever.example.com":  "whatever",
		"single":                "single",
	}
	for host, want := range cases {
		if got := stunProviderOf(host); got != want {
			t.Errorf("stunProviderOf(%q)=%q, want %q", host, got, want)
		}
	}
}

func TestStunEndpoint(t *testing.T) {
	cases := []struct{ raw, host, port string }{
		{"stun:openrelay.metered.ca:80", "openrelay.metered.ca", "80"},
		{"stun://stun.l.google.com:19302", "stun.l.google.com", "19302"},
		{"stun:stun.cloudflare.com", "stun.cloudflare.com", "3478"},
		{"", "", ""},
	}
	for _, c := range cases {
		h, p := stunEndpoint(c.raw)
		if h != c.host || p != c.port {
			t.Errorf("stunEndpoint(%q)=(%q,%q), want (%q,%q)", c.raw, h, p, c.host, c.port)
		}
	}
}

// TestHasXorMappedAddress builds a synthetic Binding Success response with a
// XOR-MAPPED-ADDRESS attribute and confirms detection, and confirms a response
// lacking that attribute is not treated as success-signal-bearing.
func TestHasXorMappedAddress(t *testing.T) {
	// Minimal success response lacking XOR-MAPPED-ADDRESS (only identifiable by
	// message type at the caller, so no attribute here -> false).
	{
		msg := make([]byte, stunHeaderSize+4+4)
		putU16(msg[0:2], stunMsgBindingResp)
		putU16(msg[2:4], 8) // two 4-byte attributes
		putU32(msg[4:8], stunMagicCookie)
		// attribute 1: unknown type 0x0001 (RESPONSE-ADDRESS), length 0
		putU16(msg[20:22], 0x0001)
		putU16(msg[22:24], 0)
		// attribute 2: SOFTWARE (0x8022), length 0
		putU16(msg[24:26], 0x8022)
		putU16(msg[26:28], 0)
		if hasXorMappedAddress(msg) {
			t.Errorf("expected no XOR-MAPPED-ADDRESS in synthetic response")
		}
	}
	// Response with XOR-MAPPED-ADDRESS (0x0020) -> true.
	{
		msg := make([]byte, stunHeaderSize+4+4)
		putU16(msg[0:2], stunMsgBindingResp)
		putU16(msg[2:4], 8)
		putU32(msg[4:8], stunMagicCookie)
		putU16(msg[20:22], stunAttrXorMapped)
		putU16(msg[22:24], 8)
		putU16(msg[24:26], 0x8022)
		putU16(msg[26:28], 0)
		if !hasXorMappedAddress(msg) {
			t.Errorf("expected XOR-MAPPED-ADDRESS detected")
		}
	}
}

func TestAggregateFamily(t *testing.T) {
	// Precedence ok > fail > n/a.
	if got := aggregateFamily(stunFamNA, stunFamOK); got != stunFamOK {
		t.Errorf("na+ok -> want ok, got %v", got)
	}
	if got := aggregateFamily(stunFamOK, stunFamFail); got != stunFamOK {
		t.Errorf("ok+fail -> want ok, got %v", got)
	}
	if got := aggregateFamily(stunFamFail, stunFamNA); got != stunFamFail {
		t.Errorf("fail+na -> want fail, got %v", got)
	}
	if got := aggregateFamily(stunFamNA, stunFamNA); got != stunFamNA {
		t.Errorf("na+na -> want na, got %v", got)
	}
}
