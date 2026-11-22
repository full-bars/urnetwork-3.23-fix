package urnettools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The two P2P source controls are kill switches. `off` must reach the provider
// as a value: a clear removes the key and hands it back to liveDefaults, where
// p2p_forged_source_drop is "on" and p2p_source_binding is "count", so the
// operator's off would turn the feature back on.
func TestP2PSourceKeysKeepOffAsAValue(t *testing.T) {
	for _, key := range []string{"p2p_source_binding", "p2p_forged_source_drop"} {
		if treatsOffAsClear(key) {
			t.Errorf("treatsOffAsClear(%q) = true; clearing the key restores the live default, not off", key)
		}
	}

	cases := []struct {
		key     string
		aliases []string
		values  []string
		bad     string
	}{
		{"p2p-source-binding", []string{"p2p-source-binding", "p2p_source_binding", "P2P-Source-Binding"}, []string{"off", "count", "drop", "Drop"}, "on"},
		{"p2p-forged-source-drop", []string{"p2p-forged-source-drop", "p2p_forged_source_drop", "P2P-Forged-Source-Drop"}, []string{"on", "off", "Off"}, "count"},
	}

	for _, tc := range cases {
		canonical := strings.ReplaceAll(tc.key, "-", "_")
		for _, alias := range tc.aliases {
			got, ok := canonicalControlKey(alias)
			if !ok || got != canonical {
				t.Errorf("canonicalControlKey(%q) = %q, %v; want %q", alias, got, ok, canonical)
			}
		}
		for _, value := range tc.values {
			if err := validateControlValue(canonical, value); err != nil {
				t.Errorf("%s=%q must be accepted: %v", canonical, value, err)
			}
		}
		if err := validateControlValue(canonical, tc.bad); err == nil {
			t.Errorf("%s=%q must be rejected", canonical, tc.bad)
		}
		helpLines := 0
		for _, line := range setKeyHelps {
			if strings.HasPrefix(strings.TrimSpace(line), tc.key+" ") {
				helpLines++
			}
		}
		if helpLines == 0 {
			t.Errorf("set help has no %s line", tc.key)
		}

		// Live set path: key table, restore delegate, control socket request
		// and reply, then the get that reads the value back.
		for _, value := range tc.values {
			t.Run(tc.key+"="+value, func(t *testing.T) {
				dir := t.TempDir()
				server := startMockControlServer(t, filepath.Join(dir, "provider.sock"))
				defer server.Close()
				p := Provider{StateDir: dir}

				if err := applySetOverride(p, tc.key, value, false); err != nil {
					t.Fatalf("set %s=%s: %v", tc.key, value, err)
				}

				server.mu.Lock()
				if len(server.requests) != 1 {
					server.mu.Unlock()
					t.Fatalf("socket saw %d requests, want 1", len(server.requests))
				}
				req := server.requests[0]
				server.mu.Unlock()
				if req.Cmd != "set" || req.Key != canonical || req.Value != value {
					t.Fatalf("socket request = %+v, want set %s=%s", req, canonical, value)
				}

				got, src, found, err := queryControlOverride(p, canonical)
				if err != nil {
					t.Fatalf("get %s: %v", canonical, err)
				}
				if !found || got != value || src != "socket" {
					t.Fatalf("get %s = (%q, %q, %v), want (%q, socket, true)", canonical, got, src, found, value)
				}
			})
		}

		// Provider not running: the same value must be queued as a set, so the
		// next start applies it instead of reverting to the default.
		for _, value := range tc.values {
			t.Run(tc.key+"="+value+"/queued", func(t *testing.T) {
				dir := t.TempDir()
				p := Provider{StateDir: dir}

				if err := applySetOverride(p, tc.key, value, false); err != nil {
					t.Fatalf("set %s=%s: %v", tc.key, value, err)
				}

				data, err := os.ReadFile(filepath.Join(dir, "pending_overrides.json"))
				if err != nil {
					t.Fatalf("read queued overrides: %v", err)
				}
				var ops []pendingOp
				if err := json.Unmarshal(data, &ops); err != nil {
					t.Fatalf("parse queued overrides: %v", err)
				}
				if len(ops) != 1 || ops[0].Op != "set" || ops[0].Key != canonical || ops[0].Value != value {
					t.Fatalf("queued ops = %+v, want one set %s=%s", ops, canonical, value)
				}

				got, src, found, err := queryControlOverride(p, canonical)
				if err != nil {
					t.Fatalf("get %s: %v", canonical, err)
				}
				if !found || got != value || src != "pending" {
					t.Fatalf("get %s = (%q, %q, %v), want (%q, pending, true)", canonical, got, src, found, value)
				}
			})
		}
	}
}
