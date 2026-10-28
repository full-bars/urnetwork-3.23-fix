package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRedactProxySource(t *testing.T) {
	cases := []struct{ input, want string }{
		{"alice:secret@proxy:bad", "proxy:bad (user=unparsed, password=unparsed)"},
		{"socks5://alice:secret@proxy:bad", "socks5://proxy:bad (user=unparsed, password=unparsed)"},
		{"alice:secret@proxy:1080?key=a@TOKEN", "proxy:1080 (user=unparsed, password=unparsed)"},
		{"alice:secret@[2001:db8::1]:bad", "[2001:db8::1]:bad (user=unparsed, password=unparsed)"},
		{"[2001:db8::1]:1080:alice:secret", "[2001:db8::1]:1080 (user=present, password=present)"},
		{"proxy:1080:alice:secret", "proxy:1080 (user=present, password=present)"},
		{"proxy:1080", "proxy:1080 (user=absent, password=absent)"},
		{"alice:secret@", "<redacted> (user=unparsed, password=unparsed)"},
		{"alice:secret@x://proxy:1080", "proxy:1080 (user=absent, password=absent)"},
	}
	for _, tc := range cases {
		if got := redactProxySource(tc.input); got != tc.want {
			t.Errorf("redactProxySource(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestProxyFileCredentialLogRedaction(t *testing.T) {
	cases := []struct{ input, diagnostic string }{
		{"alice:secret@proxy:bad", "proxy:bad (user=unparsed, password=unparsed)"},
		{"https://user:pass@api.example.com/lists/TOKEN?key=TOKEN", "https://api.example.com (user=unparsed, password=unparsed)"},
	}
	for _, tc := range cases {
		t.Run(tc.diagnostic, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "proxies.txt")
			if err := os.WriteFile(path, []byte(tc.input+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			out := captureTlog(t, func() {
				_, err := readProxySettingsFromFile(path)
				if err != nil {
					t.Error(err)
				}
			})
			if !strings.Contains(out, "proxy "+tc.diagnostic+" missing credentials") || strings.Contains(out, tc.input) || strings.Contains(out, "TOKEN") || strings.Contains(out, "secret") || strings.Contains(out, "user:pass@") {
				t.Errorf("unexpected log: %q", out)
			}
		})
	}
}
