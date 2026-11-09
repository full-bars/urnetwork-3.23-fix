package urnettools

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallProviderBinaryBoundingSet(t *testing.T) {
	for _, tc := range []struct {
		name, mask string
		wantSet    bool
	}{
		{"allowed", "00000000a80425fb", true},
		{"blocked", "00000000a80421fb", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src")
			dst := filepath.Join(dir, "dst")
			status := filepath.Join(dir, "status")
			if err := os.WriteFile(src, []byte("new"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, []byte("old"), 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(status, []byte("CapBnd:	"+tc.mask+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var set bool
			var warn bytes.Buffer
			run := func(name string, args ...string) ([]byte, error) {
				if name == "setcap" || name == "sudo" {
					set = true
					return nil, nil
				}
				old, _ := os.ReadFile(dst)
				if string(old) == "old" || set {
					return []byte(dst + " cap_net_bind_service=ep"), nil
				}
				return nil, nil
			}
			if err := installProviderBinaryWithRunnerAndStatus(src, dst, "", "linux", &warn, run, status); err != nil {
				t.Fatal(err)
			}
			if set != tc.wantSet {
				t.Fatalf("setcap called=%v want %v", set, tc.wantSet)
			}
			if !tc.wantSet && !strings.Contains(warn.String(), "bounding set") {
				t.Fatalf("missing bounding-set warning: %q", warn.String())
			}
		})
	}
}
