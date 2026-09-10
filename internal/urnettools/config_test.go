package urnettools

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFormatRelativeTime(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name     string
		t        time.Time
		expected string
	}{
		{"future timestamp", now.Add(10 * time.Second), "0s ago"},
		{"10 seconds ago", now.Add(-10 * time.Second), "10s ago"},
		{"45 seconds ago", now.Add(-45 * time.Second), "45s ago"},
		{"60 seconds ago", now.Add(-60 * time.Second), "1m ago"},
		{"5 minutes ago", now.Add(-5 * time.Minute), "5m ago"},
		{"59 minutes ago", now.Add(-59 * time.Minute), "59m ago"},
		{"2 hours ago", now.Add(-2 * time.Hour), "2h ago"},
		{"23 hours ago", now.Add(-23 * time.Hour), "23h ago"},
		{"25 hours ago", now.Add(-25 * time.Hour), "1d ago"},
		{"48 hours ago", now.Add(-48 * time.Hour), "2d ago"},
		{"5 days ago", now.Add(-5 * 24 * time.Hour), "5d ago"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := formatRelativeTime(tc.t)
			if got != tc.expected {
				t.Errorf("formatRelativeTime(%v) = %q, want %q", tc.t, got, tc.expected)
			}
		})
	}
}

func startMockStatusServer(t *testing.T, sockPath string, resp controlResponse) func() {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	_ = os.Remove(sockPath)

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen mock socket: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				var req controlRequest
				if err := json.NewDecoder(c).Decode(&req); err != nil {
					return
				}
				_ = json.NewEncoder(c).Encode(resp)
			}(conn)
		}
	}()

	return func() {
		ln.Close()
		<-done
		_ = os.Remove(sockPath)
	}
}

func TestConfigCmd_TableOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sockPath := filepath.Join(home, ".urnetwork", "provider.sock")

	twoHoursAgo := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	mockResp := controlResponse{
		OK: true,
		Settings: map[string]SettingInfo{
			"gogc": {
				Value:  "50",
				Source: "socket",
				SetAt:  twoHoursAgo,
			},
			"profile": {
				Value:  "eco",
				Source: "env",
			},
			"fast_auth": {
				Value:  "on",
				Source: "default",
			},
		},
	}

	cleanup := startMockStatusServer(t, sockPath, mockResp)
	defer cleanup()

	var buf bytes.Buffer
	if err := runConfig(&buf, nil); err != nil {
		t.Fatalf("runConfig failed: %v", err)
	}

	out := buf.String()

	// Verify header
	if !strings.Contains(out, "Setting") || !strings.Contains(out, "Value") ||
		!strings.Contains(out, "Source") || !strings.Contains(out, "Since") {
		t.Errorf("missing expected headers in output:\n%s", out)
	}

	// Verify divider
	if !strings.Contains(out, "────────────────") {
		t.Errorf("missing divider in output:\n%s", out)
	}

	// Verify rows
	if !strings.Contains(out, "gogc") || !strings.Contains(out, "50") || !strings.Contains(out, "socket") || !strings.Contains(out, "2h ago") {
		t.Errorf("missing gogc row in output:\n%s", out)
	}
	if !strings.Contains(out, "profile") || !strings.Contains(out, "eco") || !strings.Contains(out, "env") {
		t.Errorf("missing profile row in output:\n%s", out)
	}
	if !strings.Contains(out, "fast_auth") || !strings.Contains(out, "on") || !strings.Contains(out, "default") {
		t.Errorf("missing fast_auth row in output:\n%s", out)
	}

	// Verify default (no set_at) shows em dash "—"
	if !strings.Contains(out, "—") {
		t.Errorf("expected em dash '—' for default settings without set_at:\n%s", out)
	}

	// Verify alphabetical ordering: fast_auth before gogc before profile
	idxFastAuth := strings.Index(out, "fast_auth")
	idxGogc := strings.Index(out, "gogc")
	idxProfile := strings.Index(out, "profile")
	if !(idxFastAuth < idxGogc && idxGogc < idxProfile) {
		t.Errorf("settings not sorted alphabetically: fast_auth=%d, gogc=%d, profile=%d", idxFastAuth, idxGogc, idxProfile)
	}

	// Verify summary line: "3 settings from 3 sources"
	if !strings.Contains(out, "3 settings from 3 sources") {
		t.Errorf("missing or incorrect summary line in output:\n%s", out)
	}
}

func TestConfigCmd_JSONOutput(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sockPath := filepath.Join(home, ".urnetwork", "provider.sock")

	twoHoursAgo := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	mockResp := controlResponse{
		OK: true,
		Settings: map[string]SettingInfo{
			"gogc": {
				Value:  "50",
				Source: "socket",
				SetAt:  twoHoursAgo,
			},
			"profile": {
				Value:  "eco",
				Source: "env",
			},
		},
	}

	cleanup := startMockStatusServer(t, sockPath, mockResp)
	defer cleanup()

	for _, flag := range []string{"--json", "-j", "--json=true"} {
		var buf bytes.Buffer
		if err := runConfig(&buf, []string{flag}); err != nil {
			t.Fatalf("runConfig(%s) failed: %v", flag, err)
		}

		var parsed struct {
			OK       bool                   `json:"ok"`
			Settings map[string]SettingInfo `json:"settings"`
		}
		if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
			t.Fatalf("unmarshal output from flag %s: %v\noutput: %s", flag, err, buf.String())
		}
		if !parsed.OK {
			t.Errorf("flag %s: parsed.OK = false, want true", flag)
		}
		if len(parsed.Settings) != 2 {
			t.Errorf("flag %s: len(Settings) = %d, want 2", flag, len(parsed.Settings))
		}
		if parsed.Settings["gogc"].Value != "50" || parsed.Settings["gogc"].Source != "socket" {
			t.Errorf("flag %s: unexpected gogc setting: %+v", flag, parsed.Settings["gogc"])
		}
	}
}

func TestConfigCmd_SocketUnavailable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	var buf bytes.Buffer
	err := runConfig(&buf, nil)
	if err == nil {
		t.Fatal("expected error when socket is unavailable, got nil")
	}
}

func TestConfigCmd_ProviderReturnedError(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sockPath := filepath.Join(home, ".urnetwork", "provider.sock")

	cleanup := startMockStatusServer(t, sockPath, controlResponse{
		OK:    false,
		Error: "internal error",
	})
	defer cleanup()

	var buf bytes.Buffer
	err := runConfig(&buf, nil)
	if err == nil {
		t.Fatal("expected error when provider returns ok=false, got nil")
	}
	if !strings.Contains(err.Error(), "internal error") {
		t.Errorf("expected error to contain 'internal error', got %v", err)
	}
}

func TestDialControlSocket(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sockPath := filepath.Join(home, ".urnetwork", "provider.sock")

	cleanup := startMockStatusServer(t, sockPath, controlResponse{
		OK: true,
		Settings: map[string]SettingInfo{
			"metrics": {Value: "on", Source: "socket"},
		},
	})
	defer cleanup()

	resp, err := dialControlSocket(controlRequest{Cmd: "status"})
	if err != nil {
		t.Fatalf("dialControlSocket: %v", err)
	}
	if !resp.OK {
		t.Errorf("resp.OK = false, want true")
	}
	if resp.Settings["metrics"].Value != "on" {
		t.Errorf("resp.Settings[metrics] = %v, want on", resp.Settings["metrics"])
	}
}
