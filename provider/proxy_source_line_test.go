package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docopt/docopt-go"
)

func TestProxySourceClassifier(t *testing.T) {
	for _, tc := range []struct {
		name, line, reason string
		valid              bool
	}{
		{"ipv4", "1.2.3.4:1080:user:pass", "", true},
		{"ipv6", "[2001:db8::1]:1080:user:pass", "", true},
		{"ipv6 no credentials", "[2001:db8::1]:1080", "invalid_fields", false},
		{"missing credentials", "host:1080:user:", "missing_credentials", false},
		{"extra", "host:1080:user:pass:extra", "invalid_fields", false},
		{"empty host", ":1080:user:pass", "invalid_host", false},
		{"port text", "host:abc:user:pass", "invalid_port", false},
		{"port range", "host:65536:user:pass", "invalid_port", false},
		{"port zero", "host:0:user:pass", "invalid_port", false},
		{"spaces", "host:1080:user:pass  ", "", true},
		{"crlf", "host:1080:user:pass\r", "", true},
		{"comment", " # ignored", "", false},
		{"blank", " \r", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := classifyProxySourceLine(tc.line)
			if (got != nil) != tc.valid || reason != tc.reason {
				t.Fatalf("valid=%v reason=%s", got != nil, reason)
			}
		})
	}
}

func TestProxySourceAddRejectsInvalid(t *testing.T) {
	home := withTempHome(t)
	if err := os.MkdirAll(filepath.Join(home, ".urnetwork"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "source")
	original := "first:1234:user:pass\n"
	if err := os.WriteFile(path, []byte(original), 0600); err != nil {
		t.Fatal(err)
	}
	proxyAddFileBacked(path, docopt.Opts{"<key_address>": []string{"host:abc:user:secret"}})
	data, err := os.ReadFile(path)
	if err != nil || string(data) != original {
		t.Fatal("invalid input changed source")
	}
}

func TestProxySourceLoaderSweepParityAndRedaction(t *testing.T) {
	home := withTempHome(t)
	path := filepath.Join(home, "source")
	data := "first:1234:user:pass\nleaked-secret:invalid:user:secret\nlast:5678:user:pass\n"
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	out := captureOutput(t, func() {
		loaded, err := readProxySettingsFromFile(path)
		if err != nil {
			t.Fatal(err)
		}
		paid, trusted := udpPaidDesiredSet(&ProxyState{Source: path})
		if !trusted || len(loaded) != 2 || len(paid) != 2 {
			t.Fatalf("loaded=%d paid=%d trusted=%v", len(loaded), len(paid), trusted)
		}
		for _, p := range loaded {
			if paid[p.Key()] == nil {
				t.Fatal("loader and sweep disagree")
			}
		}
	})
	if !strings.Contains(out, "source line 2 rejected: invalid_port") || strings.Count(out, "source line 2 rejected") != 1 || strings.Contains(out, "leaked-secret") || strings.Contains(out, "user:secret") {
		t.Fatalf("unexpected output: %q", out)
	}
	if err := os.WriteFile(path, []byte(data+"# changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out = captureOutput(t, func() { _, _ = readProxySettingsFromFile(path) })
	if strings.Count(out, "source line 2 rejected: invalid_port") != 1 || strings.Contains(out, "secret") {
		t.Fatal("changed source must log only a redacted reason")
	}
}
