package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// tempHome sets HOME to a temp dir and returns a cleanup function.
func tempHome(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	oldHome := os.Getenv("HOME")
	os.Setenv("HOME", dir)
	return dir, func() {
		os.Setenv("HOME", oldHome)
	}
}

// disableFile creates or removes the disable_ip_autodetect marker.
func disableFile(t *testing.T, dir string, create bool) {
	t.Helper()
	path := filepath.Join(dir, ".urnetwork", "disable_ip_autodetect")
	if create {
		os.MkdirAll(filepath.Dir(path), 0o700)
		os.WriteFile(path, []byte("1\n"), 0o644)
	} else {
		os.Remove(path)
	}
}

func TestResolvePublicIP_EnvVarTakesPriority(t *testing.T) {
	_, cleanup := tempHome(t)
	defer cleanup()
	defer os.Unsetenv("URNETWORK_PUBLIC_IP")

	os.Setenv("URNETWORK_PUBLIC_IP", "203.0.113.5")
	got := resolvePublicIP()
	if got != "203.0.113.5" {
		t.Fatalf("expected 203.0.113.5, got %s", got)
	}
}

func TestResolvePublicIP_DisableFileSkipsAutodetect(t *testing.T) {
	dir, cleanup := tempHome(t)
	defer cleanup()
	defer os.Unsetenv("URNETWORK_PUBLIC_IP")

	os.Unsetenv("URNETWORK_PUBLIC_IP")
	disableFile(t, dir, true)

	// Should return empty even though network might work
	got := resolvePublicIP()
	if got != "" {
		t.Fatalf("expected empty string when autodetect disabled, got %s", got)
	}
}

func TestResolvePublicIP_DisableFileNotPresentAttemptsFetch(t *testing.T) {
	dir, cleanup := tempHome(t)
	defer cleanup()
	defer os.Unsetenv("URNETWORK_PUBLIC_IP")

	os.Unsetenv("URNETWORK_PUBLIC_IP")
	disableFile(t, dir, false) // file does not exist

	// Without network, this should return "" (the fetch fails)
	got := resolvePublicIP()
	if got != "" {
		// If network is available, we'd get a real IP — just verify it parses
		if ip := parseIPv4(got); ip == nil {
			t.Fatalf("expected valid IPv4 or empty, got %s", got)
		}
	}
}

func parseIPv4(s string) []byte {
	// Simple check: must be exactly 4 dot-separated octets, each 0-255
	parts := strings.Split(strings.TrimSpace(s), ".")
	if len(parts) != 4 {
		return nil
	}
	var b [4]byte
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || n > 255 {
			return nil
		}
		b[i] = byte(n)
	}
	return b[:]
}

func TestParseIPv4_Valid(t *testing.T) {
	cases := []string{
		"66.249.75.83",
		"192.0.2.1",
		"203.0.113.96",
		"10.0.0.1",
	}
	for _, c := range cases {
		if parseIPv4(c) == nil {
			t.Errorf("parseIPv4(%q) returned nil for valid IPv4", c)
		}
	}
}

func TestParseIPv4_Invalid(t *testing.T) {
	cases := []string{
		"not an ip",
		"",
		"256.1.1.1",
		"1.2.3",
		"::1",
		"2001:db8::1",
		"1.2.3.4.5",
		"999.999.999.999",
	}
	for _, c := range cases {
		if parseIPv4(c) != nil {
			t.Errorf("parseIPv4(%q) returned non-nil for invalid input", c)
		}
	}
}

func TestIPDetectionDisabled_DefaultFalse(t *testing.T) {
	dir, cleanup := tempHome(t)
	defer cleanup()
	disableFile(t, dir, false)
	if ipDetectionDisabled() {
		t.Fatal("expected false when disable file is missing")
	}
}

func TestIPDetectionDisabled_FilePresentTrue(t *testing.T) {
	dir, cleanup := tempHome(t)
	defer cleanup()
	disableFile(t, dir, true)
	if !ipDetectionDisabled() {
		t.Fatal("expected true when disable file exists")
	}
}
