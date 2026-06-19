package main

import (
	"path/filepath"
	"testing"
)

func TestWriteReadProxyURLState_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy_url.json")

	s := &ProxyURLState{
		Sources: []string{"https://example.com/list.txt"},
		Cache: map[string]ProxyURLEntry{
			"1.2.3.4:1080": {},
			"5.6.7.8:1080": {User: "u", Password: "p"},
		},
	}

	if err := writeProxyURLStateTo(path, s); err != nil {
		t.Fatal(err)
	}

	got, err := readProxyURLStateFrom(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sources) != 1 || got.Sources[0] != "https://example.com/list.txt" {
		t.Errorf("sources: got %v", got.Sources)
	}
	if len(got.Cache) != 2 {
		t.Errorf("cache: got %d entries, want 2", len(got.Cache))
	}
	if got.Cache["5.6.7.8:1080"].User != "u" {
		t.Errorf("cache entry user: got %q, want %q", got.Cache["5.6.7.8:1080"].User, "u")
	}
}

func TestReadProxyURLState_NotExist(t *testing.T) {
	s, err := readProxyURLStateFrom("/tmp/does-not-exist-proxy_url.json")
	if err != nil {
		t.Fatal(err)
	}
	if s.Cache == nil {
		t.Fatal("expected non-nil Cache map")
	}
}

func TestParseProxyURLLine(t *testing.T) {
	tests := []struct {
		name         string
		line         string
		wantAddr     string
		wantUser     string
		wantPassword string
		wantOK       bool
	}{
		{"plain host:port", "1.2.3.4:1080", "1.2.3.4:1080", "", "", true},
		{"host:port:user:pass", "1.2.3.4:1080:myuser:mypass", "1.2.3.4:1080", "myuser", "mypass", true},
		{"socks5 no creds", "socks5://1.2.3.4:1080", "1.2.3.4:1080", "", "", true},
		{"socks5 with creds", "socks5://myuser:mypass@1.2.3.4:1080", "1.2.3.4:1080", "myuser", "mypass", true},
		{"SOCKS5 case-insensitive scheme", "SOCKS5://1.2.3.4:1080", "1.2.3.4:1080", "", "", true},
		{"blank line", "", "", "", "", false},
		{"comment line", "# 1.2.3.4:1080", "", "", "", false},
		{"unsupported scheme", "http://1.2.3.4:1080", "", "", "", false},
		{"whitespace padded", "  1.2.3.4:1080  ", "1.2.3.4:1080", "", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, user, password, ok := parseProxyURLLine(tt.line)
			if ok != tt.wantOK {
				t.Fatalf("ok: got %v, want %v", ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if addr != tt.wantAddr {
				t.Errorf("address: got %q, want %q", addr, tt.wantAddr)
			}
			if user != tt.wantUser {
				t.Errorf("user: got %q, want %q", user, tt.wantUser)
			}
			if password != tt.wantPassword {
				t.Errorf("password: got %q, want %q", password, tt.wantPassword)
			}
		})
	}
}
