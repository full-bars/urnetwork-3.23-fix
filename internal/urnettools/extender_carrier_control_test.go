package urnettools

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExtenderCarrierControl(t *testing.T) {
	for _, port := range []string{"443", "4053", "53"} {
		t.Run(port, func(t *testing.T) {
			key, canonical := "extender-udp"+port, "extender_udp"+port
			for _, alias := range []string{key, canonical} {
				if got, ok := canonicalControlKey(alias); !ok || got != canonical {
					t.Fatalf("canonicalControlKey(%q) = %q, %v", alias, got, ok)
				}
			}
			for _, value := range []string{"on", "off", "ON", "OFF"} {
				if err := validateControlValue(canonical, value); err != nil {
					t.Fatal(err)
				}
			}
			for _, value := range []string{"", "invalid", "true", " off "} {
				if err := validateControlValue(canonical, value); err == nil {
					t.Fatalf("accepted %q", value)
				}
			}
			found := false
			for _, line := range setKeyHelps {
				found = found || strings.Contains(line, key)
			}
			if !found {
				t.Fatalf("missing help for %s", key)
			}
			dir := t.TempDir()
			p := Provider{StateDir: dir}
			for _, value := range []string{"on", "off"} {
				if err := applySetOverride(p, key, value, false); err != nil {
					t.Fatal(err)
				}
			}
			if err := applySetOverride(p, key, "invalid", false); err == nil {
				t.Fatal("invalid value accepted")
			}
			data, err := os.ReadFile(filepath.Join(dir, "pending_overrides.json"))
			if err != nil {
				t.Fatal(err)
			}
			var ops []pendingOp
			if err := json.Unmarshal(data, &ops); err != nil {
				t.Fatal(err)
			}
			if len(ops) != 2 || ops[0] != (pendingOp{Op: "set", Key: canonical, Value: "on"}) || ops[1] != (pendingOp{Op: "set", Key: canonical, Value: "off"}) {
				t.Fatalf("unexpected queue: %+v", ops)
			}
		})
	}
}

func TestExtenderCarrierControlLiveOff(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix socket test")
	}
	for _, port := range []string{"443", "4053", "53"} {
		t.Run(port, func(t *testing.T) {
			dir := t.TempDir()
			listener, err := net.Listen("unix", filepath.Join(dir, "provider.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			requests := make(chan controlRequest, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				var req controlRequest
				if json.NewDecoder(conn).Decode(&req) != nil {
					return
				}
				requests <- req
				_ = json.NewEncoder(conn).Encode(controlResponse{OK: true})
			}()
			if err := applySetOverride(Provider{StateDir: dir}, "extender-udp"+port, "off", false); err != nil {
				t.Fatal(err)
			}
			if got := <-requests; got.Cmd != "set" || got.Key != "extender_udp"+port || got.Value != "off" {
				t.Fatalf("request: %+v", got)
			}
		})
	}
}
