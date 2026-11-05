package urnettools

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallProviderBinaryExtenderCapability(t *testing.T) {
	for _, tc := range []struct {
		name        string
		present     bool
		applyFails  bool
		wantSetcap  int
		wantWarning bool
	}{
		{name: "missing", wantSetcap: 1},
		{name: "present", present: true},
		{name: "apply failure", applyFails: true, wantSetcap: 1, wantWarning: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "new")
			dst := filepath.Join(dir, "provider binary's path")
			if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
				t.Fatal(err)
			}
			present, sets, checks := tc.present, 0, 0
			run := func(name string, args ...string) ([]byte, error) {
				data, err := os.ReadFile(dst)
				if err != nil || string(data) != "new" {
					t.Fatalf("%s before capability check: %q %v", name, data, err)
				}
				switch name {
				case "getcap":
					checks++
					if len(args) != 1 || args[0] != dst {
						t.Fatalf("getcap args: %q", args)
					}
					if present {
						return []byte(dst + " cap_net_bind_service=ep"), nil
					}
					return nil, nil
				case "setcap", "sudo":
					sets++
					want := []string{"cap_net_bind_service=+ep", dst}
					if name == "sudo" {
						want = []string{"-n", "setcap", "cap_net_bind_service=+ep", dst}
					}
					if strings.Join(args, "\x00") != strings.Join(want, "\x00") {
						t.Fatalf("setcap invocation: %s %q", name, args)
					}
					if tc.applyFails {
						return nil, fmt.Errorf("permission denied")
					}
					present = true
					return nil, nil
				default:
					t.Fatalf("unexpected command %s", name)
					return nil, nil
				}
			}
			var warnings bytes.Buffer
			if err := installProviderBinaryWithRunner(src, dst, "", "linux", &warnings, run); err != nil {
				t.Fatalf("install failed: %v", err)
			}
			if checks == 0 || sets != tc.wantSetcap {
				t.Fatalf("getcap=%d setcap=%d", checks, sets)
			}
			if tc.wantSetcap == 1 && !tc.applyFails && checks != 2 {
				t.Fatalf("successful apply not verified: %d checks", checks)
			}
			if got := warnings.String(); tc.wantWarning != strings.Contains(got, "WARNING:") {
				t.Fatalf("warning=%q", got)
			}
			if tc.wantWarning {
				quoted := "'" + strings.ReplaceAll(dst, "'", "'\\''") + "'"
				want := "WARNING: extender capability missing or unverified for " + fmt.Sprintf("%q", dst) + "; run: sudo setcap cap_net_bind_service=+ep " + quoted + "\n"
				if warnings.String() != want {
					t.Fatalf("warning=%q, want %q", warnings.String(), want)
				}
			}
		})
	}
}

func TestInstallProviderBinaryCapabilityNonLinux(t *testing.T) {
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "new"), filepath.Join(dir, "provider")
	if err := os.WriteFile(src, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(string, ...string) ([]byte, error) {
		t.Fatal("non-Linux capability check ran a command")
		return nil, nil
	}
	if err := installProviderBinaryWithRunner(src, dst, "", "darwin", &bytes.Buffer{}, run); err != nil {
		t.Fatal(err)
	}
}
